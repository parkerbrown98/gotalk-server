// Command gotalk runs a Gotalk server.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/parkerbrown98/gotalk-server/internal/api"
	"github.com/parkerbrown98/gotalk-server/internal/apperr"
	"github.com/parkerbrown98/gotalk-server/internal/config"
	"github.com/parkerbrown98/gotalk-server/internal/database"
	"github.com/parkerbrown98/gotalk-server/internal/ratelimit"
	"github.com/parkerbrown98/gotalk-server/internal/service"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `Gotalk server

Usage:
  gotalk [command] [--config FILE]

Commands:
  serve        Run the server (default)
  migrate      Apply database migrations, or "migrate status" to show the schema version
  setup        Complete first-run setup headlessly from config/environment, then exit
  healthcheck  Exit 0 if the local server is healthy (for container HEALTHCHECK)
  version      Print the version

Configuration is read from built-in defaults, then --config (YAML), then GOTALK_*
environment variables. See the README for every option.
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Args[1:]); err != nil {
		var ae *apperr.Error
		if errors.As(err, &ae) {
			fmt.Fprintln(os.Stderr, "error:", ae.Message)
		} else {
			fmt.Fprintln(os.Stderr, "error:", err)
		}
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}

	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	configFile := fs.String("config", os.Getenv("GOTALK_CONFIG"), "path to a YAML config file")
	fs.StringVar(configFile, "from-file", *configFile, "alias for --config")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	switch cmd {
	case "version":
		fmt.Println(version)
		return nil
	case "help":
		fmt.Print(usage)
		return nil
	}

	cfg, err := config.Load(*configFile)
	if err != nil {
		return err
	}
	log := newLogger(cfg.Log, os.Stderr)

	switch cmd {
	case "serve":
		return serve(ctx, cfg, log)
	case "migrate":
		return migrate(ctx, cfg, log, fs.Args())
	case "setup":
		return setup(ctx, cfg, log)
	case "healthcheck":
		return healthcheck(cfg)
	default:
		fmt.Fprint(os.Stderr, usage)
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func newLogger(c config.Log, w io.Writer) *slog.Logger {
	var level slog.Level
	if err := level.UnmarshalText([]byte(c.Level)); err != nil {
		level = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: level}
	if c.Format == "text" {
		return slog.New(slog.NewTextHandler(w, opts))
	}
	return slog.New(slog.NewJSONHandler(w, opts))
}

func connect(ctx context.Context, cfg *config.Config, log *slog.Logger, migrateFirst bool) (*pgxpool.Pool, error) {
	pool, err := database.Connect(ctx, cfg.Database.URL, cfg.Database.MaxConns, cfg.Database.ConnectTimeout, log)
	if err != nil {
		return nil, err
	}
	if migrateFirst {
		if err := database.Migrate(ctx, pool, log); err != nil {
			pool.Close()
			return nil, err
		}
	}
	return pool, nil
}

func connectRedis(ctx context.Context, cfg *config.Config, log *slog.Logger) (*redis.Client, error) {
	if cfg.Redis.URL == "" {
		return nil, nil
	}
	opts, err := redis.ParseURL(cfg.Redis.URL)
	if err != nil {
		return nil, fmt.Errorf("invalid redis URL: %w", err)
	}
	rdb := redis.NewClient(opts)
	deadline := time.Now().Add(cfg.Database.ConnectTimeout)
	for {
		err := rdb.Ping(ctx).Err()
		if err == nil {
			return rdb, nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			_ = rdb.Close()
			return nil, fmt.Errorf("could not connect to Redis at %s: %w (unset GOTALK_REDIS_URL to run without Redis)", opts.Addr, err)
		}
		log.Warn("waiting for Redis", "addr", opts.Addr, "error", err.Error())
		select {
		case <-time.After(2 * time.Second):
		case <-ctx.Done():
		}
	}
}

func serve(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	log.Info("starting gotalk", "version", version, "addr", cfg.Server.Addr)

	pool, err := connect(ctx, cfg, log, cfg.Database.AutoMigrate)
	if err != nil {
		return err
	}
	defer pool.Close()

	rdb, err := connectRedis(ctx, cfg, log)
	if err != nil {
		return err
	}
	if rdb != nil {
		defer func() { _ = rdb.Close() }()
	}

	limiter, err := ratelimit.New(rdb, cfg.RateLimit.Tiers())
	if err != nil {
		return err
	}
	if limiter.Backend() == "memory" {
		log.Info("rate limiting uses in-process memory; set GOTALK_REDIS_URL when running more than one replica")
	}

	svc, err := service.New(ctx, pool, cfg, log)
	if err != nil {
		return err
	}
	if err := autoSetup(ctx, svc, cfg, log); err != nil {
		return err
	}

	handler := api.New(api.Deps{
		Service: svc, Limiter: limiter, Redis: rdb, Config: cfg, Logger: log, Version: version,
	})
	srv := &http.Server{
		Addr:              cfg.Server.Addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	ln, err := net.Listen("tcp", cfg.Server.Addr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", cfg.Server.Addr, err)
	}
	log.Info("gotalk is listening", "addr", ln.Addr().String())

	if required, err := svc.SetupRequired(ctx); err == nil && required {
		announceSetup(ctx, svc, cfg, log)
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}

	log.Info("shutting down", "timeout", cfg.Server.ShutdownTimeout.String())
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	log.Info("shutdown complete")
	return nil
}

// autoSetup completes setup at boot when admin credentials were supplied via config,
// so cloud deployments never need a browser.
func autoSetup(ctx context.Context, svc *service.Service, cfg *config.Config, log *slog.Logger) error {
	if !cfg.Setup.HasHeadlessAdmin() {
		return nil
	}
	required, err := svc.SetupRequired(ctx)
	if err != nil || !required {
		return err
	}
	_, err = svc.CompleteSetupHeadless(ctx, setupInput(cfg))
	if errors.Is(err, service.ErrSetupAlreadyCompleted) {
		// Another replica finished setup first.
		return nil
	}
	if err != nil {
		return fmt.Errorf("headless setup failed: %w", err)
	}
	log.Info("headless setup completed from configuration", "admin", cfg.Setup.AdminUsername)
	return nil
}

func setupInput(cfg *config.Config) service.SetupInput {
	return service.SetupInput{
		InstanceName:        cfg.Setup.InstanceName,
		InstanceDescription: cfg.Setup.InstanceDescription,
		RegistrationMode:    cfg.Setup.RegistrationMode,
		AdminUsername:       cfg.Setup.AdminUsername,
		AdminEmail:          cfg.Setup.AdminEmail,
		AdminPassword:       cfg.Setup.AdminPassword,
	}
}

// announceSetup prints the wizard link prominently; structured logs alone are easy to
// miss in `docker compose logs`.
func announceSetup(ctx context.Context, svc *service.Service, cfg *config.Config, log *slog.Logger) {
	token, err := svc.SetupToken(ctx)
	if err != nil || token == "" {
		return
	}
	base := cfg.Server.PublicURL
	if base == "" {
		port := "8080"
		if _, p, err := net.SplitHostPort(cfg.Server.Addr); err == nil && p != "" {
			port = p
		}
		base = "http://localhost:" + port
	}
	link := base + "/setup?token=" + token
	log.Warn("instance setup required", "setup_url", link)
	fmt.Fprintf(os.Stderr, `
  ------------------------------------------------------------------------
    Gotalk needs to be set up. Open this link in your browser:

      %s

    Or set GOTALK_SETUP_ADMIN_USERNAME / _EMAIL / _PASSWORD and restart
    to finish setup without a browser.
  ------------------------------------------------------------------------

