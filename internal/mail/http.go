package mail

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"net/url"
	"strings"
	"time"
)

const (
	httpTimeout        = 15 * time.Second
	maxProviderMessage = 200
)

var timeNow = time.Now

func requireFrom(s Settings, driver string) (*mail.Address, error) {
	from, err := s.FromAddress()
	if err != nil {
		return nil, fmt.Errorf("%s driver: %w", driver, err)
	}
	return from, nil
}

func requireSetting(value, key, driver string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("mail.%s is required for the %s driver", key, driver)
	}
	return nil
}

func apiBase(s Settings, defaultBase string) string {
	if strings.TrimSpace(s.APIURL) == "" {
		return defaultBase
	}
	return strings.TrimRight(strings.TrimSpace(s.APIURL), "/")
}

func newHTTPClient() *http.Client {
	return &http.Client{Timeout: httpTimeout}
}

// doRequest sends req and returns the status code and (at most 1 MiB of) the body.
func doRequest(client *http.Client, req *http.Request) (int, []byte, error) {
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, data, nil
}
func providerHTTPError(provider string, status int, body []byte, secrets ...string) error {
	msg := redactSecrets(providerMessage(body), secrets...)
	msg = truncateProviderMessage(msg)
	if msg == "" {
		msg = http.StatusText(status)
	}
	return fmt.Errorf("%s: provider returned HTTP %d: %s", provider, status, msg)
}

func redactSecrets(msg string, secrets ...string) string {
	for _, secret := range secrets {
		if secret != "" {
			msg = strings.ReplaceAll(msg, secret, "[redacted]")
		}
	}
	return msg
}

func truncateProviderMessage(msg string) string {
	if len(msg) > maxProviderMessage {
		return msg[:maxProviderMessage] + "..."
	}
	return msg
}

func providerMessage(body []byte) string {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err == nil {
		for _, key := range []string{"message", "Message", "error", "Error", "name"} {
			if v, ok := obj[key].(string); ok && strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		}
		if v, ok := obj["errors"].([]any); ok && len(v) > 0 {
			if m, ok := v[0].(map[string]any); ok {
				if text, ok := m["message"].(string); ok {
					return strings.TrimSpace(text)
				}
			}
		}
	}
	return strings.TrimSpace(string(body))
}

func mailAddressJSON(addr *mail.Address) map[string]string {
	out := map[string]string{"email": addr.Address}
	if addr.Name != "" {
		out["name"] = addr.Name
	}
	return out
}

func escapedPath(parts ...string) string {
	escaped := make([]string, len(parts))
	for i, p := range parts {
		escaped[i] = url.PathEscape(p)
	}
	return strings.Join(escaped, "/")
}
