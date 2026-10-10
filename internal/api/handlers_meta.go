package api

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/parkerbrown98/gotalk-server/internal/media"
	"github.com/parkerbrown98/gotalk-server/internal/permissions"
	"github.com/parkerbrown98/gotalk-server/internal/ratelimit"
	"github.com/parkerbrown98/gotalk-server/internal/service"
	"github.com/parkerbrown98/gotalk-server/internal/store"
	"github.com/parkerbrown98/gotalk-server/internal/web"
)

const tagInstance = "Instance"

type Software struct {
	Name       string `json:"name"`
	Version    string `json:"version"`
	Repository string `json:"repository"`
}

type APIInfo struct {
	Version           string   `json:"version"`
	SupportedVersions []string `json:"supported_versions"`
	MinVersion        string   `json:"min_version" doc:"Oldest API version this instance serves"`
	MaxVersion        string   `json:"max_version" doc:"Newest API version this instance serves"`
	VersionHeader     string   `json:"version_header" doc:"Clients may send their API version in this header; unsupported versions get 400"`
	BaseURL           string   `json:"base_url"`
	OpenAPIURL        string   `json:"openapi_url"`
	DocsURL           string   `json:"docs_url"`
	GatewayURL        string   `json:"gateway_url" doc:"WebSocket URL of the real-time gateway"`
	TokenScopes       []string `json:"token_scopes" doc:"Scopes personal access tokens can hold"`
}

type Features struct {
	Forums       bool   `json:"forums"`
	Chat         bool   `json:"chat"`
	Voice        bool   `json:"voice" doc:"Voice and video channels are available (a LiveKit server is configured)"`
	Search       string `json:"search" enum:"none,postgres,meilisearch"`
	APITokens    bool   `json:"api_tokens" doc:"Personal access tokens with scopes"`
	Bots         bool   `json:"bots" doc:"Applications with bot accounts and slash commands"`
	Webhooks     bool   `json:"webhooks" doc:"Places can send signed events to external URLs"`
	Policies     bool   `json:"policies" doc:"Versioned policy documents and consent records"`
	Transparency bool   `json:"transparency" doc:"Moderation statistics at /transparency"`
	Feed         bool   `json:"feed" doc:"Ranked topic feeds per place (/places/{place}/feed) and instance-wide (/feed)"`
	TopicVotes   bool   `json:"topic_votes" doc:"Topics can be voted up and down (places can turn it off)"`
	Email        bool   `json:"email" doc:"The instance can send email"`
	// PasswordReset and EmailVerification need email.
	PasswordReset     bool `json:"password_reset" doc:"POST /auth/password-reset emails a reset link"`
	EmailVerification bool `json:"email_verification" doc:"Accounts can verify their email address"`
	Uploads           bool `json:"uploads" doc:"Avatars, place icons and banners, and the instance icon can be uploaded"`
}

type FeedInfo struct {
	Sorts            []string `json:"sorts" doc:"Supported sort orders, default first"`
	Windows          []string `json:"windows" doc:"Time windows of the top and controversial sorts"`
	DefaultWindow    string   `json:"default_window"`
	PageSize         int      `json:"page_size" doc:"Default items per page"`
	MaxPageSize      int      `json:"max_page_size"`
	ReadBatch        int      `json:"read_batch" doc:"Most topic IDs one POST /feed/read accepts"`
	MarkAllReadLimit int      `json:"mark_all_read_limit" doc:"Most topics one mark-all-read call touches"`
	ExcerptLength    int      `json:"excerpt_length"`
}

type Policies struct {
	TermsURL      *string `json:"terms_url"`
	PrivacyURL    *string `json:"privacy_url"`
	GuidelinesURL *string `json:"guidelines_url"`
	// Current lists the versions in effect, so clients can tell when to show changes.
	Current []PolicySummary `json:"current"`
}

