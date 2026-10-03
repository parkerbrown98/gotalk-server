package api

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/parkerbrown98/gotalk-server/internal/apperr"
	"github.com/parkerbrown98/gotalk-server/internal/ratelimit"
	"github.com/parkerbrown98/gotalk-server/internal/service"
)

type ctxKey int

const (
	principalKey ctxKey = iota
	clientKey
	baseURLKey
)

func principalFrom(ctx context.Context) *service.Principal {
	p, _ := ctx.Value(principalKey).(*service.Principal)
	return p
}

func clientFrom(ctx context.Context) service.ClientInfo {
	c, _ := ctx.Value(clientKey).(service.ClientInfo)
	return c
}

func baseURLFrom(ctx context.Context) string {
	b, _ := ctx.Value(baseURLKey).(string)
	return b
}

// requestMeta stores the caller's IP/user agent and this instance's public base URL in the
// request context for handlers that are not plain net/http.
func (s *Server) requestMeta(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip, viaProxy := s.clientIP(r)
		ctx := context.WithValue(r.Context(), clientKey, service.ClientInfo{IP: ip, UserAgent: r.UserAgent()})
		ctx = context.WithValue(ctx, baseURLKey, s.baseURL(r, viaProxy))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) trustedProxy(addr netip.Addr) bool {
	addr = addr.Unmap()
	for _, p := range s.proxies {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// clientIP resolves the caller's address. Forwarding headers are only honored when proxy
// trust is enabled and the direct peer is a trusted proxy; the client is then the
// right-most X-Forwarded-For hop that is not itself a trusted proxy, since entries to its
// left can be forged by the client.
func (s *Server) clientIP(r *http.Request) (ip string, viaProxy bool) {
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	peer, err := netip.ParseAddr(host)
	if err != nil || !s.Config.Server.TrustProxy || !s.trustedProxy(peer) {
		return host, false
	}
	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		hop, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			break
		}
		if !s.trustedProxy(hop) {
			return hop.Unmap().String(), true
		}
	}
	return peer.Unmap().String(), true
}

func (s *Server) baseURL(r *http.Request, viaProxy bool) string {
	if s.Config.Server.PublicURL != "" {
		return s.Config.Server.PublicURL
	}
	scheme, host := "http", r.Host
	if r.TLS != nil {
		scheme = "https"
	}
	if viaProxy {
		if p := r.Header.Get("X-Forwarded-Proto"); p == "https" || p == "http" {
			scheme = p
		}
		if h := r.Header.Get("X-Forwarded-Host"); h != "" {
			host = h
		}
	}
	return scheme + "://" + host
}

// Paths that stay reachable before first-run setup completes.
var setupAllowedPrefixes = []string{
	"/healthz", "/readyz", "/setup", "/.well-known/",
	APIPrefix + "/setup", APIPrefix + "/instance", APIPrefix + "/permissions",
	APIPrefix + "/openapi", APIPrefix + "/docs", APIPrefix + "/schemas",
}

// setupGate redirects browsers to the wizard and rejects API calls with 503 until the
// instance has been set up.
func (s *Server) setupGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, p := range setupAllowedPrefixes {
			if strings.HasPrefix(r.URL.Path, p) {
				next.ServeHTTP(w, r)
				return
			}
		}
		required, err := s.Service.SetupRequired(r.Context())
		if err != nil {
			s.Logger.Error("checking setup state", "error", err)
			writeProblem(w, http.StatusServiceUnavailable, "the database is unavailable")
			return
		}
		if !required {
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Retry-After", "30")
			writeProblem(w, http.StatusServiceUnavailable,
				"this instance has not been set up yet; an administrator must complete setup at /setup")
			return
		}
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
	})
}

func writeProblem(w http.ResponseWriter, status int, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"title":  http.StatusText(status),
		"status": status,
		"detail": detail,
	})
}

// authRequired reports whether op demands authentication. An empty security requirement
// in the list marks authentication as optional.
func authRequired(op *huma.Operation) bool {
	if len(op.Security) == 0 {
		return false
	}
	for _, req := range op.Security {
		if len(req) == 0 {
			return false
		}
	}
	return true
}

