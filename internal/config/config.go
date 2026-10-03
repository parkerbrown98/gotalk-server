// Package config loads layered configuration: built-in defaults, then an optional
// YAML file, then environment variables.
//
// Environment variables use the GOTALK_ prefix, and the first underscore after the
// prefix separates the section from the key, e.g. GOTALK_DATABASE_URL -> database.url,
// GOTALK_AUTH_JWT_SECRET -> auth.jwt_secret. Appending _FILE to any variable reads the
// value from that file path instead (Docker/Kubernetes secrets convention). The common
// platform variables DATABASE_URL, REDIS_URL and PORT are honored as fallbacks.
package config

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/providers/env/v2"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
)

const EnvPrefix = "GOTALK_"

type Config struct {
	Server    Server    `koanf:"server"`
	Database  Database  `koanf:"database"`
	Redis     Redis     `koanf:"redis"`
	Auth      Auth      `koanf:"auth"`
	RateLimit RateLimit `koanf:"ratelimit"`
	Log       Log       `koanf:"log"`
	Setup     Setup     `koanf:"setup"`
}

type Server struct {
	Addr string `koanf:"addr"`
	// PublicURL is the externally reachable base URL (e.g. https://forum.example.com).
	// When empty it is derived from each request.
	PublicURL string `koanf:"public_url"`
	// TrustProxy honors X-Forwarded-For / X-Forwarded-Proto / X-Forwarded-Host, but only
	// on requests whose direct peer is within TrustedProxies. Enable when running behind a
	// reverse proxy, ingress, or load balancer.
	TrustProxy bool `koanf:"trust_proxy"`
	// TrustedProxies lists CIDRs of proxies allowed to set forwarding headers. Defaults to
	// loopback and private ranges, which covers same-host proxies and in-cluster ingress.
	TrustedProxies       []string      `koanf:"trusted_proxies"`
	CORSAllowedOrigins   []string      `koanf:"cors_allowed_origins"`
	CORSAllowCredentials bool          `koanf:"cors_allow_credentials"`
	ShutdownTimeout      time.Duration `koanf:"shutdown_timeout"`
}

// TrustedProxyPrefixes parses TrustedProxies. Validate guarantees it succeeds.
func (s Server) TrustedProxyPrefixes() []netip.Prefix {
	out := make([]netip.Prefix, 0, len(s.TrustedProxies))
	for _, cidr := range s.TrustedProxies {
		if p, err := netip.ParsePrefix(cidr); err == nil {
			out = append(out, p)
		}
	}
	return out
}

type Database struct {
	URL         string `koanf:"url"`
	MaxConns    int32  `koanf:"max_conns"`
	AutoMigrate bool   `koanf:"auto_migrate"`
	// ConnectTimeout bounds how long startup waits for PostgreSQL to become reachable.
	ConnectTimeout time.Duration `koanf:"connect_timeout"`
}

type Redis struct {
	// URL is optional. Without it, rate limiting falls back to per-process memory.
	URL string `koanf:"url"`
}

type Auth struct {
	// JWTSecret is optional. Without it, a random secret is generated once and stored in
	// the database so every replica shares it.
	JWTSecret       string        `koanf:"jwt_secret"`
	AccessTokenTTL  time.Duration `koanf:"access_token_ttl"`
	RefreshTokenTTL time.Duration `koanf:"refresh_token_ttl"`
}

type RateLimit struct {
	Enabled bool `koanf:"enabled"`
	// Rates use the "<limit>-<period>" format, where period is S, M, H or D (e.g. "300-M").
	Default string `koanf:"default"`
	Auth    string `koanf:"auth"`
}

type Log struct {
	Level  string `koanf:"level"`
	Format string `koanf:"format"`
}

// Setup holds values for headless (non-interactive) first-run setup. When the admin
// fields are all present and the instance is not yet configured, setup completes
// automatically at boot.
type Setup struct {
	// Token overrides the generated one-time token required by the browser wizard.
	Token               string `koanf:"token"`
	InstanceName        string `koanf:"instance_name"`
	InstanceDescription string `koanf:"instance_description"`
	RegistrationMode    string `koanf:"registration_mode"`
	AdminUsername       string `koanf:"admin_username"`
	AdminEmail          string `koanf:"admin_email"`
	AdminPassword       string `koanf:"admin_password"`
}

// HasHeadlessAdmin reports whether enough values were supplied to run setup without a browser.
func (s Setup) HasHeadlessAdmin() bool {
	return s.AdminUsername != "" && s.AdminEmail != "" && s.AdminPassword != ""
}

func defaults() map[string]any {
	localDB := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword("gotalk", "gotalk"),
		Host:     "localhost:5432",
		Path:     "/gotalk",
		RawQuery: "sslmode=disable",
	}
	return map[string]any{
		"server.addr":                 ":8080",
		"server.cors_allowed_origins": []string{"*"},
		"server.trusted_proxies": []string{
			"127.0.0.0/8", "::1/128", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "fc00::/7",
		},
		"server.shutdown_timeout":  "20s",
		"database.url":             localDB.String(),
		"database.max_conns":       20,
		"database.auto_migrate":    true,
		"database.connect_timeout": "30s",
		"auth.access_token_ttl":    "15m",
		"auth.refresh_token_ttl":   "720h",
		"ratelimit.enabled":        true,
		"ratelimit.default":        "300-M",
		"ratelimit.auth":           "10-M",
		"log.level":                "info",
		"log.format":               "json",
		"setup.instance_name":      "Gotalk",
		"setup.registration_mode":  "open",
	}
}