type Limits struct {
	MessageLength          int      `json:"message_length"`
	PostLength             int      `json:"post_length"`
	TitleLength            int      `json:"title_length"`
	GroupDMRecipients      int      `json:"group_dm_recipients"`
	WebhooksPerPlace       int      `json:"webhooks_per_place"`
	ApplicationsPerUser    int      `json:"applications_per_user"`
	CommandsPerApplication int      `json:"commands_per_application"`
	PersonalTokens         int      `json:"personal_tokens"`
	UploadSize             int64    `json:"upload_size" doc:"Largest accepted upload in bytes"`
	UploadTypes            []string `json:"upload_types" doc:"Accepted image types"`
	UploadMaxSide          int      `json:"upload_max_side" doc:"Largest accepted image width or height in pixels"`
}

type WebhookInfo struct {
	Events          []string `json:"events" doc:"Events webhooks can subscribe to"`
	SignatureHeader string   `json:"signature_header"`
	MaxAttempts     int      `json:"max_attempts"`
}

type Stats struct {
	Users  int64 `json:"users"`
	Places int64 `json:"places"`
}

type Instance struct {
	Name             string               `json:"name"`
	Description      string               `json:"description"`
	IconURL          *string              `json:"icon_url"`
	URL              string               `json:"url"`
	Software         Software             `json:"software"`
	API              APIInfo              `json:"api"`
	RegistrationMode string               `json:"registration_mode" enum:"open,invite_only,closed"`
	SetupRequired    bool                 `json:"setup_required"`
	Status           string               `json:"status" enum:"awaiting_setup,healthy,degraded" doc:"Overall state; degraded means some features (see degraded_features) do not work"`
	DegradedFeatures []string             `json:"degraded_features" doc:"Features that are unavailable or failing, e.g. email or storage"`
	Features         Features             `json:"features"`
	RateLimits       []ratelimit.TierInfo `json:"rate_limits"`
	Policies         Policies             `json:"policies"`
	Limits           Limits               `json:"limits"`
	Webhooks         WebhookInfo          `json:"webhooks"`
	Feed             FeedInfo             `json:"feed"`
	Stats            Stats                `json:"stats"`
}

func (s *Server) instanceInfo(ctx context.Context, settings store.InstanceSetting) (Instance, error) {
	stats, err := s.Service.InstanceStats(ctx)
	if err != nil {
		return Instance{}, err
	}
	docs, err := s.Service.CurrentPolicies(ctx)
	if err != nil {
		return Instance{}, err
	}
	base := baseURLFrom(ctx)
	apiBase := base + APIPrefix
	limits := s.Limiter.Tiers()
	if !s.Config.RateLimit.Enabled {
		limits = []ratelimit.TierInfo{}
	}
	policies := Policies{Current: mapSlice(docs, toPolicySummary)}
	for _, d := range docs {
		u := apiBase + "/policies/" + d.Kind
		switch d.Kind {
		case service.PolicyTerms:
			policies.TermsURL = &u
		case service.PolicyPrivacy:
			policies.PrivacyURL = &u
		case service.PolicyGuidelines:
			policies.GuidelinesURL = &u
		}
	}
	lim := service.InstanceLimits()
	state, degraded, err := s.Service.InstanceState(ctx)
	if err != nil {
		return Instance{}, err
	}
	mailOn := s.Service.MailEnabled()
	return Instance{
		Name:        settings.Name,
		Description: settings.Description,
		IconURL:     settings.IconUrl,
		URL:         base,
		Software: Software{
			Name:       "gotalk",
			Version:    s.Version,
			Repository: "https://github.com/parkerbrown98/gotalk-server",
		},
		API: APIInfo{
			Version:           APIVersion,
			SupportedVersions: SupportedAPIVersions,
			MinVersion:        SupportedAPIVersions[0],
			MaxVersion:        SupportedAPIVersions[len(SupportedAPIVersions)-1],
			VersionHeader:     APIVersionHeader,
			BaseURL:           apiBase,
			OpenAPIURL:        apiBase + "/openapi.json",
			DocsURL:           apiBase + "/docs",
			GatewayURL:        gatewayURL(base),
			TokenScopes:       service.Scopes,
		},
		RegistrationMode: settings.RegistrationMode,
		SetupRequired:    settings.SetupCompletedAt == nil,
		Status:           state,
		DegradedFeatures: degraded,
		Features: Features{
			Forums: true, Chat: true, Voice: s.Service.VoiceEnabled(), Search: "postgres",
			APITokens: true, Bots: true, Webhooks: true, Policies: true, Transparency: true,
			Feed: true, TopicVotes: true,
			Email: mailOn, PasswordReset: mailOn, EmailVerification: mailOn, Uploads: s.Service.StorageEnabled(),
		},
		RateLimits: limits,
		Policies:   policies,
		Limits: Limits{
			MessageLength: lim.MessageLength, PostLength: lim.PostLength, TitleLength: lim.TitleLength,
			GroupDMRecipients: lim.GroupDMRecipients, WebhooksPerPlace: lim.WebhooksPerPlace,
			ApplicationsPerUser: lim.ApplicationsPerUser, CommandsPerApplication: lim.CommandsPerApplication,
			PersonalTokens: lim.PersonalTokens,
			UploadSize:     s.Service.MaxUploadSize(), UploadTypes: media.ContentTypes(), UploadMaxSide: media.MaxSide,
		},
		Webhooks: WebhookInfo{
			Events: service.WebhookEvents(), SignatureHeader: service.HeaderSignature, MaxAttempts: service.MaxWebhookAttempts,
		},
		Feed: FeedInfo{
			Sorts: service.FeedSorts, Windows: service.FeedWindows, DefaultWindow: service.DefaultFeedWindow,
			PageSize: service.DefaultFeedPageSize, MaxPageSize: service.MaxFeedPageSize, ReadBatch: service.MaxFeedReadBatch,
			MarkAllReadLimit: service.MaxMarkAllRead, ExcerptLength: service.ExcerptLength,
		},
		Stats: Stats{Users: stats.Users, Places: stats.Places},
	}, nil
}

