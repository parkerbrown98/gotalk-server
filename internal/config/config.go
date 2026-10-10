// Package config loads layered configuration: built-in defaults, then an optional
// YAML file, then environment variables.
//
// Environment variables use the GOTALK_ prefix, and the first underscore after the
// prefix separates the section from the key, e.g. GOTALK_DATABASE_URL -> database.url,
// GOTALK_AUTH_JWT_SECRET -> auth.jwt_secret. Appending _FILE to any variable reads the
// value from that file path instead (Docker/Kubernetes secrets convention). The common
// platform variables DATABASE_URL, REDIS_URL and PORT are honored as fallbacks. Empty
// environment variables are ignored, so Compose files can pass optional ones through.
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

	"github.com/parkerbrown98/gotalk-server/internal/mail"
	"github.com/parkerbrown98/gotalk-server/internal/storage"
)

const EnvPrefix = "GOTALK_"

type Config struct {
	Server    Server           `koanf:"server"`
	Database  Database         `koanf:"database"`
	Redis     Redis            `koanf:"redis"`
	Auth      Auth             `koanf:"auth"`
	RateLimit RateLimit        `koanf:"ratelimit"`
	Log       Log              `koanf:"log"`
	Setup     Setup            `koanf:"setup"`
	Voice     Voice            `koanf:"voice"`
	Webhooks  Webhooks         `koanf:"webhooks"`
	Storage   storage.Settings `koanf:"storage"`
	Mail      mail.Settings    `koanf:"mail"`
	Uploads   Uploads          `koanf:"uploads"`
	Embeds    Embeds           `koanf:"embeds"`

	// explicit holds the keys set by the config file or the environment (as opposed to
	// built-in defaults).
	explicit map[string]bool
}

// IsSet reports whether key (e.g. "mail.driver") was set by the config file or the
// environment rather than coming from a built-in default.
func (c *Config) IsSet(key string) bool { return c.explicit[key] }

// MarkSet records key as explicitly configured. Tests use it to simulate operator config.
func (c *Config) MarkSet(keys ...string) {
	if c.explicit == nil {
		c.explicit = map[string]bool{}
	}
	for _, k := range keys {
		c.explicit[k] = true
	}
}

// Uploads limits files users upload (avatars, icons and attachments).
type Uploads struct {
	// MaxSize is the largest accepted upload in bytes.
	MaxSize int64 `koanf:"max_size"`
}

// Embeds controls link previews: the server fetches links posted in messages and posts and
// stores their title, description and preview image.
type Embeds struct {
	Enabled bool `koanf:"enabled"`
	// AllowPrivateNetworks lets previews fetch loopback, private and link-local addresses. Leave it
	// off in production: it lets users make the server request internal services.
	AllowPrivateNetworks bool `koanf:"allow_private_networks"`
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
	// Content limits creating topics, posts and reports, to slow down spam.
	Content string `koanf:"content"`
	// Chat limits sending messages, reacting and typing indicators.
	Chat string `koanf:"chat"`
}

// Tiers maps rate limit tier names to their configured rates.
func (r RateLimit) Tiers() map[string]string {
	return map[string]string{"default": r.Default, "auth": r.Auth, "content": r.Content, "chat": r.Chat}
}

type Log struct {
	Level  string `koanf:"level"`
	Format string `koanf:"format"`
}

// Voice connects the instance to a LiveKit server, which routes voice and video. Voice is
// enabled when LiveKitURL is set.
type Voice struct {
	// LiveKitURL is the LiveKit URL clients connect to, e.g. wss://voice.example.com.
	LiveKitURL string `koanf:"livekit_url"`
	// LiveKitAPIURL is how this server reaches LiveKit's API when that differs from the
	// client URL (e.g. http://livekit:7880 inside a container network).
	LiveKitAPIURL    string `koanf:"livekit_api_url"`
	LiveKitAPIKey    string `koanf:"livekit_api_key"`
	LiveKitAPISecret string `koanf:"livekit_api_secret"`
	// TokenTTL bounds how long a join token can be used to connect.
	TokenTTL time.Duration `koanf:"token_ttl"`
	// JoinTimeout is how long a joined user has to connect to LiveKit before their voice
	// state is dropped.
	JoinTimeout time.Duration `koanf:"join_timeout"`
	// SessionRetention is how long ended voice sessions are kept for diagnostics.
	SessionRetention time.Duration `koanf:"session_retention"`
}

