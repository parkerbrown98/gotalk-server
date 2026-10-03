package service

import (
	"context"
	"crypto/subtle"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/parkerbrown98/gotalk-server/internal/apperr"
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

type SetupInput struct {
	Token               string
	InstanceName        string
	InstanceDescription string
	RegistrationMode    string
	AdminUsername       string
	AdminEmail          string
	AdminPassword       string
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
		result, err = s.createSession(ctx, q, user, client)
		return err
	})
	if err != nil {
		return nil, settings, err
	}
	s.setupDone.Store(true)
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
