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
	"github.com/parkerbrown98/gotalk-server/internal/backup"
	"github.com/parkerbrown98/gotalk-server/internal/config"
	"github.com/parkerbrown98/gotalk-server/internal/database"
	"github.com/parkerbrown98/gotalk-server/internal/ratelimit"
	"github.com/parkerbrown98/gotalk-server/internal/service"
	"github.com/parkerbrown98/gotalk-server/internal/storage"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `Gotalk server

Usage:
  gotalk [command] [flags]

Commands:
  serve        Run the server (default)
  migrate      Apply database migrations, or "migrate status" to show the schema version
  setup        Complete first-run setup headlessly from config/environment, then exit.
                 --reset           on a configured instance, re-apply the setup.* values
                                   (instance name/description/registration mode that are set,
                                   and the admin account: created, or promoted with its
                                   password reset and sessions revoked)
                 --reset-settings  forget storage/email/voice/CORS settings saved in the wizard
                 --skip-checks     complete setup even if pre-flight checks fail
  check        Run pre-flight checks (database, Redis, storage, email, voice); exit 1 on errors
  backup       Write a backup archive (database + media).
                 --output FILE     archive path, or - for stdout (default gotalk-backup-<time>.tar.gz)
                 --no-media        database only
                 --upload          store the archive in the storage backend under backups/
                 --keep N          with --upload, keep only the N newest uploaded backups
  backup list  List backups stored in the storage backend
  restore      Restore a backup archive into an empty database (stop the server first).
                 --input FILE      archive path, or - for stdin
                 --from-storage NAME  restore backups/NAME from the storage backend
                 --force           replace an existing instance's data
                 --no-media        database only
  healthcheck  Exit 0 if the local server is healthy (for container HEALTHCHECK)
  version      Print the version

All commands accept --config FILE (YAML). Configuration is read from built-in defaults,
then the config file, then GOTALK_* environment variables. See the README for every option.
`

