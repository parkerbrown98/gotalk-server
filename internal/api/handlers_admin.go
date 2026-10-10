package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5"

	"github.com/parkerbrown98/gotalk-server/internal/apperr"
	"github.com/parkerbrown98/gotalk-server/internal/mail"
	"github.com/parkerbrown98/gotalk-server/internal/service"
	"github.com/parkerbrown98/gotalk-server/internal/storage"
)

// ConfigSection is one settings section as administrators see it. Secrets are never
// returned; secrets_set lists the ones that hold a value.
type ConfigSection[T any] struct {
	Source     string     `json:"source" enum:"default,config,settings" doc:"default: built-in defaults; config: set by the config file or environment (read-only here); settings: saved through the wizard or this API"`
	Editable   bool       `json:"editable"`
	ConfigKey  string     `json:"config_key" doc:"Setting this key in the config file or environment makes the section read-only"`
	Settings   T          `json:"settings"`
	SecretsSet []string   `json:"secrets_set" doc:"Secret fields that hold a value"`
	UpdatedAt  *time.Time `json:"updated_at,omitempty"`
	UpdatedBy  *string    `json:"updated_by,omitempty" format:"uuid"`
	Error      string     `json:"error,omitempty" doc:"Why the section's provider could not be opened"`
}

type ConfigDrivers struct {
	Storage []string `json:"storage"`
	Mail    []string `json:"mail"`
}

// InstanceConfig is the pluggable provider configuration.
type InstanceConfig struct {
	Storage ConfigSection[storage.Settings]      `json:"storage"`
	Mail    ConfigSection[mail.Settings]         `json:"mail"`
	Voice   ConfigSection[service.VoiceSettings] `json:"voice"`
	CORS    ConfigSection[service.CORSSettings]  `json:"cors"`
	Drivers ConfigDrivers                        `json:"drivers" doc:"Available storage and mail drivers"`
}

func section[T any](v service.SectionView) ConfigSection[T] {
	out := ConfigSection[T]{
		Source: v.Source, Editable: v.Editable, ConfigKey: v.Key, SecretsSet: v.SecretsSet,
		UpdatedAt: v.UpdatedAt, UpdatedBy: idString(v.UpdatedBy), Error: v.Error,
	}
	out.Settings, _ = v.Settings.(T)
	return out
}

func (s *Server) instanceConfig() InstanceConfig {
	out := InstanceConfig{Drivers: ConfigDrivers{Storage: storage.Drivers(), Mail: mail.Drivers()}}
	for _, v := range s.Service.ConfigView() {
		switch v.Section {
		case service.SectionStorage:
			out.Storage = section[storage.Settings](v)
		case service.SectionMail:
			out.Mail = section[mail.Settings](v)
		case service.SectionVoice:
			out.Voice = section[service.VoiceSettings](v)
		case service.SectionCORS:
			out.CORS = section[service.CORSSettings](v)
		}
	}
	return out
}

type ConfigUpdateInput struct {
	Force bool `query:"force" doc:"Save even if a live check fails"`
	Body  service.ProviderSettings
}

type ConfigUpdateResponse struct {
	Config InstanceConfig `json:"config"`
	Checks []SetupCheck   `json:"checks"`
}

type ConfigTestRequest struct {
	Settings    service.ProviderSettings `json:"settings"`
	TestEmailTo string                   `json:"test_email_to,omitempty" maxLength:"254" doc:"Also send a test email to this address"`
}

type SectionPath struct {
	Section string `path:"section" enum:"storage,mail,voice,cors"`
}

func requireAdmin(ctx context.Context) error {
	if !mustPrincipal(ctx).User.IsInstanceAdmin {
		return apperr.Forbidden("only instance administrators can change instance settings")
	}
	return nil
}

