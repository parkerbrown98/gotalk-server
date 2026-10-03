// Package service holds Gotalk's business logic. It is transport-agnostic: handlers in
// internal/api translate HTTP to these calls, and the CLI reuses them for headless setup.
package service

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/parkerbrown98/gotalk-server/internal/apperr"
	"github.com/parkerbrown98/gotalk-server/internal/auth"
	"github.com/parkerbrown98/gotalk-server/internal/config"
	"github.com/parkerbrown98/gotalk-server/internal/store"
)

type Service struct {
	pool      *pgxpool.Pool
	q         *store.Queries
	cfg       *config.Config
	log       *slog.Logger
	tokens    *auth.TokenIssuer
	setupDone atomic.Bool
	events    atomic.Pointer[Publisher]
	// pending buffers events emitted inside a transaction, keyed by its *store.Queries,
	// until the transaction commits.
	pending sync.Map
}

// New prepares the service, creating the instance settings row (with a generated JWT
// secret and setup token) on first boot.
func New(ctx context.Context, pool *pgxpool.Pool, cfg *config.Config, log *slog.Logger) (*Service, error) {
	s := &Service{pool: pool, q: store.New(pool), cfg: cfg, log: log}

	secret := make([]byte, 64)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	token, err := auth.RandomString(32, auth.Base62)
	if err != nil {
		return nil, err
	}
	if err := s.q.EnsureInstanceSettings(ctx, store.EnsureInstanceSettingsParams{
		JwtSecret:  secret,
		SetupToken: &token,
	}); err != nil {
		return nil, fmt.Errorf("initializing instance settings: %w", err)
	}

	settings, err := s.q.GetInstanceSettings(ctx)
	if err != nil {
		return nil, fmt.Errorf("loading instance settings: %w", err)
	}
	s.setupDone.Store(settings.SetupCompletedAt != nil)

	jwtSecret := settings.JwtSecret
	if cfg.Auth.JWTSecret != "" {
		jwtSecret = []byte(cfg.Auth.JWTSecret)
	}
	s.tokens = auth.NewTokenIssuer(jwtSecret, cfg.Auth.AccessTokenTTL)
	return s, nil
}

func (s *Service) Pool() *pgxpool.Pool { return s.pool }

func (s *Service) Config() *config.Config { return s.cfg }

func (s *Service) tx(ctx context.Context, fn func(q *store.Queries) error) error {
	var events []Event
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		s.pending.Store(q, &events)
		defer s.pending.Delete(q)
		return fn(q)
	})
	if err != nil {
		return err
	}
	for _, ev := range events {
		s.publish(ctx, ev)
	}
	return nil
}

func notFound(err error, format string, args ...any) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return apperr.NotFound(format, args...)
	}
	return err
}

// ClientInfo describes the caller for session bookkeeping.
type ClientInfo struct {
	IP        string
	UserAgent string
}

// Pagination bounds list queries.
type Pagination struct {
	Limit  int32
	Offset int32
}

func (p Pagination) normalized() Pagination {
	if p.Limit <= 0 || p.Limit > 100 {
		p.Limit = 50
	}
	if p.Offset < 0 {
		p.Offset = 0
	}
	return p
}

var (
	usernamePattern = regexp.MustCompile(`^[a-zA-Z0-9_.-]{3,32}$`)
	slugPattern     = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,30}[a-z0-9]$`)

	reservedUsernames = setOf("admin", "administrator", "root", "system", "gotalk", "support",
		"moderator", "mod", "staff", "everyone", "here", "me", "@me", "null", "undefined", "api", "setup")
	reservedSlugs = setOf("new", "admin", "api", "setup", "discover", "settings", "explore", "search",
		"places", "users", "login", "logout", "register", "invite", "invites", "static", "well-known")
)

func setOf(items ...string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, i := range items {
		m[i] = true
	}
	return m
}

const (
	minPasswordLen = 10
	// Argon2 cost grows with input size; cap it to keep hashing bounded.
	maxPasswordLen = 256
)

func validateUsername(username string) error {
	if !usernamePattern.MatchString(username) {
		return apperr.Invalid("username must be 3-32 characters of letters, numbers, '_', '.' or '-'")
	}
	if reservedUsernames[strings.ToLower(username)] {
		return apperr.Invalid("username %q is reserved", username)
	}
	return nil
}

func validatePassword(password string) error {
	if len(password) < minPasswordLen || len(password) > maxPasswordLen {
		return apperr.Invalid("password must be between %d and %d characters", minPasswordLen, maxPasswordLen)
	}
	return nil
}

func validateEmail(email string) error {
	addr, err := mail.ParseAddress(email)
	if err != nil || addr.Address != email || len(email) > 254 {
		return apperr.Invalid("email address is invalid")
	}
	return nil
}

func validateSlug(slug string) error {
	if !slugPattern.MatchString(slug) {
		return apperr.Invalid("slug must be 3-32 lowercase letters, numbers or '-', starting and ending with a letter or number")
	}
	if reservedSlugs[slug] {
		return apperr.Invalid("slug %q is reserved", slug)
	}
	return nil
}

func validateRegistrationMode(mode string) error {
	switch mode {
	case "open", "invite_only", "closed":
		return nil
	}
	return apperr.Invalid("registration_mode must be one of open, invite_only, closed")
}

// validateOptionalURL accepts nil (unchanged), "" (clear) or an absolute http(s) URL, which
// keeps javascript: and data: URLs out of fields clients render as links or images.
func validateOptionalURL(field string, v *string) error {
	if v == nil || *v == "" {
		return nil
	}
	u, err := url.Parse(*v)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || len(*v) > 2048 {
		return apperr.Invalid("%s must be an absolute http(s) URL", field)
	}
	return nil
}