type flags struct {
	reset, resetSettings, skipChecks bool
	output, input, fromStorage       string
	noMedia, upload, force           bool
	keep                             int
}

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
	var f flags
	fs.BoolVar(&f.reset, "reset", false, "re-apply setup values to a configured instance")
	fs.BoolVar(&f.resetSettings, "reset-settings", false, "forget settings saved through the wizard")
	fs.BoolVar(&f.skipChecks, "skip-checks", false, "complete setup even if checks fail")
	fs.StringVar(&f.output, "output", "", "backup archive path, or - for stdout")
	fs.StringVar(&f.input, "input", "", "archive to restore, or - for stdin")
	fs.StringVar(&f.fromStorage, "from-storage", "", "restore backups/NAME from the storage backend")
	fs.BoolVar(&f.noMedia, "no-media", false, "skip media files")
	fs.BoolVar(&f.upload, "upload", false, "store the backup in the storage backend")
	fs.BoolVar(&f.force, "force", false, "replace existing data when restoring")
	fs.IntVar(&f.keep, "keep", 0, "with --upload, keep only this many uploaded backups")
	// Allow "backup list" style subcommands before the flags.
	sub := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	positional := fs.Args()
	if sub != "" {
		positional = append([]string{sub}, positional...)
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
	// Commands other than serve write their own output to stdout, so keep logs on stderr.
	log := newLogger(cfg.Log, os.Stderr)

	switch cmd {
	case "serve":
		return serve(ctx, cfg, log)
	case "migrate":
		return migrate(ctx, cfg, log, positional)
	case "setup":
		return setup(ctx, cfg, log, f)
	case "check", "preflight":
		return check(ctx, cfg, log)
	case "backup":
		if len(positional) > 0 && positional[0] == "list" {
			return backupList(ctx, cfg, log)
		}
		return runBackup(ctx, cfg, log, f)
	case "restore":
		return runRestore(ctx, cfg, log, f)
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

	handler, err := api.New(api.Deps{
		Service: svc, Limiter: limiter, Redis: rdb, Config: cfg, Logger: log, Version: version,
	})
	if err != nil {
		return err
	}
	defer handler.Close()
	srv := &http.Server{
		Addr:              cfg.Server.Addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	// WebSocket connections are hijacked, so Shutdown does not drain them; close them
	// (code 1001, "going away") so clients reconnect to another replica.
	srv.RegisterOnShutdown(handler.Close)

	ln, err := net.Listen("tcp", cfg.Server.Addr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", cfg.Server.Addr, err)
	}
	log.Info("gotalk is listening", "addr", ln.Addr().String())

	if required, err := svc.SetupRequired(ctx); err == nil && required {
		announceSetup(ctx, svc, cfg, log)
	}
	go logPreflight(ctx, svc, rdb, log)

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

func setup(ctx context.Context, cfg *config.Config, log *slog.Logger, f flags) error {
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
		return reapplySetup(ctx, svc, cfg, f)
	}
	if !cfg.Setup.HasHeadlessAdmin() {
		return errors.New("headless setup needs setup.admin_username, setup.admin_email and setup.admin_password " +
			"(or GOTALK_SETUP_ADMIN_USERNAME / GOTALK_SETUP_ADMIN_EMAIL / GOTALK_SETUP_ADMIN_PASSWORD)")
	}

	ping, closeRedis := redisPinger(cfg)
	defer closeRedis()
	checks := svc.Preflight(ctx, ping)
	failed := printChecks(os.Stdout, checks)
	if failed && !f.skipChecks {
		return errors.New("pre-flight checks failed; fix the errors above (or pass --skip-checks)")
	}
	settings, err := svc.CompleteSetupHeadless(ctx, setupInput(cfg))
	if err != nil {
		return err
	}
	fmt.Printf("setup complete: instance %q with administrator %q\n", settings.Name, cfg.Setup.AdminUsername)
	return nil
}

// reapplySetup handles `gotalk setup` on a configured instance: a no-op unless --reset or
// --reset-settings is given.
func reapplySetup(ctx context.Context, svc *service.Service, cfg *config.Config, f flags) error {
	if !f.reset && !f.resetSettings {
		fmt.Println("instance is already set up; nothing to do (use --reset to re-apply setup values or --reset-settings to forget wizard settings)")
		return nil
	}
	if f.resetSettings {
		n, err := svc.ResetAllSettings(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("forgot %d saved settings section(s); config file, environment and defaults apply\n", n)
	}
	if !f.reset {
		return nil
	}
	in := service.ReapplyInput{}
	if cfg.IsSet("setup.instance_name") {
		in.InstanceName = &cfg.Setup.InstanceName
	}
	if cfg.IsSet("setup.instance_description") {
		in.InstanceDescription = &cfg.Setup.InstanceDescription
	}
	if cfg.IsSet("setup.registration_mode") {
		in.RegistrationMode = &cfg.Setup.RegistrationMode
	}
	if cfg.Setup.AdminUsername != "" {
		if cfg.Setup.AdminPassword == "" {
			return errors.New("setup.admin_password is required to reset the administrator")
		}
		in.AdminUsername, in.AdminEmail, in.AdminPassword = cfg.Setup.AdminUsername, cfg.Setup.AdminEmail, cfg.Setup.AdminPassword
	}
	res, err := svc.ReapplySetup(ctx, in)
	if err != nil {
		return err
	}
	fmt.Printf("re-applied setup values: instance %q, registration %s\n", res.Settings.Name, res.Settings.RegistrationMode)
	switch {
	case res.AdminCreated:
		fmt.Printf("created administrator %q\n", in.AdminUsername)
	case res.AdminReset:
		fmt.Printf("administrator %q: password reset, sessions and personal access tokens revoked\n", in.AdminUsername)
	}
	return nil
}

func redisPinger(cfg *config.Config) (func(context.Context) error, func()) {
	if cfg.Redis.URL == "" {
		return nil, func() {}
	}
	opts, err := redis.ParseURL(cfg.Redis.URL)
	if err != nil {
		return func(context.Context) error { return fmt.Errorf("invalid redis URL: %w", err) }, func() {}
	}
	rdb := redis.NewClient(opts)
	return func(ctx context.Context) error { return rdb.Ping(ctx).Err() }, func() { _ = rdb.Close() }
}

// printChecks writes one line per check (plus hints) and reports whether any failed.
func printChecks(w io.Writer, checks []service.Check) bool {
	failed := false
	for _, c := range checks {
		mark := map[string]string{service.CheckOK: "ok", service.CheckWarning: "WARN", service.CheckError: "FAIL", service.CheckSkipped: "--"}[c.Status]
		_, _ = fmt.Fprintf(w, "[%-4s] %-10s %s\n", mark, c.Name, c.Detail)
		if c.Hint != "" && c.Status != service.CheckOK {
			_, _ = fmt.Fprintf(w, "       %-10s -> %s\n", "", c.Hint)
		}
		failed = failed || c.Status == service.CheckError
	}
	return failed
}

func check(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	pool, err := connect(ctx, cfg, log, false)
	if err != nil {
		printChecks(os.Stdout, []service.Check{{Name: "database", Status: service.CheckError, Detail: err.Error(),
			Hint: "check GOTALK_DATABASE_URL and that PostgreSQL is running"}})
		return errors.New("pre-flight checks failed")
	}
	defer pool.Close()
	st, err := database.Status(ctx, pool)
	if err != nil {
		return err
	}
	if st.Current < st.Latest {
		printChecks(os.Stdout, []service.Check{{Name: "database", Status: service.CheckError,
			Detail: fmt.Sprintf("schema version %d, latest is %d", st.Current, st.Latest), Hint: "run `gotalk migrate`"}})
		return errors.New("pre-flight checks failed")
	}
	svc, err := service.New(ctx, pool, cfg, log)
	if err != nil {
		return err
	}
	ping, closeRedis := redisPinger(cfg)
	defer closeRedis()
	if printChecks(os.Stdout, svc.Preflight(ctx, ping)) {
		return errors.New("pre-flight checks failed")
	}
	return nil
}

// logPreflight logs one line per dependency at boot, so problems show up as clear,
// actionable messages instead of later runtime errors.
func logPreflight(ctx context.Context, svc *service.Service, rdb *redis.Client, log *slog.Logger) {
	var ping func(context.Context) error
	if rdb != nil {
		ping = func(ctx context.Context) error { return rdb.Ping(ctx).Err() }
	}
	for _, c := range svc.Preflight(ctx, ping) {
		attrs := []any{"check", c.Name, "detail", c.Detail}
		if c.Hint != "" && c.Status != service.CheckOK {
			attrs = append(attrs, "hint", c.Hint)
		}
		switch c.Status {
		case service.CheckError:
			log.Error("pre-flight check failed", attrs...)
		case service.CheckWarning:
			log.Warn("pre-flight check warning", attrs...)
		default:
			log.Info("pre-flight check", append(attrs, "status", c.Status)...)
		}
	}
}

const backupPrefix = "backups/"

func openStorage(ctx context.Context, cfg *config.Config, log *slog.Logger) (storage.Backend, *pgxpool.Pool, error) {
	pool, err := connect(ctx, cfg, log, false)
	if err != nil {
		return nil, nil, err
	}
	svc, err := service.New(ctx, pool, cfg, log)
	if err != nil {
		pool.Close()
		return nil, nil, err
	}
	p := svc.Providers()
	if p.StorageErr != nil {
		return nil, pool, fmt.Errorf("storage is misconfigured: %w", p.StorageErr)
	}
	return p.Storage, pool, nil
}

func runBackup(ctx context.Context, cfg *config.Config, log *slog.Logger, f flags) error {
	b, pool, err := openStorage(ctx, cfg, log)
	if pool != nil {
		defer pool.Close()
	}
	if err != nil {
		return err
	}
	name := "gotalk-backup-" + time.Now().UTC().Format("20060102-150405") + ".tar.gz"
	opts := backup.Options{Version: version, Storage: b, MediaPrefixes: service.MediaPrefixes(), Logger: log}
	if f.noMedia {
		opts.Storage = nil
	}

	var out io.Writer
	var file *os.File
	switch {
	case f.upload:
		// The archive's size must be known before uploading, so write a temporary file first.
		if file, err = os.CreateTemp("", "gotalk-backup-*.tar.gz"); err != nil {
			return err
		}
		defer func() { _ = file.Close(); _ = os.Remove(file.Name()) }()
		out = file
	case f.output == "-":
		out = os.Stdout
	default:
		path := f.output
		if path == "" {
			path = name
		}
		if file, err = os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600); err != nil { //nolint:gosec // operator-chosen path
			return err
		}
		defer func() { _ = file.Close() }()
		out, name = file, path
	}

	m, counts, err := backup.Write(ctx, pool, out, opts)
	if err != nil {
		if file != nil && !f.upload {
			_ = file.Close()
			_ = os.Remove(name)
		}
		return err
	}
	var rows int64
	for _, n := range counts.Rows {
		rows += n
	}
	summary := fmt.Sprintf("%d tables, %d rows, %d media files (%d bytes), schema version %d", len(m.Tables), rows, counts.MediaFiles, counts.MediaBytes, m.SchemaVersion)

	if f.upload {
		size, err := file.Seek(0, io.SeekCurrent)
		if err != nil {
			return err
		}
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return err
		}
		if b == nil {
			return errors.New("--upload needs a working storage backend")
		}
		if err := b.Put(ctx, backupPrefix+name, file, size, "application/gzip"); err != nil {
			return fmt.Errorf("uploading the backup: %w", err)
		}
		fmt.Fprintf(os.Stderr, "backup uploaded to %s as %s%s: %s\n", b.Describe(), backupPrefix, name, summary)
		if f.keep > 0 {
			return pruneBackups(ctx, b, f.keep)
		}
		return nil
	}
	if f.output == "-" {
		fmt.Fprintln(os.Stderr, "backup written to stdout:", summary)
		return nil
	}
	if err := file.Sync(); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "backup written to %s: %s\n", name, summary)
	return nil
}