// Enabled reports whether a LiveKit server is configured.
func (v Voice) Enabled() bool { return v.LiveKitURL != "" }

// Webhooks controls delivery of place webhooks to external URLs.
type Webhooks struct {
	// AllowPrivateNetworks lets webhooks reach loopback, private and link-local addresses.
	// Keep it off unless every place manager is trusted, or webhooks can probe the
	// server's internal network.
	AllowPrivateNetworks bool `koanf:"allow_private_networks"`
	// Timeout bounds each delivery attempt.
	Timeout time.Duration `koanf:"timeout"`
	// DeliveryRetention is how long finished deliveries stay in the delivery log.
	DeliveryRetention time.Duration `koanf:"delivery_retention"`
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
		"server.shutdown_timeout":     "20s",
		"database.url":                localDB.String(),
		"database.max_conns":          20,
		"database.auto_migrate":       true,
		"database.connect_timeout":    "30s",
		"auth.access_token_ttl":       "15m",
		"auth.refresh_token_ttl":      "720h",
		"ratelimit.enabled":           true,
		"ratelimit.default":           "300-M",
		"ratelimit.auth":              "10-M",
		"ratelimit.content":           "30-M",
		"ratelimit.chat":              "120-M",
		"log.level":                   "info",
		"log.format":                  "json",
		"setup.instance_name":         "Gotalk",
		"setup.registration_mode":     "open",
		"voice.token_ttl":             "10m",
		"voice.join_timeout":          "60s",
		"voice.session_retention":     "720h",
		"webhooks.timeout":            "10s",
		"webhooks.delivery_retention": "168h",
		"storage.driver":              "local",
		"storage.local_path":          "data",
		"uploads.max_size":            8 << 20,
		"embeds.enabled":              true,
	}
}

// listKeys are split on commas when supplied through the environment.
var listKeys = map[string]bool{"server.cors_allowed_origins": true, "server.trusted_proxies": true}

// Load builds the configuration. configFile may be empty.
func Load(configFile string) (*Config, error) {
	return load(configFile, os.Environ)
}

