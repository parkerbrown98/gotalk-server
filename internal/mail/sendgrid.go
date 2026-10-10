package mail

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/mail"
	"slices"
)

const sendgridBaseURL = "https://api.sendgrid.com"

type sendgridSender struct {
	apiKey string
	base   string
	from   *mail.Address
	client *http.Client
}

func init() {
	Register("sendgrid", newSendgridSender)
}

func newSendgridSender(s Settings, _ Env) (Sender, error) {
	if err := requireSetting(s.APIKey, "api_key", "sendgrid"); err != nil {
		return nil, err
	}
	from, err := requireFrom(s, "sendgrid")
	if err != nil {
		return nil, err
	}
	return &sendgridSender{apiKey: s.APIKey, base: apiBase(s, sendgridBaseURL), from: from, client: newHTTPClient()}, nil
}

func (s *sendgridSender) Driver() string {
	return "sendgrid"
}

func (s *sendgridSender) Describe() string {
	return "sendgrid " + s.base
}

func (s *sendgridSender) Send(ctx context.Context, msg Message) error {
	to, err := validateMessage("sendgrid", msg)
	if err != nil {
		return err
	}
	content := []map[string]string{{"type": "text/plain", "value": msg.Text}}
	if msg.HTML != "" {
		content = append(content, map[string]string{"type": "text/html", "value": msg.HTML})
	}
	body := map[string]any{
		"personalizations": []map[string]any{{"to": []map[string]string{mailAddressJSON(to)}}},
		"from":             mailAddressJSON(s.from),
		"subject":          msg.Subject,
		"content":          content,
	}
	reqBody, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("sendgrid: encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.base+"/v3/mail/send", bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("sendgrid: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+s.apiKey)
	req.Header.Set("Content-Type", "application/json")
	status, data, err := doRequest(s.client, req)
	if err != nil {
		return fmt.Errorf("sendgrid: request failed: %w", err)
	}
	if status != http.StatusAccepted {
		return providerHTTPError("sendgrid", status, data, s.apiKey)
	}
	return nil
}

func (s *sendgridSender) Check(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.base+"/v3/scopes", http.NoBody)
	if err != nil {
		return fmt.Errorf("sendgrid: build check request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+s.apiKey)
	status, data, err := doRequest(s.client, req)
	if err != nil {
		return fmt.Errorf("sendgrid: check request failed: %w", err)
	}
	if status != http.StatusOK {
		return providerHTTPError("sendgrid", status, data, s.apiKey)
	}
	var parsed struct {
		Scopes []string `json:"scopes"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return fmt.Errorf("sendgrid: parse scopes response: %w", err)
	}
	if !slices.Contains(parsed.Scopes, "mail.send") {
		return fmt.Errorf("sendgrid: api key lacks Mail Send permission (missing mail.send scope)")
	}
	return nil
}