func listBackups(ctx context.Context, b storage.Backend) ([]storage.Object, error) {
	var out []storage.Object
	err := b.List(ctx, backupPrefix, func(o storage.Object) error {
		if strings.HasSuffix(o.Key, ".tar.gz") {
			out = append(out, o)
		}
		return nil
	})
	return out, err
}

// pruneBackups deletes all but the newest keep backups. Names embed the UTC time, so
// lexical order is chronological.
func pruneBackups(ctx context.Context, b storage.Backend, keep int) error {
	list, err := listBackups(ctx, b)
	if err != nil {
		return err
	}
	for i := 0; i < len(list)-keep; i++ {
		if err := b.Delete(ctx, list[i].Key); err != nil {
			return fmt.Errorf("deleting old backup %s: %w", list[i].Key, err)
		}
		fmt.Fprintln(os.Stderr, "deleted old backup", list[i].Key)
	}
	return nil
}

func backupList(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	b, pool, err := openStorage(ctx, cfg, log)
	if pool != nil {
		defer pool.Close()
	}
	if err != nil {
		return err
	}
	list, err := listBackups(ctx, b)
	if err != nil {
		return err
	}
	if len(list) == 0 {
		fmt.Println("no backups in", b.Describe())
		return nil
	}
	for _, o := range list {
		fmt.Printf("%s\t%d bytes\n", strings.TrimPrefix(o.Key, backupPrefix), o.Size)
	}
	return nil
}