func (s *Server) registerInstanceConfig() {
	huma.Register(s.api, withSessionOnly(withAuth(operation("get-instance-config", http.MethodGet, "/instance/config",
		"Show storage, email, voice and CORS settings and where they come from (instance admins only)", tagInstance))),
		handle(s, func(ctx context.Context, _ *struct{}) (*Body[InstanceConfig], error) {
			if err := requireAdmin(ctx); err != nil {
				return nil, err
			}
			return ok(s.instanceConfig())
		}))

	huma.Register(s.api, withSessionOnly(withAuth(operation("update-instance-config", http.MethodPatch, "/instance/config",
		"Check and save settings for the given sections, then apply them on every replica (instance admins only)", tagInstance))),
		handle(s, func(ctx context.Context, in *ConfigUpdateInput) (*Body[ConfigUpdateResponse], error) {
			checks, err := s.Service.SaveSettings(ctx, mustPrincipal(ctx), in.Body, in.Force)
			if err != nil {
				return nil, err
			}
			return ok(ConfigUpdateResponse{Config: s.instanceConfig(), Checks: toChecks(checks)})
		}))

	huma.Register(s.api, withSessionOnly(withStatus(withAuth(operation("reset-instance-config", http.MethodDelete, "/instance/config/{section}",
		"Forget saved settings for a section so config, environment and defaults apply again (instance admins only)", tagInstance)), http.StatusOK)),
		handle(s, func(ctx context.Context, in *SectionPath) (*Body[InstanceConfig], error) {
			if err := s.Service.ResetSettings(ctx, mustPrincipal(ctx), in.Section); err != nil {
				return nil, err
			}
			return ok(s.instanceConfig())
		}))

	huma.Register(s.api, withSessionOnly(withAuthRateLimit(withAuth(operation("test-instance-config", http.MethodPost, "/instance/config/test",
		"Check settings without saving them, optionally sending a test email (instance admins only)", tagInstance)))),
		handle(s, func(ctx context.Context, in *Body[ConfigTestRequest]) (*Body[ChecksResponse], error) {
			checks, err := s.Service.TestSettings(ctx, mustPrincipal(ctx), in.Body.Settings, in.Body.TestEmailTo)
			if err != nil {
				return nil, err
			}
			return ok(ChecksResponse{Checks: toChecks(checks)})
		}))

	huma.Register(s.api, withAuth(operation("get-instance-checks", http.MethodGet, "/instance/checks",
		"Run the pre-flight checks against the live configuration (instance admins only)", tagInstance)),
		handle(s, func(ctx context.Context, _ *struct{}) (*Body[ChecksResponse], error) {
			if err := requireAdmin(ctx); err != nil {
				return nil, err
			}
			return ok(ChecksResponse{Checks: toChecks(s.Service.Preflight(ctx, s.redisPing()))})
		}))
}

// ImageUpload is a raw image request body.
type ImageUpload struct {
	RawBody []byte `contentType:"image/*"`
}

type PlaceImageUpload struct {
	Place   string `path:"place" doc:"Place ID or slug"`
	RawBody []byte `contentType:"image/*"`
}

type PlaceImagePath struct {
	Place string `path:"place" doc:"Place ID or slug"`
}

func (s *Server) uploadOp(id, method, path, summary, tag string) huma.Operation {
	op := withAuth(operation(id, method, path, summary, tag))
	op.Description = "Send the image as the raw request body (PNG, JPEG, GIF or WebP). Metadata such as EXIF " +
		"location is removed. The limit is `limits.upload_size` in GET /instance."
	op.MaxBodyBytes = s.Config.Uploads.MaxSize
	op.Errors = append(op.Errors, http.StatusRequestEntityTooLarge, http.StatusServiceUnavailable)
	return withContentRateLimit(op)
}

func (s *Server) registerUploads() {
	huma.Register(s.api, s.uploadOp("upload-my-avatar", http.MethodPut, "/users/@me/avatar", "Upload an avatar image", tagUsers),
		handle(s, func(ctx context.Context, in *ImageUpload) (*Body[SelfUser], error) {
			user, err := s.Service.SetAvatar(ctx, mustPrincipal(ctx), in.RawBody, baseURLFrom(ctx))
			if err != nil {
				return nil, err
			}
			return ok(toSelfUser(user))
		}))

	huma.Register(s.api, withStatus(withAuth(operation("delete-my-avatar", http.MethodDelete, "/users/@me/avatar",
		"Remove the avatar", tagUsers)), http.StatusOK),
		handle(s, func(ctx context.Context, _ *struct{}) (*Body[SelfUser], error) {
			user, err := s.Service.ClearAvatar(ctx, mustPrincipal(ctx))
			if err != nil {
				return nil, err
			}
			return ok(toSelfUser(user))
		}))

	for _, kind := range []struct{ name, purpose string }{{"icon", service.UploadPlaceIcon}, {"banner", service.UploadPlaceBanner}} {
		huma.Register(s.api, s.uploadOp("upload-place-"+kind.name, http.MethodPut, "/places/{place}/"+kind.name,
			"Upload the place "+kind.name+" (MANAGE_PLACE)", tagPlaces),
			handle(s, func(ctx context.Context, in *PlaceImageUpload) (*Body[Place], error) {
				view, err := s.Service.SetPlaceImage(ctx, mustPrincipal(ctx), in.Place, kind.purpose, in.RawBody, baseURLFrom(ctx))
				if err != nil {
					return nil, err
				}
				return ok(toPlaceView(view))
			}))
		huma.Register(s.api, withStatus(withAuth(operation("delete-place-"+kind.name, http.MethodDelete, "/places/{place}/"+kind.name,
			"Remove the place "+kind.name+" (MANAGE_PLACE)", tagPlaces)), http.StatusOK),
			handle(s, func(ctx context.Context, in *PlaceImagePath) (*Body[Place], error) {
				view, err := s.Service.SetPlaceImage(ctx, mustPrincipal(ctx), in.Place, kind.purpose, nil, baseURLFrom(ctx))
				if err != nil {
					return nil, err
				}
				return ok(toPlaceView(view))
			}))
	}

	huma.Register(s.api, s.uploadOp("upload-instance-icon", http.MethodPut, "/instance/icon", "Upload the instance icon (instance admins only)", tagInstance),
		handle(s, func(ctx context.Context, in *ImageUpload) (*Body[Instance], error) {
			settings, err := s.Service.SetInstanceIcon(ctx, mustPrincipal(ctx), in.RawBody, baseURLFrom(ctx))
			if err != nil {
				return nil, err
			}
			info, err := s.instanceInfo(ctx, settings)
			if err != nil {
				return nil, err
			}
			return ok(info)
		}))

	huma.Register(s.api, withStatus(withAuth(operation("delete-instance-icon", http.MethodDelete, "/instance/icon",
		"Remove the instance icon (instance admins only)", tagInstance)), http.StatusOK),
		handle(s, func(ctx context.Context, _ *struct{}) (*Body[Instance], error) {
			settings, err := s.Service.SetInstanceIcon(ctx, mustPrincipal(ctx), nil, baseURLFrom(ctx))
			if err != nil {
				return nil, err
			}
			info, err := s.instanceInfo(ctx, settings)
			if err != nil {
				return nil, err
			}
			return ok(info)
		}))
}

