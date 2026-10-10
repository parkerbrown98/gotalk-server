// Package mail sends email through pluggable drivers. Drivers register themselves by name
// (like database/sql drivers). Built in: "smtp", the HTTP APIs of "sendgrid", "mailgun",
// "postmark", "resend" and "ses" (Amazon SES v2), and "log", which writes messages to the
// server log instead of sending them (for development).
package mail

import (
	"context"
	"fmt"
	"log/slog"
	"net/mail"
	"slices"
	"strings"
	"sync"
)

// Message is one email. Text is required; HTML is optional.
type Message struct {
	To      string
	Subject string
	Text    string
	HTML    string
}

// Sender is a mail driver instance.
type Sender interface {
	// Driver returns the registered driver name.
	Driver() string
	// Describe returns a short, credential-free description, e.g. "smtp smtp.example.com:587".
	Describe() string
	// Send delivers msg from the configured sender address.
	Send(ctx context.Context, msg Message) error
	// Check verifies connectivity and credentials without sending mail, where the
	// provider allows it.
	Check(ctx context.Context) error
}

// Settings configures email delivery. Every field can be set in the config file, as a
// GOTALK_MAIL_* environment variable, or through the setup wizard. Fields that do not
// apply to the chosen driver are ignored.
type Settings struct {
	// Driver selects the provider: smtp, sendgrid, mailgun, postmark, resend, ses or log.
	// Empty disables email.
	Driver string `koanf:"driver" json:"driver" doc:"Mail driver; empty disables email"`
	// From is the sender, e.g. "Gotalk <noreply@forum.example.com>".
	From string `koanf:"from" json:"from,omitempty" doc:"Sender address, e.g. Gotalk <noreply@forum.example.com>"`

	SMTPHost     string `koanf:"smtp_host" json:"smtp_host,omitempty"`
	SMTPPort     int    `koanf:"smtp_port" json:"smtp_port,omitempty" doc:"Defaults to 587 (starttls), 465 (tls) or 25 (none)"`
	SMTPUsername string `koanf:"smtp_username" json:"smtp_username,omitempty"`
	SMTPPassword string `koanf:"smtp_password" json:"smtp_password,omitempty" doc:"Write-only; leave empty to keep the current value"`
	// SMTPTLS is "starttls" (default), "tls" (implicit TLS, usually port 465) or "none".
	SMTPTLS string `koanf:"smtp_tls" json:"smtp_tls,omitempty" enum:"starttls,tls,none," doc:"starttls (default), tls (implicit, port 465) or none"`

	// APIKey authenticates with HTTP API providers (SendGrid, Mailgun, Postmark server
	// token, Resend).
	APIKey string `koanf:"api_key" json:"api_key,omitempty" doc:"Provider API key or server token; write-only"`
	// APIURL overrides the provider's API base URL, e.g. https://api.eu.mailgun.net for
	// Mailgun's EU region.
	APIURL string `koanf:"api_url" json:"api_url,omitempty" doc:"Override the provider's API base URL (e.g. https://api.eu.mailgun.net)"`
	// Domain is the Mailgun sending domain.
	Domain string `koanf:"domain" json:"domain,omitempty" doc:"Mailgun sending domain"`

	// Region, AccessKeyID and SecretAccessKey configure Amazon SES.
	Region          string `koanf:"region" json:"region,omitempty" doc:"Amazon SES region"`
	AccessKeyID     string `koanf:"access_key_id" json:"access_key_id,omitempty" doc:"Amazon SES access key ID"`
	SecretAccessKey string `koanf:"secret_access_key" json:"secret_access_key,omitempty" doc:"Amazon SES secret access key; write-only"`

	// Options carries settings for third-party drivers.
	Options map[string]string `koanf:"options" json:"options,omitempty"`
}

// Enabled reports whether a driver is selected.
func (s Settings) Enabled() bool { return s.Driver != "" }

// Redacted returns a copy with secrets removed, safe to show to administrators.
func (s Settings) Redacted() Settings {
	s.SMTPPassword, s.APIKey, s.SecretAccessKey = "", "", ""
	return s
}

// SecretsSet lists the secret fields that hold a value.
func (s Settings) SecretsSet() []string {
	out := []string{}
	if s.SMTPPassword != "" {
		out = append(out, "smtp_password")
	}
	if s.APIKey != "" {
		out = append(out, "api_key")
	}
	if s.SecretAccessKey != "" {
		out = append(out, "secret_access_key")
	}
	return out
}

// KeepSecrets fills empty secret fields from prev, so callers can update settings
// without re-entering secrets.
func (s Settings) KeepSecrets(prev Settings) Settings {
	if s.SMTPPassword == "" {
		s.SMTPPassword = prev.SMTPPassword
	}
	if s.APIKey == "" {
		s.APIKey = prev.APIKey
	}
	if s.SecretAccessKey == "" {
		s.SecretAccessKey = prev.SecretAccessKey
	}
	return s
}

// FromAddress parses From.
func (s Settings) FromAddress() (*mail.Address, error) {
	if strings.TrimSpace(s.From) == "" {
		return nil, fmt.Errorf("mail.from is required (e.g. \"Gotalk <noreply@forum.example.com>\")")
	}
	addr, err := mail.ParseAddress(s.From)
	if err != nil {
		return nil, fmt.Errorf("mail.from %q is not a valid address: %w", s.From, err)
	}
	return addr, nil
}

// Env carries process-level dependencies drivers may use.
type Env struct {
	// Logger is used by the log driver.
	Logger *slog.Logger
}

// Factory builds a sender from settings. It should validate the settings but must not
// perform network calls; use Sender.Check for that.
type Factory func(Settings, Env) (Sender, error)

var (
	mu      sync.RWMutex
	drivers = map[string]Factory{}
)

// Register makes a driver available by name. It panics if the name is taken.
func Register(name string, f Factory) {
	mu.Lock()
	defer mu.Unlock()
	if _, dup := drivers[name]; dup {
		panic("mail: driver registered twice: " + name)
	}
	drivers[name] = f
}

// Drivers lists the registered driver names, sorted.
func Drivers() []string {
	mu.RLock()
	defer mu.RUnlock()
	names := make([]string, 0, len(drivers))
	for n := range drivers {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

// Open builds a sender. It returns (nil, nil) when no driver is selected.
func Open(s Settings, env Env) (Sender, error) {
	if !s.Enabled() {
		return nil, nil
	}
	mu.RLock()
	f, ok := drivers[s.Driver]
	mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown mail driver %q (available: %s)", s.Driver, strings.Join(Drivers(), ", "))
	}
	if env.Logger == nil {
		env.Logger = slog.Default()
	}
	return f(s, env)
}
