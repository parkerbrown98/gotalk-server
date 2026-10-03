package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/parkerbrown98/gotalk-server/internal/service"
)

const (
	tagAuth  = "Auth"
	tagUsers = "Users"
)

type RegisterRequest struct {
	Username       string `json:"username" minLength:"3" maxLength:"32" pattern:"^[a-zA-Z0-9_.-]+$"`
	Email          string `json:"email" format:"email" maxLength:"254"`
	Password       string `json:"password" minLength:"10" maxLength:"256"`
	InviteCode     string `json:"invite_code,omitempty" maxLength:"64" doc:"Required when registration_mode is invite_only; joins the invite's place"`
	AcceptPolicies bool   `json:"accept_policies,omitempty" doc:"Record consent to every current policy that requires it (see GET /policies)"`
}

type LoginRequest struct {
	Login    string `json:"login" minLength:"1" maxLength:"254" doc:"Username or email address"`
	Password string `json:"password" minLength:"1" maxLength:"256"`
}

type RefreshRequest struct {
	RefreshToken string `json:"refresh_token" minLength:"1" maxLength:"256"`
}

type ProfileUpdateRequest struct {
	DisplayName *string `json:"display_name,omitempty" maxLength:"64"`
	Bio         *string `json:"bio,omitempty" maxLength:"1000"`
	Pronouns    *string `json:"pronouns,omitempty" maxLength:"40"`
	AvatarURL   *string `json:"avatar_url,omitempty" maxLength:"2048" doc:"Empty string clears the avatar"`
}

type PasswordChangeRequest struct {
	CurrentPassword string `json:"current_password" minLength:"1" maxLength:"256"`
	NewPassword     string `json:"new_password" minLength:"10" maxLength:"256"`
}

type AccountDeleteRequest struct {
	Password string `json:"password" minLength:"1" maxLength:"256"`
}

type SessionPath struct {
	SessionID string `path:"sessionID" format:"uuid"`
}

type UsernamePath struct {
	Username string `path:"username" maxLength:"32"`
}

func (s *Server) registerAuth() {
	huma.Register(s.api, withAuthRateLimit(withStatus(operation("register", http.MethodPost, "/auth/register",
		"Create an account", tagAuth), http.StatusCreated)),
		handle(s, func(ctx context.Context, in *Body[RegisterRequest]) (*Body[Tokens], error) {
			res, err := s.Service.Register(ctx, service.RegisterInput{
				Username:       in.Body.Username,
				Email:          in.Body.Email,
				Password:       in.Body.Password,
				InviteCode:     in.Body.InviteCode,
				AcceptPolicies: in.Body.AcceptPolicies,
			}, clientFrom(ctx))
			if err != nil {
				return nil, err
			}
			return ok(toTokens(res))
		}))

	huma.Register(s.api, withAuthRateLimit(operation("login", http.MethodPost, "/auth/login",
		"Log in with a username or email and password", tagAuth)),
		handle(s, func(ctx context.Context, in *Body[LoginRequest]) (*Body[Tokens], error) {
			res, err := s.Service.Login(ctx, in.Body.Login, in.Body.Password, clientFrom(ctx))
			if err != nil {
				return nil, err
			}
			return ok(toTokens(res))
		}))

	huma.Register(s.api, withAuthRateLimit(operation("refresh", http.MethodPost, "/auth/refresh",
		"Exchange a refresh token for new tokens", tagAuth)),
		handle(s, func(ctx context.Context, in *Body[RefreshRequest]) (*Body[Tokens], error) {
			res, err := s.Service.Refresh(ctx, in.Body.RefreshToken, clientFrom(ctx))
			if err != nil {
				return nil, err
			}
			return ok(toTokens(res))
		}))

	huma.Register(s.api, withSessionOnly(withStatus(withAuth(operation("logout", http.MethodPost, "/auth/logout",
		"End the current session", tagAuth)), http.StatusNoContent)),
		handle(s, func(ctx context.Context, _ *struct{}) (*struct{}, error) {
			return nil, s.Service.Logout(ctx, mustPrincipal(ctx))
		}))
}

