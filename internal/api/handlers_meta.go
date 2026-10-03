package api

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/parkerbrown98/gotalk-server/internal/database"
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
}

type Policies struct {
	TermsURL      *string `json:"terms_url"`
	PrivacyURL    *string `json:"privacy_url"`
	GuidelinesURL *string `json:"guidelines_url"`
	// Current lists the versions in effect, so clients can tell when to show changes.
	Current []PolicySummary `json:"current"`
}

type Limits struct {
	MessageLength          int `json:"message_length"`
	PostLength             int `json:"post_length"`
	TitleLength            int `json:"title_length"`
	GroupDMRecipients      int `json:"group_dm_recipients"`
	WebhooksPerPlace       int `json:"webhooks_per_place"`
	ApplicationsPerUser    int `json:"applications_per_user"`
	CommandsPerApplication int `json:"commands_per_application"`
	PersonalTokens         int `json:"personal_tokens"`
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
	Features         Features             `json:"features"`
	RateLimits       []ratelimit.TierInfo `json:"rate_limits"`
	Policies         Policies             `json:"policies"`
	Limits           Limits               `json:"limits"`
	Webhooks         WebhookInfo          `json:"webhooks"`
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
		Features: Features{
			Forums: true, Chat: true, Voice: s.Service.VoiceEnabled(), Search: "postgres",
			APITokens: true, Bots: true, Webhooks: true, Policies: true, Transparency: true,
		},
		RateLimits: limits,
		Policies:   policies,
		Limits: Limits{
			MessageLength: lim.MessageLength, PostLength: lim.PostLength, TitleLength: lim.TitleLength,
			GroupDMRecipients: lim.GroupDMRecipients, WebhooksPerPlace: lim.WebhooksPerPlace,
			ApplicationsPerUser: lim.ApplicationsPerUser, CommandsPerApplication: lim.CommandsPerApplication,
			PersonalTokens: lim.PersonalTokens,
		},
		Webhooks: WebhookInfo{
			Events: service.WebhookEvents(), SignatureHeader: service.HeaderSignature, MaxAttempts: service.MaxWebhookAttempts,
		},
		Stats: Stats{Users: stats.Users, Places: stats.Places},
	}, nil
}

type SetupCheck struct {
	Name   string `json:"name"`
	Status string `json:"status" enum:"ok,warning,error,skipped"`
	Detail string `json:"detail"`
}

type SetupStatus struct {
	SetupRequired bool         `json:"setup_required"`
	Checks        []SetupCheck `json:"checks"`
	Defaults      struct {
		InstanceName     string `json:"instance_name"`
		RegistrationMode string `json:"registration_mode"`
	} `json:"defaults"`
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
		handle(s, func(ctx context.Context, _ *struct{}) (*Body[SetupStatus], error) {
			required, err := s.Service.SetupRequired(ctx)
			if err != nil {
				return nil, err
			}
			out := SetupStatus{SetupRequired: required, Checks: s.preflightChecks(ctx)}
			out.Defaults.InstanceName = s.Config.Setup.InstanceName
			out.Defaults.RegistrationMode = s.Config.Setup.RegistrationMode
			return ok(out)
		}))

	huma.Register(s.api, withAuthRateLimit(withStatus(operation("complete-setup", http.MethodPost, "/setup",
		"Complete first-run setup and create the first administrator", tagInstance), http.StatusCreated)),
		handle(s, func(ctx context.Context, in *Body[SetupRequest]) (*Body[SetupResponse], error) {
			b := in.Body
			result, settings, err := s.Service.CompleteSetup(ctx, service.SetupInput{
				Token:               b.SetupToken,
				InstanceName:        b.Instance.Name,
				InstanceDescription: b.Instance.Description,
				RegistrationMode:    b.Instance.RegistrationMode,
				AdminUsername:       b.Admin.Username,
				AdminEmail:          b.Admin.Email,
				AdminPassword:       b.Admin.Password,
			}, clientFrom(ctx))
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

func (s *Server) preflightChecks(ctx context.Context) []SetupCheck {
	checks := []SetupCheck{}

	if status, err := database.Status(ctx, s.Service.Pool()); err != nil {
		checks = append(checks, SetupCheck{"database", "error", err.Error()})
	} else if status.Current < status.Latest {
		checks = append(checks, SetupCheck{"database", "error", "migrations are pending; run `gotalk migrate`"})
	} else {
		checks = append(checks, SetupCheck{"database", "ok", "connected; schema is up to date"})
	}

	if s.Redis == nil {
		checks = append(checks, SetupCheck{"redis", "skipped",
			"not configured; rate limits, presence and real-time events stay within this process (fine for a single server)"})
	} else if err := s.Redis.Ping(ctx).Err(); err != nil {
		checks = append(checks, SetupCheck{"redis", "error", err.Error()})
	} else {
		checks = append(checks, SetupCheck{"redis", "ok", "connected"})
	}

	if s.Config.Server.PublicURL == "" {
		checks = append(checks, SetupCheck{"public_url", "warning",
			"GOTALK_SERVER_PUBLIC_URL is not set; links will be derived from each request's host"})
	} else {
		checks = append(checks, SetupCheck{"public_url", "ok", s.Config.Server.PublicURL})
	}

	checks = append(checks, SetupCheck{"email", "skipped", "email delivery is not available yet"})
	checks = append(checks, s.voiceCheck(ctx))
	return checks
}

// voiceCheck reports whether the configured LiveKit server is reachable.
func (s *Server) voiceCheck(ctx context.Context) SetupCheck {
	lk := s.Service.VoiceBackend()
	if lk == nil {
		return SetupCheck{"voice", "skipped", "not configured; set GOTALK_VOICE_LIVEKIT_URL and API credentials to enable voice channels"}
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := lk.Ping(ctx); err != nil {
		return SetupCheck{"voice", "error", "LiveKit is unreachable or rejected the API key: " + err.Error()}
	}
	return SetupCheck{"voice", "ok", "connected to LiveKit at " + lk.URL()}
}

// handleHealthz is a liveness probe: it only reports that the process is serving.
func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleReadyz is a readiness probe. An instance awaiting setup is still ready, since the
// wizard must be reachable through the load balancer.
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
		status, code = "unavailable", http.StatusServiceUnavailable
	} else if required, err := s.Service.SetupRequired(ctx); err == nil && required {
		status = "awaiting_setup"
	}
	// Voice is optional: an unreachable LiveKit degrades the instance but does not take it
	// out of rotation.
	if lk := s.Service.VoiceBackend(); lk == nil {
		checks["voice"] = "not_configured"
	} else if err := lk.Ping(ctx); err != nil {
		checks["voice"] = "error: " + err.Error()
		if status == "ready" {
			status = "degraded"
		}
	} else {
		checks["voice"] = "ok"
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

func (s *Server) handleSetupPage(w http.ResponseWriter, r *http.Request) {
	required, err := s.Service.SetupRequired(r.Context())
	if err != nil {
		writeProblem(w, http.StatusServiceUnavailable, "the database is unavailable")
		return
	}
	if !required {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	web.ServePage(w, "setup.html")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
