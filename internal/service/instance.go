package service

import (
	"context"
	"crypto/subtle"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/parkerbrown98/gotalk-server/internal/apperr"
	"github.com/parkerbrown98/gotalk-server/internal/auth"
	"github.com/parkerbrown98/gotalk-server/internal/store"
)

// ErrSetupAlreadyCompleted is returned when setup runs against a configured instance.
var ErrSetupAlreadyCompleted = &apperr.Error{Kind: apperr.KindConflict, Message: "setup has already been completed"}

// SetupRequired reports whether first-run setup is still pending. Once completed the
// answer is cached in memory, so the steady-state cost is a single atomic load.
func (s *Service) SetupRequired(ctx context.Context) (bool, error) {
	if s.setupDone.Load() {
		return false, nil
	}
	settings, err := s.q.GetInstanceSettings(ctx)
	if err != nil {
		return true, err
	}
	if settings.SetupCompletedAt != nil {
		s.setupDone.Store(true)
		return false, nil
	}
	return true, nil
}

// SetupToken returns the one-time token required by the browser wizard, or "" once setup
// is complete.
func (s *Service) SetupToken(ctx context.Context) (string, error) {
	settings, err := s.q.GetInstanceSettings(ctx)
	if err != nil {
		return "", err
	}
	if settings.SetupCompletedAt != nil {
		return "", nil
	}
	if s.cfg.Setup.Token != "" {
		return s.cfg.Setup.Token, nil
	}
	if settings.SetupToken == nil {
		return "", nil
	}
	return *settings.SetupToken, nil
}

func (s *Service) checkSetupToken(settings store.InstanceSetting, provided string) bool {
	if provided == "" {
		return false
	}
	candidates := []string{s.cfg.Setup.Token}
	if settings.SetupToken != nil {
		candidates = append(candidates, *settings.SetupToken)
	}
	for _, c := range candidates {
		if c != "" && subtle.ConstantTimeCompare([]byte(c), []byte(provided)) == 1 {
			return true
		}
	}
	return false
}

// ValidSetupToken reports whether token unlocks the wizard of an instance awaiting setup.
func (s *Service) ValidSetupToken(ctx context.Context, token string) bool {
	settings, err := s.q.GetInstanceSettings(ctx)
	return err == nil && settings.SetupCompletedAt == nil && s.checkSetupToken(settings, token)
}

type SetupInput struct {
	Token               string
	InstanceName        string
	InstanceDescription string
	RegistrationMode    string
	AdminUsername       string
	AdminEmail          string
	AdminPassword       string
	// Settings optionally configures storage, email, voice and CORS from the wizard. Each
	// section is checked live before setup completes unless SkipChecks is set.
	Settings   ProviderSettings
	SkipChecks bool
}

// TestSetupSettings live-checks wizard settings before setup completes; it needs the
// setup token. testEmailTo, when set, also sends a test email.
func (s *Service) TestSetupSettings(ctx context.Context, token string, in ProviderSettings, testEmailTo string) ([]Check, error) {
	settings, err := s.q.GetInstanceSettings(ctx)
	if err != nil {
		return nil, err
	}
	if settings.SetupCompletedAt != nil {
		return nil, ErrSetupAlreadyCompleted
	}
	if !s.checkSetupToken(settings, token) {
		return nil, apperr.Forbidden("invalid setup token; copy the setup link printed in the server logs")
	}
	return s.testSettings(ctx, in, testEmailTo)
}

// CompleteSetup configures the instance and creates the first administrator. It is safe
// to call concurrently from several replicas: exactly one caller wins.
func (s *Service) CompleteSetup(ctx context.Context, in SetupInput, client ClientInfo) (*AuthResult, store.InstanceSetting, error) {
	return s.completeSetup(ctx, in, client, true)
}

// CompleteSetupHeadless is CompleteSetup for trusted callers (CLI, boot-time config) that
// do not need to present the setup token.
func (s *Service) CompleteSetupHeadless(ctx context.Context, in SetupInput) (store.InstanceSetting, error) {
	_, settings, err := s.completeSetup(ctx, in, ClientInfo{UserAgent: "gotalk-cli"}, false)
	return settings, err
}

