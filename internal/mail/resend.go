package mail

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/mail"
)

const resendBaseURL = "https://api.resend.com"

type resendSender struct {
	apiKey string
	base   string
	from   *mail.Address
	client *http.Client
}

func init() {
	Register("resend", newResendSender)
}

func newResendSender(s Settings, _ Env) (Sender, error) {
	if err := requireSetting(s.APIKey, "api_key", "resend"); err != nil {
		return nil, err
	}
	from, err := requireFrom(s, "resend")
	if err != nil {
		return nil, err
	}
	return &resendSender{apiKey: s.APIKey, base: apiBase(s, resendBaseURL), from: from, client: newHTTPClient()}, nil
}

func (s *resendSender) Driver() string {
	return "resend"
}

func (s *resendSender) Describe() string {
	return "resend " + s.base
}

func (s *resendSender) Send(ctx context.Context, msg Message) error {
	to, err := validateMessage("resend", msg)
	if err != nil {
		return err
	}
	body := map[string]any{
		"from":    s.from.String(),
		"to":      []string{to.Address},
		"subject": msg.Subject,
		"text":    msg.Text,
	}
	if msg.HTML != "" {
		body["html"] = msg.HTML
	}
	reqBody, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("resend: encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.base+"/emails", bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("resend: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+s.apiKey)
	req.Header.Set("Content-Type", "application/json")
	status, data, err := doRequest(s.client, req)
	if err != nil {
		return fmt.Errorf("resend: request failed: %w", err)
	}
	if status < http.StatusOK || status >= http.StatusMultipleChoices {
		return providerHTTPError("resend", status, data, s.apiKey)
	}
	return nil
}

func (s *resendSender) Check(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.base+"/domains", http.NoBody)
	if err != nil {
		return fmt.Errorf("resend: build check request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+s.apiKey)
	status, data, err := doRequest(s.client, req)
	if err != nil {
		return fmt.Errorf("resend: check request failed: %w", err)
	}
	if status == http.StatusOK || (status == http.StatusUnauthorized && responseName(data) == "restricted_api_key") {
		return nil
	}
	return providerHTTPError("resend", status, data, s.apiKey)
}

func responseName(body []byte) string {
	var parsed struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(body, &parsed)
	return parsed.Name
}