// listKeys are split on commas when supplied through the environment.
var listKeys = map[string]bool{"server.cors_allowed_origins": true, "server.trusted_proxies": true}

// Load builds the configuration. configFile may be empty.
func Load(configFile string) (*Config, error) {
	return load(configFile, os.Environ)
}

func load(configFile string, environ func() []string) (*Config, error) {
	k := koanf.New(".")

	if err := k.Load(confmap.Provider(defaults(), "."), nil); err != nil {
		return nil, fmt.Errorf("loading defaults: %w", err)
	}

	if configFile != "" {
		if err := k.Load(file.Provider(configFile), yaml.Parser()); err != nil {
			return nil, fmt.Errorf("loading config file %q: %w", configFile, err)
		}
	}

	platform := map[string]any{}
	for _, kv := range environ() {
		name, value, _ := strings.Cut(kv, "=")
		switch name {
		case "DATABASE_URL":
			platform["database.url"] = value
		case "REDIS_URL":
			platform["redis.url"] = value
		case "PORT":
			platform["server.addr"] = ":" + value
		}
	}
	if err := k.Load(confmap.Provider(platform, "."), nil); err != nil {
		return nil, fmt.Errorf("loading platform environment: %w", err)
	}

	var fileErr error
	envProvider := env.Provider(".", env.Opt{
		Prefix:      EnvPrefix,
		EnvironFunc: environ,
		TransformFunc: func(name, value string) (string, any) {
			key := strings.ToLower(strings.TrimPrefix(name, EnvPrefix))
			if strings.HasSuffix(key, "_file") {
				// Reading an operator-supplied path is the point of the _FILE convention.
				data, err := os.ReadFile(value) //nolint:gosec // path comes from trusted deployment config
				if err != nil {
					fileErr = errors.Join(fileErr, fmt.Errorf("reading %s: %w", name, err))
					return "", nil
				}
				key = strings.TrimSuffix(key, "_file")
				value = strings.TrimRight(string(data), "\r\n")
			}
			section, rest, ok := strings.Cut(key, "_")
			if !ok {
				return "", nil
			}
			key = section + "." + rest
			if listKeys[key] {
				return key, splitList(value)
			}
			return key, value
		},
	})
	if err := k.Load(envProvider, nil); err != nil {
		return nil, fmt.Errorf("loading environment: %w", err)
	}
	if fileErr != nil {
		return nil, fileErr
	}

	var cfg Config
	if err := k.UnmarshalWithConf("", &cfg, koanf.UnmarshalConf{Tag: "koanf"}); err != nil {
		return nil, fmt.Errorf("decoding config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func splitList(v string) []string {
	var out []string
	for _, part := range strings.Split(v, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Validate reports every invalid setting at once so operators can fix them in one pass.
func (c *Config) Validate() error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if c.Server.Addr == "" {
		add("server.addr must not be empty")
	}
	for _, cidr := range c.Server.TrustedProxies {
		if _, err := netip.ParsePrefix(cidr); err != nil {
			add("server.trusted_proxies contains an invalid CIDR %q", cidr)
		}
	}
	if c.Server.PublicURL != "" {
		u, err := url.Parse(c.Server.PublicURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			add("server.public_url must be an absolute http(s) URL, got %q", c.Server.PublicURL)
		}
		c.Server.PublicURL = strings.TrimRight(c.Server.PublicURL, "/")
	}
	if c.Database.URL == "" {
		add("database.url must not be empty (set GOTALK_DATABASE_URL)")
	}
	if c.Database.MaxConns < 1 {
		add("database.max_conns must be at least 1")
	}
	if c.Auth.JWTSecret != "" && len(c.Auth.JWTSecret) < 32 {
		add("auth.jwt_secret must be at least 32 characters when set")
	}
	if c.Auth.AccessTokenTTL < time.Minute {
		add("auth.access_token_ttl must be at least 1m")
	}
	if c.Auth.RefreshTokenTTL < c.Auth.AccessTokenTTL {
		add("auth.refresh_token_ttl must be longer than auth.access_token_ttl")
	}
	switch c.Setup.RegistrationMode {
	case "open", "invite_only", "closed":
	default:
		add("setup.registration_mode must be one of open, invite_only, closed")
	}
	switch c.Log.Format {
	case "json", "text":
	default:
		add("log.format must be json or text")
	}
	if c.Server.CORSAllowCredentials {
		for _, o := range c.Server.CORSAllowedOrigins {
			if o == "*" {
				add("server.cors_allow_credentials cannot be combined with a wildcard origin")
			}
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("invalid configuration:\n  %w", errors.Join(errs...))
	}
	return nil
}