func (s *Service) completeSetup(ctx context.Context, in SetupInput, client ClientInfo, requireToken bool) (*AuthResult, store.InstanceSetting, error) {
	settings, err := s.q.GetInstanceSettings(ctx)
	if err != nil {
		return nil, store.InstanceSetting{}, err
	}
	if settings.SetupCompletedAt != nil {
		return nil, settings, ErrSetupAlreadyCompleted
	}
	if requireToken && !s.checkSetupToken(settings, in.Token) {
		return nil, settings, apperr.Forbidden("invalid setup token; copy the setup link printed in the server logs")
	}

	in.InstanceName = strings.TrimSpace(in.InstanceName)
	if in.InstanceName == "" || len(in.InstanceName) > 100 {
		return nil, settings, apperr.Invalid("instance name must be 1-100 characters")
	}
	if len(in.InstanceDescription) > 1000 {
		return nil, settings, apperr.Invalid("instance description must be at most 1000 characters")
	}
	if in.RegistrationMode == "" {
		in.RegistrationMode = "open"
	}
	if err := errors.Join(
		validateRegistrationMode(in.RegistrationMode),
		validateUsername(in.AdminUsername),
		validateEmail(in.AdminEmail),
		validatePassword(in.AdminPassword),
	); err != nil {
		return nil, settings, firstAppErr(err)
	}
	cands, _, err := s.prepareAndCheck(ctx, in.Settings, in.SkipChecks)
	if err != nil {
		return nil, settings, err
	}

	var result *AuthResult
	err = s.tx(ctx, func(q *store.Queries) error {
		updated, err := q.CompleteSetup(ctx, store.CompleteSetupParams{
			Name:             in.InstanceName,
			Description:      in.InstanceDescription,
			RegistrationMode: in.RegistrationMode,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrSetupAlreadyCompleted
		}
		if err != nil {
			return err
		}
		settings = updated

		user, err := s.createUser(ctx, q, in.AdminUsername, in.AdminEmail, in.AdminPassword, true)
		if err != nil {
			return err
		}
		if len(cands) > 0 {
			if err := s.storeCandidates(ctx, q, cands, &user.ID); err != nil {
				return err
			}
		}
		result, err = s.createSession(ctx, q, user, client)
		return err
	})
	if err != nil {
		return nil, settings, err
	}
	s.setupDone.Store(true)
	if len(cands) > 0 {
		if err := s.ReloadProviders(ctx); err != nil {
			s.log.Error("applying settings from setup", "error", err)
		}
	}
	s.log.Info("instance setup completed", "instance", settings.Name, "admin", in.AdminUsername)
	return result, settings, nil
}

// firstAppErr unwraps errors.Join output so callers see a single typed error.
func firstAppErr(err error) error {
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, e := range joined.Unwrap() {
			if e != nil {
				return e
			}
		}
	}
	return err
}

func (s *Service) InstanceSettings(ctx context.Context) (store.InstanceSetting, error) {
	return s.q.GetInstanceSettings(ctx)
}

type InstanceStats struct {
	Users  int64
	Places int64
}

// Limits are fixed content and resource limits, published so clients can validate input
// before sending it.
type Limits struct {
	MessageLength          int
	PostLength             int
	TitleLength            int
	GroupDMRecipients      int
	WebhooksPerPlace       int
	ApplicationsPerUser    int
	CommandsPerApplication int
	PersonalTokens         int
}

func InstanceLimits() Limits {
	return Limits{
		MessageLength:          maxMessageLen,
		PostLength:             maxPostLen,
		TitleLength:            maxTitleLen,
		GroupDMRecipients:      maxGroupRecipients,
		WebhooksPerPlace:       MaxWebhooksPerPlace,
		ApplicationsPerUser:    MaxApplicationsPerUser,
		CommandsPerApplication: MaxCommandsPerApp,
		PersonalTokens:         MaxPersonalTokens,
	}
}

func (s *Service) InstanceStats(ctx context.Context) (InstanceStats, error) {
	users, err := s.q.CountUsers(ctx)
	if err != nil {
		return InstanceStats{}, err
	}
	places, err := s.q.CountPlaces(ctx)
	if err != nil {
		return InstanceStats{}, err
	}
	return InstanceStats{Users: users, Places: places}, nil
}

type InstanceUpdate struct {
	Name             *string
	Description      *string
	IconURL          *string
	RegistrationMode *string
}