`, link)
}

func migrate(ctx context.Context, cfg *config.Config, log *slog.Logger, args []string) error {
	pool, err := connect(ctx, cfg, log, false)
	if err != nil {
		return err
	}
	defer pool.Close()

	if len(args) > 0 && args[0] == "status" {
		st, err := database.Status(ctx, pool)
		if err != nil {
			return err
		}
		fmt.Printf("current version: %d\nlatest version:  %d\n", st.Current, st.Latest)
		if st.Current < st.Latest {
			fmt.Println("pending migrations: run `gotalk migrate`")
		}
		return nil
	}
	if err := database.Migrate(ctx, pool, log); err != nil {
		return err
	}
	fmt.Println("database is up to date")
	return nil
}

func setup(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	if !cfg.Setup.HasHeadlessAdmin() {
		return errors.New("headless setup needs setup.admin_username, setup.admin_email and setup.admin_password " +
			"(or GOTALK_SETUP_ADMIN_USERNAME / GOTALK_SETUP_ADMIN_EMAIL / GOTALK_SETUP_ADMIN_PASSWORD)")
	}
	pool, err := connect(ctx, cfg, log, true)
	if err != nil {
		return err
	}
	defer pool.Close()

	svc, err := service.New(ctx, pool, cfg, log)
	if err != nil {
		return err
	}
	required, err := svc.SetupRequired(ctx)
	if err != nil {
		return err
	}
	if !required {
		fmt.Println("instance is already set up; nothing to do")
		return nil
	}
	settings, err := svc.CompleteSetupHeadless(ctx, setupInput(cfg))
	if err != nil {
		return err
	}
	fmt.Printf("setup complete: instance %q with administrator %q\n", settings.Name, cfg.Setup.AdminUsername)
	return nil
}

func healthcheck(cfg *config.Config) error {
	_, port, err := net.SplitHostPort(cfg.Server.Addr)
	if err != nil {
		return fmt.Errorf("parsing server.addr: %w", err)
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + port + "/healthz") //nolint:gosec // fixed loopback probe of our own port
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unhealthy: HTTP %d", resp.StatusCode)
	}
	return nil
}
