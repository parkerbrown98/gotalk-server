// Package api exposes Gotalk over HTTP: a chi router hosting the huma-based REST API under
// /api/v1 plus health, discovery, and setup-wizard routes.
package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"github.com/redis/go-redis/v9"

	"github.com/parkerbrown98/gotalk-server/internal/config"
	"github.com/parkerbrown98/gotalk-server/internal/ratelimit"
	"github.com/parkerbrown98/gotalk-server/internal/realtime"
	"github.com/parkerbrown98/gotalk-server/internal/service"
)

const (
	APIVersion = "v1"
	APIPrefix  = "/api/" + APIVersion
)

type Deps struct {
	Service *service.Service
	Limiter *ratelimit.Limiter
	// Redis is optional. When set, gateway events and presence are shared through it so
	// every replica can serve every client.
	Redis   *redis.Client
	Config  *config.Config
	Logger  *slog.Logger
	Version string
	// HeartbeatInterval overrides the gateway heartbeat (30s by default).
	HeartbeatInterval time.Duration
}

// Server is the HTTP handler for an instance. Close it to disconnect gateway clients.
type Server struct {
	Deps
	api     huma.API
	proxies []netip.Prefix
	hub     *realtime.Hub
	handler http.Handler
}

// New builds the full HTTP handler and starts the real-time gateway.
func New(d Deps) (*Server, error) {
	s := &Server{Deps: d, proxies: d.Config.Server.TrustedProxyPrefixes()}

	broker, presence := realtime.NewMemoryBroker(), realtime.NewMemoryPresence()
	if d.Redis != nil {
		broker, presence = realtime.NewRedisBroker(d.Redis), realtime.NewRedisPresence(d.Redis)
	}
	s.hub = realtime.NewHub(broker, presence, d.Service, d.Logger)
	if err := s.hub.Start(context.Background()); err != nil {
		return nil, fmt.Errorf("starting the real-time gateway: %w", err)
	}
	d.Service.SetPublisher(gatewayPublisher{hub: s.hub, log: d.Logger})

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(s.requestMeta)
	r.Use(s.requestLogger)
	r.Use(middleware.Recoverer)
	r.Use(securityHeaders)
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   d.Config.Server.CORSAllowedOrigins,
		AllowedMethods:   []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions},
		AllowedHeaders:   []string{"Authorization", "Content-Type", "X-Request-Id"},
		ExposedHeaders:   []string{"X-RateLimit-Limit", "X-RateLimit-Remaining", "X-RateLimit-Reset", "Retry-After", "X-Request-Id", "Link"},
		AllowCredentials: d.Config.Server.CORSAllowCredentials,
		MaxAge:           300,
	}))
	r.Use(s.setupGate)

	r.Get("/healthz", s.handleHealthz)
	r.Get("/readyz", s.handleReadyz)
	r.Get("/.well-known/gotalk-instance", s.handleWellKnown)
	r.Get("/", s.handleLanding)
	r.Get("/setup", s.handleSetupPage)

	r.Route(APIPrefix, func(r chi.Router) {
		r.Get(gatewayPathSuffix, s.handleGateway)

		cfg := huma.DefaultConfig("Gotalk API", d.Version)
		cfg.Info.Description = "REST API for a Gotalk instance. Any client may target any instance; " +
			"start with GET /instance to discover capabilities."
		cfg.Servers = []*huma.Server{{URL: APIPrefix}}
		// Drop the default $schema link transformer to keep response bodies minimal.
		cfg.CreateHooks = nil
		cfg.Components.SecuritySchemes = map[string]*huma.SecurityScheme{
			"bearer": {Type: "http", Scheme: "bearer", BearerFormat: "JWT"},
		}
		s.api = humachi.New(r, cfg)
		s.api.UseMiddleware(s.authMiddleware, s.rateLimitMiddleware)

		s.registerMeta()
		s.registerAuth()
		s.registerUsers()
		s.registerPlaces()
		s.registerMembers()
		s.registerRoles()
		s.registerInvites()
		s.registerBoards()
		s.registerTopics()
		s.registerPosts()
		s.registerSearch()
		s.registerNotifications()
		s.registerDrafts()
		s.registerModeration()
		s.registerChannels()
		s.registerMessages()
		s.registerDirectMessages()
	})

	s.handler = r
	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.handler.ServeHTTP(w, r) }

// Close disconnects every gateway client (close code 1001) and stops routing events. It
// is safe to call more than once.
func (s *Server) Close() {
	s.Service.SetPublisher(nil)
	s.hub.Close()
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)

		level := slog.LevelInfo
		switch {
		case r.URL.Path == "/healthz" || r.URL.Path == "/readyz":
			level = slog.LevelDebug
		case ww.Status() >= 500:
			level = slog.LevelError
		}
		s.Logger.Log(r.Context(), level, "http request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", ww.Status(),
			"bytes", ww.BytesWritten(),
			"duration_ms", time.Since(start).Milliseconds(),
			"client_ip", clientFrom(r.Context()).IP,
			"request_id", middleware.GetReqID(r.Context()),
		)
	})
}