func (s *Server) authMiddleware(ctx huma.Context, next func(huma.Context)) {
	header := ctx.Header("Authorization")
	if header == "" {
		if authRequired(ctx.Operation()) {
			ctx.SetHeader("WWW-Authenticate", `Bearer realm="gotalk"`)
			_ = huma.WriteErr(s.api, ctx, http.StatusUnauthorized, "authentication required")
			return
		}
		next(ctx)
		return
	}
	token, ok := strings.CutPrefix(header, "Bearer ")
	if !ok || token == "" {
		ctx.SetHeader("WWW-Authenticate", `Bearer realm="gotalk", error="invalid_request"`)
		_ = huma.WriteErr(s.api, ctx, http.StatusUnauthorized, "authorization header must use the Bearer scheme")
		return
	}
	p, err := s.Service.Authenticate(ctx.Context(), token)
	if err != nil {
		var ae *apperr.Error
		if errors.As(err, &ae) {
			ctx.SetHeader("WWW-Authenticate", `Bearer realm="gotalk", error="invalid_token"`)
			_ = huma.WriteErr(s.api, ctx, http.StatusUnauthorized, ae.Message)
			return
		}
		s.Logger.Error("authenticating request", "error", err)
		_ = huma.WriteErr(s.api, ctx, http.StatusInternalServerError, "internal server error")
		return
	}
	next(huma.WithValue(ctx, principalKey, p))
}

const rateLimitTierKey = "rateLimitTier"

// rateLimitMiddleware applies the operation's tier, keyed by user when authenticated and
// by client IP otherwise. It fails open if the backing store is unavailable.
func (s *Server) rateLimitMiddleware(ctx huma.Context, next func(huma.Context)) {
	if !s.Config.RateLimit.Enabled {
		next(ctx)
		return
	}
	tier := ratelimit.TierDefault
	if t, ok := ctx.Operation().Metadata[rateLimitTierKey].(string); ok {
		tier = t
	}
	key := "ip:" + clientFrom(ctx.Context()).IP
	if p := principalFrom(ctx.Context()); p != nil {
		key = "user:" + p.User.ID.String()
	}
	res, err := s.Limiter.Take(ctx.Context(), tier, key)
	if err != nil {
		s.Logger.Warn("rate limiter unavailable; allowing request", "error", err)
		next(ctx)
		return
	}
	if res.Limit >= 0 {
		ctx.SetHeader("X-RateLimit-Limit", strconv.FormatInt(res.Limit, 10))
		ctx.SetHeader("X-RateLimit-Remaining", strconv.FormatInt(res.Remaining, 10))
		ctx.SetHeader("X-RateLimit-Reset", strconv.FormatInt(res.Reset.Unix(), 10))
	}
	if res.Reached {
		retry := max(int64(time.Until(res.Reset).Seconds()), 1)
		ctx.SetHeader("Retry-After", strconv.FormatInt(retry, 10))
		_ = huma.WriteErr(s.api, ctx, http.StatusTooManyRequests,
			"rate limit exceeded; retry after "+strconv.FormatInt(retry, 10)+"s")
		return
	}
	next(ctx)
}

// handle adapts a service-style handler, translating service errors into RFC 9457
// problem responses.
func handle[I, O any](s *Server, fn func(context.Context, *I) (*O, error)) func(context.Context, *I) (*O, error) {
	return func(ctx context.Context, in *I) (*O, error) {
		out, err := fn(ctx, in)
		if err != nil {
			return nil, s.toHTTPError(ctx, err)
		}
		return out, nil
	}
}

func (s *Server) toHTTPError(ctx context.Context, err error) error {
	var se huma.StatusError
	if errors.As(err, &se) {
		return err
	}
	var ae *apperr.Error
	if errors.As(err, &ae) {
		switch ae.Kind {
		case apperr.KindInvalid:
			return huma.Error422UnprocessableEntity(ae.Message)
		case apperr.KindUnauthorized:
			return huma.Error401Unauthorized(ae.Message)
		case apperr.KindForbidden:
			return huma.Error403Forbidden(ae.Message)
		case apperr.KindNotFound:
			return huma.Error404NotFound(ae.Message)
		case apperr.KindConflict:
			return huma.Error409Conflict(ae.Message)
		case apperr.KindUnavailable:
			return huma.Error503ServiceUnavailable(ae.Message)
		}
	}
	s.Logger.ErrorContext(ctx, "unhandled error", "error", err, "request_id", middleware.GetReqID(ctx))
	return huma.Error500InternalServerError("internal server error")
}

func mustPrincipal(ctx context.Context) *service.Principal {
	p := principalFrom(ctx)
	if p == nil {
		// authMiddleware guarantees a principal for secured operations.
		panic("api: secured operation reached handler without a principal")
	}
	return p
}
