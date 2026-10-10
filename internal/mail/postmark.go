package mail

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/mail"
)

const postmarkBaseURL = "https://api.postmarkapp.com"

type postmarkSender struct {
	apiKey string
	base   string
	from   *mail.Address
	client *http.Client
}

type postmarkResponse struct {
	ErrorCode int    `json:"ErrorCode"`
	Message   string `json:"Message"`
}

func init() {
	Register("postmark", newPostmarkSender)
}

func newPostmarkSender(s Settings, _ Env) (Sender, error) {
	if err := requireSetting(s.APIKey, "api_key", "postmark"); err != nil {
		return nil, err
	}
	from, err := requireFrom(s, "postmark")
	if err != nil {
		return nil, err
	}
	return &postmarkSender{apiKey: s.APIKey, base: apiBase(s, postmarkBaseURL), from: from, client: newHTTPClient()}, nil
}

func (s *postmarkSender) Driver() string {
	return "postmark"
}

func (s *postmarkSender) Describe() string {
	return "postmark " + s.base
}

func (s *postmarkSender) Send(ctx context.Context, msg Message) error {
	if _, err := validateMessage("postmark", msg); err != nil {
		return err
	}
	body := map[string]any{
		"From":          s.from.String(),
		"To":            msg.To,
		"Subject":       msg.Subject,
		"TextBody":      msg.Text,
		"HtmlBody":      msg.HTML,
		"MessageStream": "outbound",
	}
	reqBody, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("postmark: encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.base+"/email", bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("postmark: build request: %w", err)
	}
	req.Header.Set("X-Postmark-Server-Token", s.apiKey)
	req.Header.Set("Content-Type", "application/json")
	status, data, err := doRequest(s.client, req)
	if err != nil {
		return fmt.Errorf("postmark: request failed: %w", err)
	}
	var parsed postmarkResponse
	if len(data) > 0 {
		_ = json.Unmarshal(data, &parsed)
	}
	if status != http.StatusOK {
		return providerHTTPError("postmark", status, data, s.apiKey)
	}
	if parsed.ErrorCode != 0 {
		msg := truncateProviderMessage(redactSecrets(parsed.Message, s.apiKey))
		return fmt.Errorf("postmark: provider returned HTTP %d: %s", status, msg)
	}
	return nil
}

func (s *postmarkSender) Check(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.base+"/server", http.NoBody)
	if err != nil {
		return fmt.Errorf("postmark: build check request: %w", err)
	}
	req.Header.Set("X-Postmark-Server-Token", s.apiKey)
	status, data, err := doRequest(s.client, req)
	if err != nil {
		return fmt.Errorf("postmark: check request failed: %w", err)
	}
	if status != http.StatusOK {
		return providerHTTPError("postmark", status, data, s.apiKey)
	}
	return nil
}