func load(configFile string, environ func() []string) (*Config, error) {
	// Operator-supplied layers are loaded on their own first, so explicitly set keys can
	// be told apart from defaults.
	layers := koanf.New(".")

	if configFile != "" {
		if err := layers.Load(file.Provider(configFile), yaml.Parser()); err != nil {
			return nil, fmt.Errorf("loading config file %q: %w", configFile, err)
		}
	}

	platform := map[string]any{}
	for _, kv := range environ() {
		name, value, _ := strings.Cut(kv, "=")
		if value == "" {
			continue
		}
		switch name {
		case "DATABASE_URL":
			platform["database.url"] = value
		case "REDIS_URL":
			platform["redis.url"] = value
		case "PORT":
			platform["server.addr"] = ":" + value
		}
	}
	if err := layers.Load(confmap.Provider(platform, "."), nil); err != nil {
		return nil, fmt.Errorf("loading platform environment: %w", err)
	}

	var fileErr error
	envProvider := env.Provider(".", env.Opt{
		Prefix:      EnvPrefix,
		EnvironFunc: environ,
		TransformFunc: func(name, value string) (string, any) {
			key := strings.ToLower(strings.TrimPrefix(name, EnvPrefix))
			if strings.HasSuffix(key, "_file") {
				if value == "" {
					return "", nil
				}
				// Reading an operator-supplied path is the point of the _FILE convention.
				data, err := os.ReadFile(value) //nolint:gosec // path comes from trusted deployment config
				if err != nil {
					fileErr = errors.Join(fileErr, fmt.Errorf("reading %s: %w", name, err))
					return "", nil
				}
				key = strings.TrimSuffix(key, "_file")
				value = strings.TrimRight(string(data), "\r\n")
			}
			if value == "" {
				return "", nil
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
	if err := layers.Load(envProvider, nil); err != nil {
		return nil, fmt.Errorf("loading environment: %w", err)
	}
	if fileErr != nil {
		return nil, fileErr
	}

	k := koanf.New(".")
	if err := k.Load(confmap.Provider(defaults(), "."), nil); err != nil {
		return nil, fmt.Errorf("loading defaults: %w", err)
	}
	if err := k.Merge(layers); err != nil {
		return nil, fmt.Errorf("merging configuration: %w", err)
	}

	var cfg Config
	if err := k.UnmarshalWithConf("", &cfg, koanf.UnmarshalConf{Tag: "koanf"}); err != nil {
		return nil, fmt.Errorf("decoding config: %w", err)
	}
	for _, key := range layers.Keys() {
		if v := layers.Get(key); v != nil && v != "" {
			cfg.MarkSet(key)
		}
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
	if err := ValidateCORS(c.Server.CORSAllowedOrigins, c.Server.CORSAllowCredentials); err != nil {
		errs = append(errs, err)
	}
	if c.Voice.Enabled() {
		if err := ValidateVoice(c.Voice); err != nil {
			errs = append(errs, err)
		}
	}
	if _, err := storage.Open(c.Storage); err != nil {
		errs = append(errs, err)
	}
	if _, err := mail.Open(c.Mail, mail.Env{}); err != nil {
		errs = append(errs, err)
	}
	if c.Uploads.MaxSize < 64<<10 || c.Uploads.MaxSize > 100<<20 {
		add("uploads.max_size must be between 65536 (64 KiB) and 104857600 (100 MiB) bytes")
	}
	if c.Voice.TokenTTL < time.Minute || c.Voice.TokenTTL > 24*time.Hour {
		add("voice.token_ttl must be between 1m and 24h")
	}
	if c.Voice.JoinTimeout < 10*time.Second {
		add("voice.join_timeout must be at least 10s")
	}
	if c.Voice.SessionRetention < time.Hour {
		add("voice.session_retention must be at least 1h")
	}
	if c.Webhooks.Timeout < time.Second || c.Webhooks.Timeout > time.Minute {
		add("webhooks.timeout must be between 1s and 1m")
	}
	if c.Webhooks.DeliveryRetention < time.Hour {
		add("webhooks.delivery_retention must be at least 1h")
	}

	if len(errs) > 0 {
		return fmt.Errorf("invalid configuration:\n  %w", errors.Join(errs...))
	}
	return nil
}

// ValidateVoice checks LiveKit connection settings of an enabled voice section.
func ValidateVoice(v Voice) error {
	var errs []error
	for _, kv := range [][2]string{{"voice.livekit_url", v.LiveKitURL}, {"voice.livekit_api_url", v.LiveKitAPIURL}} {
		key, val := kv[0], kv[1]
		if val == "" {
			continue
		}
		u, err := url.Parse(val)
		if err != nil || u.Host == "" || (u.Scheme != "ws" && u.Scheme != "wss" && u.Scheme != "http" && u.Scheme != "https") {
			errs = append(errs, fmt.Errorf("%s must be an absolute ws, wss, http or https URL, got %q", key, val))
		}
	}
	if v.LiveKitAPIKey == "" {
		errs = append(errs, errors.New("voice.livekit_api_key is required when voice.livekit_url is set"))
	}
	if len(v.LiveKitAPISecret) < 32 {
		errs = append(errs, errors.New("voice.livekit_api_secret must be at least 32 characters when voice.livekit_url is set"))
	}
	return errors.Join(errs...)
}

// ValidateCORS checks allowed origins: "*", exact origins such as https://app.example.com,
// or origins with a single wildcard such as https://*.example.com. Custom schemes are
// allowed, because the desktop app's webview sends tauri://localhost (macOS and Linux).
func ValidateCORS(origins []string, allowCredentials bool) error {
	var errs []error
	for _, o := range origins {
		if o == "*" {
			if allowCredentials {
				errs = append(errs, errors.New("server.cors_allow_credentials cannot be combined with a wildcard origin"))
			}
			continue
		}
		if strings.Count(o, "*") > 1 {
			errs = append(errs, fmt.Errorf("server.cors_allowed_origins entry %q may contain at most one wildcard", o))
			continue
		}
		u, err := url.Parse(strings.Replace(o, "*", "wildcard", 1))
		if err != nil || u.Scheme == "" || u.Host == "" || (u.Path != "" && u.Path != "/") ||
			u.RawQuery != "" || u.Fragment != "" || strings.HasSuffix(o, "/") {
			errs = append(errs, fmt.Errorf("server.cors_allowed_origins entry %q must be an origin like https://app.example.com or tauri://localhost (scheme and host, no path or trailing slash)", o))
		}
	}
	return errors.Join(errs...)
}