func (s *Service) UpdateInstance(ctx context.Context, p *Principal, in InstanceUpdate) (store.InstanceSetting, error) {
	if !p.User.IsInstanceAdmin {
		return store.InstanceSetting{}, apperr.Forbidden("only instance administrators can change instance settings")
	}
	if in.Name != nil {
		trimmed := strings.TrimSpace(*in.Name)
		if trimmed == "" || len(trimmed) > 100 {
			return store.InstanceSetting{}, apperr.Invalid("instance name must be 1-100 characters")
		}
		in.Name = &trimmed
	}
	if in.RegistrationMode != nil {
		if err := validateRegistrationMode(*in.RegistrationMode); err != nil {
			return store.InstanceSetting{}, err
		}
	}
	if err := validateOptionalURL("icon_url", in.IconURL); err != nil {
		return store.InstanceSetting{}, err
	}
	return s.q.UpdateInstanceSettings(ctx, store.UpdateInstanceSettingsParams{
		Name:             in.Name,
		Description:      in.Description,
		IconUrl:          in.IconURL,
		RegistrationMode: in.RegistrationMode,
	})
}

// ReapplyInput carries what `gotalk setup --reset` re-applies to a configured instance.
// Nil instance fields are left unchanged; admin fields are optional.
type ReapplyInput struct {
	InstanceName        *string
	InstanceDescription *string
	RegistrationMode    *string
	AdminUsername       string
	AdminEmail          string
	AdminPassword       string
}

// ReapplyResult reports what ReapplySetup changed.
type ReapplyResult struct {
	Settings     store.InstanceSetting
	AdminCreated bool
	AdminReset   bool
}

// ReapplySetup re-applies setup values to an already configured instance, for recovery
// and infrastructure-as-code: instance fields are updated, and the administrator account
// is created, or (when it exists) promoted to administrator with its password reset and
// every session and personal access token revoked.
func (s *Service) ReapplySetup(ctx context.Context, in ReapplyInput) (ReapplyResult, error) {
	var res ReapplyResult
	if in.InstanceName != nil {
		trimmed := strings.TrimSpace(*in.InstanceName)
		if trimmed == "" || len(trimmed) > 100 {
			return res, apperr.Invalid("instance name must be 1-100 characters")
		}
		in.InstanceName = &trimmed
	}
	if in.RegistrationMode != nil {
		if err := validateRegistrationMode(*in.RegistrationMode); err != nil {
			return res, err
		}
	}
	if in.AdminUsername != "" {
		if err := validatePassword(in.AdminPassword); err != nil {
			return res, err
		}
	}
	err := s.tx(ctx, func(q *store.Queries) error {
		var err error
		res.Settings, err = q.UpdateInstanceSettings(ctx, store.UpdateInstanceSettingsParams{
			Name: in.InstanceName, Description: in.InstanceDescription, RegistrationMode: in.RegistrationMode,
		})
		if err != nil || in.AdminUsername == "" {
			return err
		}
		user, err := q.GetUserByUsername(ctx, in.AdminUsername)
		if errors.Is(err, pgx.ErrNoRows) {
			if err := errors.Join(validateUsername(in.AdminUsername), validateEmail(in.AdminEmail)); err != nil {
				return firstAppErr(err)
			}
			_, err = s.createUser(ctx, q, in.AdminUsername, in.AdminEmail, in.AdminPassword, true)
			res.AdminCreated = err == nil
			return err
		}
		if err != nil {
			return err
		}
		if user.IsBot {
			return apperr.Invalid("%q is a bot account", in.AdminUsername)
		}
		hash, err := auth.HashPassword(in.AdminPassword)
		if err != nil {
			return err
		}
		if err := q.UpdateUserPassword(ctx, store.UpdateUserPasswordParams{ID: user.ID, PasswordHash: hash}); err != nil {
			return err
		}
		if err := q.SetInstanceAdmin(ctx, user.ID); err != nil {
			return err
		}
		s.emitSessionsEnded(ctx, q, user.ID, nil, nil)
		if _, err := q.RevokePersonalTokens(ctx, user.ID); err != nil {
			return err
		}
		res.AdminReset = true
		return q.RevokeAllUserSessions(ctx, user.ID)
	})
	return res, err
}