func (s *Server) registerUsers() {
	huma.Register(s.api, withAuth(operation("get-me", http.MethodGet, "/users/@me",
		"Get the authenticated user", tagUsers)),
		handle(s, func(ctx context.Context, _ *struct{}) (*Body[SelfUser], error) {
			return ok(toSelfUser(mustPrincipal(ctx).User))
		}))

	huma.Register(s.api, withAuth(operation("update-me", http.MethodPatch, "/users/@me",
		"Update the authenticated user's profile", tagUsers)),
		handle(s, func(ctx context.Context, in *Body[ProfileUpdateRequest]) (*Body[SelfUser], error) {
			user, err := s.Service.UpdateProfile(ctx, mustPrincipal(ctx), service.ProfileUpdate{
				DisplayName: in.Body.DisplayName,
				Bio:         in.Body.Bio,
				Pronouns:    in.Body.Pronouns,
				AvatarURL:   in.Body.AvatarURL,
			})
			if err != nil {
				return nil, err
			}
			return ok(toSelfUser(user))
		}))

	huma.Register(s.api, withSessionOnly(withAuthRateLimit(withAuth(operation("delete-me", http.MethodDelete, "/users/@me",
		"Delete the authenticated user's account", tagUsers)))),
		handle(s, func(ctx context.Context, in *Body[AccountDeleteRequest]) (*struct{}, error) {
			return nil, s.Service.DeleteAccount(ctx, mustPrincipal(ctx), in.Body.Password)
		}))

	huma.Register(s.api, withSessionOnly(withAuthRateLimit(withStatus(withAuth(operation("change-password", http.MethodPost,
		"/users/@me/password", "Change password, sign out other sessions and revoke personal access tokens", tagUsers)), http.StatusNoContent))),
		handle(s, func(ctx context.Context, in *Body[PasswordChangeRequest]) (*struct{}, error) {
			return nil, s.Service.ChangePassword(ctx, mustPrincipal(ctx), in.Body.CurrentPassword, in.Body.NewPassword)
		}))

	huma.Register(s.api, withSessionOnly(withAuth(operation("list-my-sessions", http.MethodGet, "/users/@me/sessions",
		"List active sessions (logged-in devices)", tagUsers))),
		handle(s, func(ctx context.Context, _ *struct{}) (*Body[[]Session], error) {
			p := mustPrincipal(ctx)
			sessions, err := s.Service.ListSessions(ctx, p)
			if err != nil {
				return nil, err
			}
			out := make([]Session, len(sessions))
			for i, sess := range sessions {
				out[i] = toSession(sess, p.SessionID)
			}
			return ok(out)
		}))

	huma.Register(s.api, withSessionOnly(withAuth(operation("revoke-my-session", http.MethodDelete, "/users/@me/sessions/{sessionID}",
		"Revoke a session", tagUsers))),
		handle(s, func(ctx context.Context, in *SessionPath) (*struct{}, error) {
			id, err := parseID("sessionID", in.SessionID)
			if err != nil {
				return nil, err
			}
			return nil, s.Service.RevokeSession(ctx, mustPrincipal(ctx), id)
		}))

	huma.Register(s.api, withAuth(operation("list-my-places", http.MethodGet, "/users/@me/places",
		"List places the authenticated user belongs to", tagUsers)),
		handle(s, func(ctx context.Context, _ *struct{}) (*Body[[]Place], error) {
			places, err := s.Service.ListMyPlaces(ctx, mustPrincipal(ctx))
			if err != nil {
				return nil, err
			}
			return ok(mapSlice(places, toPlace))
		}))

	huma.Register(s.api, operation("get-user", http.MethodGet, "/users/{username}",
		"Get a user's public profile", tagUsers),
		handle(s, func(ctx context.Context, in *UsernamePath) (*Body[User], error) {
			user, err := s.Service.GetUserByUsername(ctx, in.Username)
			if err != nil {
				return nil, err
			}
			return ok(toUser(user))
		}))
}
