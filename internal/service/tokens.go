package service

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/parkerbrown98/gotalk-server/internal/apperr"
	"github.com/parkerbrown98/gotalk-server/internal/auth"
	"github.com/parkerbrown98/gotalk-server/internal/store"
)

// API token scopes. A token may only do what its scopes allow; login sessions hold all of them.
const (
	// ScopeRead allows GET requests.
	ScopeRead = "read"
	// ScopeWrite allows every other request.
	ScopeWrite = "write"
	// ScopeGateway allows connecting to the real-time gateway.
	ScopeGateway = "gateway"
	// ScopeAdmin keeps the instance administrator role for an administrator's token.
	ScopeAdmin = "admin"

	TokenKindPersonal = "personal"
	TokenKindBot      = "bot"

	MaxPersonalTokens = 25
	maxTokenNameLen   = 100
	maxTokenLifetime  = 366 * 24 * time.Hour
)

// Scopes lists every scope in canonical order.
var Scopes = []string{ScopeRead, ScopeWrite, ScopeGateway, ScopeAdmin}

var botScopes = []string{ScopeRead, ScopeWrite, ScopeGateway}

// ViaToken reports whether the caller authenticated with an API token (personal or bot)
// rather than a login session.
func (p *Principal) ViaToken() bool { return p.Token != nil }

// HasScope reports whether the caller may act with scope.
func (p *Principal) HasScope(scope string) bool {
	return p.Token == nil || slices.Contains(p.Token.Scopes, scope)
}

// authenticateAPIToken resolves a personal access token or bot token. The token's ID
// stands in for the session ID, so revoking the token closes its gateway connections.
func (s *Service) authenticateAPIToken(ctx context.Context, token string) (*Principal, error) {
	row, err := s.q.GetAPITokenUser(ctx, auth.HashAPIToken(token))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, apperr.Unauthorized("API token is invalid, expired or revoked")
	}
	if err != nil {
		return nil, err
	}
	t, user := row.ApiToken, row.User
	// An administrator's everyday tokens should not be able to administer the instance.
	if !slices.Contains(t.Scopes, ScopeAdmin) {
		user.IsInstanceAdmin = false
	}
	if t.LastUsedAt == nil || time.Since(*t.LastUsedAt) > time.Minute {
		if err := s.q.TouchAPIToken(ctx, t.ID); err != nil {
			s.log.Warn("recording API token use", "token", t.ID, "error", err)
		}
	}
	return &Principal{User: user, SessionID: t.ID, Token: &t}, nil
}

// normalizeScopes validates scopes and returns them deduplicated in canonical order.
func normalizeScopes(scopes []string) ([]string, error) {
	if len(scopes) == 0 {
		return nil, apperr.Invalid("choose at least one scope (%s)", strings.Join(Scopes, ", "))
	}
	for _, sc := range scopes {
		if !slices.Contains(Scopes, sc) {
			return nil, apperr.Invalid("unknown scope %q; scopes are %s", sc, strings.Join(Scopes, ", "))
		}
	}
	out := []string{}
	for _, sc := range Scopes {
		if slices.Contains(scopes, sc) {
			out = append(out, sc)
		}
	}
	return out, nil
}

type TokenInput struct {
	Name   string
	Scopes []string
	// ExpiresIn is the token's lifetime; zero means it never expires.
	ExpiresIn time.Duration
}

// CreatePersonalToken issues a personal access token. The token itself is returned once;
// only its hash is stored.
func (s *Service) CreatePersonalToken(ctx context.Context, p *Principal, in TokenInput) (store.ApiToken, string, error) {
	if p.User.IsBot {
		return store.ApiToken{}, "", apperr.Forbidden("bots cannot create personal access tokens")
	}
	if p.ViaToken() {
		return store.ApiToken{}, "", apperr.Forbidden("API tokens cannot create other tokens; use a login session")
	}
	name := strings.TrimSpace(in.Name)
	if name == "" || utf8.RuneCountInString(name) > maxTokenNameLen {
		return store.ApiToken{}, "", apperr.Invalid("name must be 1-%d characters", maxTokenNameLen)
	}
	scopes, err := normalizeScopes(in.Scopes)
	if err != nil {
		return store.ApiToken{}, "", err
	}
	if slices.Contains(scopes, ScopeAdmin) && !p.User.IsInstanceAdmin {
		return store.ApiToken{}, "", apperr.Forbidden("only instance administrators can create tokens with the admin scope")
	}
	var expires *time.Time
	switch {
	case in.ExpiresIn < 0 || in.ExpiresIn > maxTokenLifetime:
		return store.ApiToken{}, "", apperr.Invalid("tokens can last at most 366 days")
	case in.ExpiresIn > 0:
		t := time.Now().Add(in.ExpiresIn)
		expires = &t
	}
	n, err := s.q.CountActivePersonalTokens(ctx, p.User.ID)
	if err != nil {
		return store.ApiToken{}, "", err
	}
	if n >= MaxPersonalTokens {
		return store.ApiToken{}, "", apperr.Conflict("you can have at most %d personal access tokens; revoke one first", MaxPersonalTokens)
	}
	token, hash, hint, err := auth.NewAPIToken(auth.PersonalTokenPrefix)
	if err != nil {
		return store.ApiToken{}, "", err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return store.ApiToken{}, "", err
	}
	t, err := s.q.CreateAPIToken(ctx, store.CreateAPITokenParams{
		ID: id, UserID: p.User.ID, Kind: TokenKindPersonal, Name: name, TokenHash: hash, TokenHint: hint,
		Scopes: scopes, ExpiresAt: expires,
	})
	return t, token, err
}

func (s *Service) ListPersonalTokens(ctx context.Context, p *Principal) ([]store.ApiToken, error) {
	return s.q.ListPersonalTokens(ctx, p.User.ID)
}

// RevokePersonalToken revokes one of the caller's tokens and closes its gateway connections.
func (s *Service) RevokePersonalToken(ctx context.Context, p *Principal, tokenID uuid.UUID) error {
	n, err := s.q.RevokePersonalToken(ctx, store.RevokePersonalTokenParams{ID: tokenID, UserID: p.User.ID})
	if err != nil {
		return err
	}
	if n == 0 {
		return apperr.NotFound("token not found")
	}
	s.emitSessionsEnded(ctx, s.q, p.User.ID, []uuid.UUID{tokenID}, nil)
	return nil
}

// issueBotToken creates a new token for an application's bot.
func (s *Service) issueBotToken(ctx context.Context, q *store.Queries, app store.Application) (string, error) {
	token, hash, hint, err := auth.NewAPIToken(auth.BotTokenPrefix)
	if err != nil {
		return "", err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return "", err
	}
	_, err = q.CreateAPIToken(ctx, store.CreateAPITokenParams{
		ID: id, UserID: app.BotUserID, ApplicationID: &app.ID, Kind: TokenKindBot, Name: "bot",
		TokenHash: hash, TokenHint: hint, Scopes: botScopes,
	})
	return token, err
}