type SetupCheck struct {
	Name   string `json:"name"`
	Status string `json:"status" enum:"ok,warning,error,skipped"`
	Detail string `json:"detail"`
	Hint   string `json:"hint,omitempty" doc:"What to do about a warning or error"`
}

func toChecks(checks []service.Check) []SetupCheck {
	out := make([]SetupCheck, len(checks))
	for i, c := range checks {
		out[i] = SetupCheck{Name: c.Name, Status: c.Status, Detail: c.Detail, Hint: c.Hint}
	}
	return out
}

type SetupStatus struct {
	SetupRequired bool         `json:"setup_required"`
	Checks        []SetupCheck `json:"checks"`
	Defaults      struct {
		InstanceName     string `json:"instance_name"`
		RegistrationMode string `json:"registration_mode"`
	} `json:"defaults"`
	// Config is included when the request carries a valid Gotalk-Setup-Token header.
	Config *InstanceConfig `json:"config,omitempty" doc:"Current storage, email, voice and CORS settings (secrets redacted); only with a valid Gotalk-Setup-Token header"`
}

type SetupRequest struct {
	SetupToken string `json:"setup_token" minLength:"1" doc:"One-time token printed in the server logs on first boot"`
	Instance   struct {
		Name             string `json:"name" minLength:"1" maxLength:"100"`
		Description      string `json:"description,omitempty" maxLength:"1000"`
		RegistrationMode string `json:"registration_mode,omitempty" enum:"open,invite_only,closed" default:"open"`
	} `json:"instance"`
	Admin struct {
		Username string `json:"username" minLength:"3" maxLength:"32" pattern:"^[a-zA-Z0-9_.-]+$"`
		Email    string `json:"email" format:"email" maxLength:"254"`
		Password string `json:"password" minLength:"10" maxLength:"256"`
	} `json:"admin"`
	Settings   *service.ProviderSettings `json:"settings,omitempty" doc:"Optional storage, email, voice and CORS settings; omitted sections keep their config/defaults"`
	SkipChecks bool                      `json:"skip_checks,omitempty" doc:"Save settings even if their live checks fail"`
}

type SetupTestRequest struct {
	SetupToken  string                   `json:"setup_token" minLength:"1"`
	Settings    service.ProviderSettings `json:"settings"`
	TestEmailTo string                   `json:"test_email_to,omitempty" maxLength:"254" doc:"Also send a test email to this address"`
}

type ChecksResponse struct {
	Checks []SetupCheck `json:"checks"`
}

