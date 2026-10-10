package mail

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/mail"

	"github.com/parkerbrown98/gotalk-server/internal/sigv4"
)

type sesSender struct {
	accessKeyID     string
	secretAccessKey string
	region          string
	base            string
	from            *mail.Address
	client          *http.Client
}

func init() {
	Register("ses", newSESSender)
}

func newSESSender(s Settings, _ Env) (Sender, error) {
	if err := requireSetting(s.Region, "region", "ses"); err != nil {
		return nil, err
	}
	if err := requireSetting(s.AccessKeyID, "access_key_id", "ses"); err != nil {
		return nil, err
	}
	if err := requireSetting(s.SecretAccessKey, "secret_access_key", "ses"); err != nil {
		return nil, err
	}
	from, err := requireFrom(s, "ses")
	if err != nil {
		return nil, err
	}
	base := apiBase(s, "https://email."+s.Region+".amazonaws.com")
	return &sesSender{
		accessKeyID:     s.AccessKeyID,
		secretAccessKey: s.SecretAccessKey,
		region:          s.Region,
		base:            base,
		from:            from,
		client:          newHTTPClient(),
	}, nil
}

func (s *sesSender) Driver() string {
	return "ses"
}

func (s *sesSender) Describe() string {
	return "ses " + s.region
}

func (s *sesSender) Send(ctx context.Context, msg Message) error {
	to, err := validateMessage("ses", msg)
	if err != nil {
		return err
	}
	body := map[string]any{
		"FromEmailAddress": s.from.String(),
		"Destination": map[string]any{
			"ToAddresses": []string{to.Address},
		},
		"Content": map[string]any{
			"Simple": map[string]any{
				"Subject": map[string]string{"Data": msg.Subject, "Charset": "UTF-8"},
				"Body": map[string]any{
					"Text": map[string]string{"Data": msg.Text, "Charset": "UTF-8"},
				},
			},
		},
	}
	if msg.HTML != "" {
		body["Content"].(map[string]any)["Simple"].(map[string]any)["Body"].(map[string]any)["Html"] = map[string]string{"Data": msg.HTML, "Charset": "UTF-8"}
	}
	reqBody, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("ses: encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.base+"/v2/email/outbound-emails", bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("ses: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	s.sign(req, reqBody)
	status, data, err := doRequest(s.client, req)
	if err != nil {
		return fmt.Errorf("ses: request failed: %w", err)
	}
	if status < http.StatusOK || status >= http.StatusMultipleChoices {
		return providerHTTPError("ses", status, data, s.accessKeyID, s.secretAccessKey)
	}
	return nil
}

func (s *sesSender) Check(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.base+"/v2/email/account", http.NoBody)
	if err != nil {
		return fmt.Errorf("ses: build check request: %w", err)
	}
	s.sign(req, nil)
	status, data, err := doRequest(s.client, req)
	if err != nil {
		return fmt.Errorf("ses: check request failed: %w", err)
	}
	if status != http.StatusOK {
		return providerHTTPError("ses", status, data, s.accessKeyID, s.secretAccessKey)
	}
	return nil
}

func (s *sesSender) sign(req *http.Request, body []byte) {
	signer := sigv4.Signer{
		Credentials: sigv4.Credentials{
			AccessKeyID:     s.accessKeyID,
			SecretAccessKey: s.secretAccessKey,
		},
		Region:  s.region,
		Service: "ses",
	}
	signer.Sign(req, sigv4.HashPayload(body), timeNow())
}