// handleMedia serves uploaded files. Keys are unique per upload, so responses are cached
// for a year.
func (s *Server) handleMedia(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "*")
	etag := `"` + key + `"`
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	rc, obj, err := s.Service.OpenMedia(r.Context(), key)
	if err != nil {
		var ae *apperr.Error
		switch {
		case errors.As(err, &ae) && ae.Kind == apperr.KindNotFound:
			writeProblem(w, http.StatusNotFound, "file not found")
		case errors.As(err, &ae):
			writeProblem(w, http.StatusServiceUnavailable, ae.Message)
		default:
			s.Logger.Error("serving media", "key", key, "error", err)
			writeProblem(w, http.StatusServiceUnavailable, "the file is temporarily unavailable")
		}
		return
	}
	defer func() { _ = rc.Close() }()
	h := w.Header()
	h.Set("Content-Type", obj.ContentType)
	if obj.Size >= 0 {
		h.Set("Content-Length", strconv.FormatInt(obj.Size, 10))
	}
	h.Set("Cache-Control", "public, max-age=31536000, immutable")
	h.Set("ETag", etag)
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	h.Set("Cross-Origin-Resource-Policy", "cross-origin")
	if !obj.ModTime.IsZero() {
		h.Set("Last-Modified", obj.ModTime.UTC().Format(http.TimeFormat))
	}
	if r.Method == http.MethodHead {
		return
	}
	_, _ = io.Copy(w, rc)
}

type PasswordResetRequest struct {
	Email string `json:"email" format:"email" maxLength:"254"`
}

type PasswordResetConfirmRequest struct {
	Token       string `json:"token" minLength:"1" maxLength:"128" doc:"The token from the reset link"`
	NewPassword string `json:"new_password" minLength:"10" maxLength:"256"`
}

type EmailTokenRequest struct {
	Token string `json:"token" minLength:"1" maxLength:"128" doc:"The token from the verification link"`
}

func (s *Server) registerEmailFlows() {
	huma.Register(s.api, withAuthRateLimit(withStatus(operation("request-password-reset", http.MethodPost, "/auth/password-reset",
		"Email a password reset link", tagAuth), http.StatusAccepted)),
		handle(s, func(ctx context.Context, in *Body[PasswordResetRequest]) (*struct{}, error) {
			// The answer is the same whether or not an account uses the address.
			return nil, s.Service.RequestPasswordReset(ctx, in.Body.Email, baseURLFrom(ctx))
		}))

	huma.Register(s.api, withAuthRateLimit(withStatus(operation("confirm-password-reset", http.MethodPost, "/auth/password-reset/confirm",
		"Set a new password with a reset token; signs out every session", tagAuth), http.StatusNoContent)),
		handle(s, func(ctx context.Context, in *Body[PasswordResetConfirmRequest]) (*struct{}, error) {
			return nil, s.Service.ResetPassword(ctx, in.Body.Token, in.Body.NewPassword)
		}))

	huma.Register(s.api, withAuthRateLimit(operation("verify-email", http.MethodPost, "/auth/verify-email",
		"Confirm an email address with a verification token", tagAuth)),
		handle(s, func(ctx context.Context, in *Body[EmailTokenRequest]) (*Body[User], error) {
			user, err := s.Service.VerifyEmail(ctx, in.Body.Token)
			if err != nil {
				return nil, err
			}
			return ok(toUser(user))
		}))

	huma.Register(s.api, withSessionOnly(withAuthRateLimit(withStatus(withAuth(operation("send-verification-email", http.MethodPost,
		"/users/@me/email/verification", "Email a link that verifies the account's address", tagUsers)), http.StatusAccepted))),
		handle(s, func(ctx context.Context, _ *struct{}) (*struct{}, error) {
			return nil, s.Service.SendVerificationEmail(ctx, mustPrincipal(ctx), baseURLFrom(ctx))
		}))
}