type SetupResponse struct {
	Instance Instance `json:"instance"`
	Tokens   Tokens   `json:"tokens"`
}

type InstanceUpdateRequest struct {
	Name             *string `json:"name,omitempty" minLength:"1" maxLength:"100"`
	Description      *string `json:"description,omitempty" maxLength:"1000"`
	IconURL          *string `json:"icon_url,omitempty" maxLength:"2048" doc:"Empty string clears the icon"`
	RegistrationMode *string `json:"registration_mode,omitempty" enum:"open,invite_only,closed"`
}

func (s *Server) registerMeta() {
	huma.Register(s.api, operation("get-instance", http.MethodGet, "/instance",
		"Describe this instance", tagInstance),
		handle(s, func(ctx context.Context, _ *struct{}) (*Body[Instance], error) {
			settings, err := s.Service.InstanceSettings(ctx)
			if err != nil {
				return nil, err
			}
			info, err := s.instanceInfo(ctx, settings)
			if err != nil {
				return nil, err
			}
			return ok(info)
		}))

	huma.Register(s.api, withAuth(operation("update-instance", http.MethodPatch, "/instance",
		"Update instance settings (instance admins only)", tagInstance)),
		handle(s, func(ctx context.Context, in *Body[InstanceUpdateRequest]) (*Body[Instance], error) {
			settings, err := s.Service.UpdateInstance(ctx, mustPrincipal(ctx), service.InstanceUpdate{
				Name:             in.Body.Name,
				Description:      in.Body.Description,
				IconURL:          in.Body.IconURL,
				RegistrationMode: in.Body.RegistrationMode,
			})
			if err != nil {
				return nil, err
			}
			info, err := s.instanceInfo(ctx, settings)
			if err != nil {
				return nil, err
			}
			return ok(info)
		}))

	huma.Register(s.api, operation("list-permissions", http.MethodGet, "/permissions",
		"List permission bits understood by this instance", tagInstance),
		handle(s, func(ctx context.Context, _ *struct{}) (*Body[[]permissions.Definition], error) {
			return ok(permissions.Definitions())
		}))

	huma.Register(s.api, operation("get-setup-status", http.MethodGet, "/setup/status",
		"Report first-run setup state and pre-flight checks", tagInstance),
		handle(s, func(ctx context.Context, in *SetupTokenHeader) (*Body[SetupStatus], error) {
			required, err := s.Service.SetupRequired(ctx)
			if err != nil {
				return nil, err
			}
			out := SetupStatus{SetupRequired: required, Checks: toChecks(s.Service.Preflight(ctx, s.redisPing()))}
			out.Defaults.InstanceName = s.Config.Setup.InstanceName
			out.Defaults.RegistrationMode = s.Config.Setup.RegistrationMode
			if required && in.Token != "" && s.Service.ValidSetupToken(ctx, in.Token) {
				cfg := s.instanceConfig()
				out.Config = &cfg
			}
			return ok(out)
		}))

	huma.Register(s.api, withAuthRateLimit(operation("test-setup-settings", http.MethodPost, "/setup/test",
		"Check storage, email, voice or CORS settings during setup without saving them", tagInstance)),
		handle(s, func(ctx context.Context, in *Body[SetupTestRequest]) (*Body[ChecksResponse], error) {
			checks, err := s.Service.TestSetupSettings(ctx, in.Body.SetupToken, in.Body.Settings, in.Body.TestEmailTo)
			if err != nil {
				return nil, err
			}
			return ok(ChecksResponse{Checks: toChecks(checks)})
		}))

	huma.Register(s.api, withAuthRateLimit(withStatus(operation("complete-setup", http.MethodPost, "/setup",
		"Complete first-run setup and create the first administrator", tagInstance), http.StatusCreated)),
		handle(s, func(ctx context.Context, in *Body[SetupRequest]) (*Body[SetupResponse], error) {
			b := in.Body
			input := service.SetupInput{
				Token:               b.SetupToken,
				InstanceName:        b.Instance.Name,
				InstanceDescription: b.Instance.Description,
				RegistrationMode:    b.Instance.RegistrationMode,
				AdminUsername:       b.Admin.Username,
				AdminEmail:          b.Admin.Email,
				AdminPassword:       b.Admin.Password,
				SkipChecks:          b.SkipChecks,
			}
			if b.Settings != nil {
				input.Settings = *b.Settings
			}
			result, settings, err := s.Service.CompleteSetup(ctx, input, clientFrom(ctx))
			if err != nil {
				return nil, err
			}
			info, err := s.instanceInfo(ctx, settings)
			if err != nil {
				return nil, err
			}
			return ok(SetupResponse{Instance: info, Tokens: toTokens(result)})
		}))
}