// spool copies r into a temporary file so it can be read twice.
func spool(r io.Reader) (*os.File, func(), error) {
	f, err := os.CreateTemp("", "gotalk-restore-*.tar.gz")
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { _ = f.Close(); _ = os.Remove(f.Name()) }
	if _, err := io.Copy(f, r); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("reading the backup: %w", err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		cleanup()
		return nil, nil, err
	}
	return f, cleanup, nil
}

func runRestore(ctx context.Context, cfg *config.Config, log *slog.Logger, f flags) error {
	if (f.input == "") == (f.fromStorage == "") {
		return errors.New("restore needs exactly one of --input FILE (or - for stdin) and --from-storage NAME")
	}
	// Restore must not run migrations or create instance settings before loading data.
	pool, err := connect(ctx, cfg, log, false)
	if err != nil {
		return err
	}
	defer pool.Close()

	var in io.ReadSeeker
	switch {
	case f.fromStorage != "":
		// The archive is read from the storage configured in the file/environment, since
		// the database (and any settings saved in it) is about to be replaced.
		b, err := storage.Open(cfg.Storage)
		if err != nil {
			return fmt.Errorf("storage is misconfigured: %w", err)
		}
		rc, _, err := b.Get(ctx, backupPrefix+strings.TrimPrefix(f.fromStorage, backupPrefix))
		if err != nil {
			return fmt.Errorf("opening the backup: %w", err)
		}
		defer func() { _ = rc.Close() }()
		spooled, cleanup, err := spool(rc)
		if err != nil {
			return err
		}
		defer cleanup()
		in = spooled
	case f.input == "-":
		// The archive is verified before restoring, which needs a second read.
		spooled, cleanup, err := spool(os.Stdin)
		if err != nil {
			return err
		}
		defer cleanup()
		in = spooled
	default:
		file, err := os.Open(f.input)
		if err != nil {
			return err
		}
		defer func() { _ = file.Close() }()
		in = file
	}
	opts := backup.RestoreOptions{Force: f.force, Logger: log}
	if !f.noMedia {
		opts.MediaTarget = func(ctx context.Context) (storage.Backend, error) {
			svc, err := service.New(ctx, pool, cfg, log)
			if err != nil {
				return nil, err
			}
			p := svc.Providers()
			if p.StorageErr != nil {
				return nil, p.StorageErr
			}
			return p.Storage, nil
		}
	}
	m, counts, err := backup.Restore(ctx, pool, in, opts)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "restored backup from %s (gotalk %s, schema version %d): %d tables, %d media files\n",
		m.CreatedAt.Format(time.RFC3339), m.GotalkVersion, m.SchemaVersion, len(counts.Rows), counts.MediaFiles)
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