type SetupTokenHeader struct {
	Token string `header:"Gotalk-Setup-Token" doc:"The setup token; when valid, the response includes the current settings"`
}

func (s *Server) redisPing() func(context.Context) error {
	if s.Redis == nil {
		return nil
	}
	return func(ctx context.Context) error { return s.Redis.Ping(ctx).Err() }
}

// handleHealthz is a liveness probe: it only reports that the process is serving. The
// state field is informational (awaiting_setup, healthy or degraded).
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	body := map[string]string{"status": "ok"}
	if state, _, err := s.Service.InstanceState(r.Context()); err == nil {
		body["state"] = state
	}
	writeJSON(w, http.StatusOK, body)
}

// handleReadyz is a readiness probe. An instance awaiting setup is still ready, since the
// wizard must be reachable through the load balancer. Optional features that fail (voice,
// storage, email) degrade the instance without taking it out of rotation.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	checks := map[string]string{}
	healthy := true
	if err := s.Service.Pool().Ping(ctx); err != nil {
		checks["database"] = "error: " + err.Error()
		healthy = false
	} else {
		checks["database"] = "ok"
	}
	if s.Redis == nil {
		checks["redis"] = "not_configured"
	} else if err := s.Redis.Ping(ctx).Err(); err != nil {
		checks["redis"] = "error: " + err.Error()
		healthy = false
	} else {
		checks["redis"] = "ok"
	}

	status, code := "ready", http.StatusOK
	if !healthy {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "unavailable", "checks": checks})
		return
	}
	if required, err := s.Service.SetupRequired(ctx); err == nil && required {
		status = "awaiting_setup"
	}
	degrade := func() {
		if status == "ready" {
			status = "degraded"
		}
	}
	p := s.Service.Providers()
	if lk := p.Voice; lk == nil && p.VoiceErr == nil {
		checks["voice"] = "not_configured"
	} else if lk == nil {
		checks["voice"] = "error: " + p.VoiceErr.Error()
		degrade()
	} else if err := lk.Ping(ctx); err != nil {
		checks["voice"] = "error: " + err.Error()
		degrade()
	} else {
		checks["voice"] = "ok"
	}
	health := s.Service.ProviderHealth(ctx, true)
	for _, name := range []string{"storage", "email"} {
		c := health[name]
		switch {
		case c.Status == service.CheckOK:
			checks[name] = "ok"
		case name == "email" && p.Mail == nil && p.MailErr == nil:
			checks[name] = "not_configured"
			degrade()
		default:
			checks[name] = c.Status + ": " + c.Detail
			if c.Status != service.CheckSkipped {
				degrade()
			}
		}
	}
	writeJSON(w, code, map[string]any{"status": status, "checks": checks})
}
func (s *Server) handleWellKnown(w http.ResponseWriter, r *http.Request) {
	base := baseURLFrom(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{
		"software":          "gotalk",
		"version":           s.Version,
		"instance_url":      base,
		"api_base_url":      base + APIPrefix,
		"api_versions":      SupportedAPIVersions,
		"instance_info_url": base + APIPrefix + "/instance",
		"gateway_url":       gatewayURL(base),
	})
}

func (s *Server) handleLanding(w http.ResponseWriter, _ *http.Request) {
	web.ServePage(w, "index.html")
}

// handleSetupPage serves the wizard. Once setup is complete the same page lets
// administrators sign in and reconfigure storage, email, voice and CORS.
func (s *Server) handleSetupPage(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	web.ServePage(w, "setup.html")
}

func (s *Server) handlePage(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		web.ServePage(w, name)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
