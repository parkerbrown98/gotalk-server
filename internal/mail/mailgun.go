package mail

import (
	"context"
	"fmt"
	"net/http"
	"net/mail"
	"net/url"
	"strings"
)

const mailgunBaseURL = "https://api.mailgun.net"

type mailgunSender struct {
	apiKey string
	base   string
	domain string
	from   *mail.Address
	client *http.Client
}

func init() {
	Register("mailgun", newMailgunSender)
}

func newMailgunSender(s Settings, _ Env) (Sender, error) {
	if err := requireSetting(s.APIKey, "api_key", "mailgun"); err != nil {
		return nil, err
	}
	if err := requireSetting(s.Domain, "domain", "mailgun"); err != nil {
		return nil, err
	}
	from, err := requireFrom(s, "mailgun")
	if err != nil {
		return nil, err
	}
	return &mailgunSender{
		apiKey: s.APIKey,
		base:   apiBase(s, mailgunBaseURL),
		domain: s.Domain,
		from:   from,
		client: newHTTPClient(),
	}, nil
}

func (s *mailgunSender) Driver() string {
	return "mailgun"
}

func (s *mailgunSender) Describe() string {
	return "mailgun " + s.domain
}

func (s *mailgunSender) Send(ctx context.Context, msg Message) error {
	to, err := validateMessage("mailgun", msg)
	if err != nil {
		return err
	}
	form := url.Values{}
	form.Set("from", s.from.String())
	form.Set("to", to.Address)
	form.Set("subject", msg.Subject)
	form.Set("text", msg.Text)
	if msg.HTML != "" {
		form.Set("html", msg.HTML)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.base+"/v3/"+escapedPath(s.domain)+"/messages", strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("mailgun: build request: %w", err)
	}
	req.SetBasicAuth("api", s.apiKey)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	status, data, err := doRequest(s.client, req)
	if err != nil {
		return fmt.Errorf("mailgun: request failed: %w", err)
	}
	if status < http.StatusOK || status >= http.StatusMultipleChoices {
		return providerHTTPError("mailgun", status, data, s.apiKey)
	}
	return nil
}

func (s *mailgunSender) Check(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.base+"/v3/domains/"+escapedPath(s.domain), http.NoBody)
	if err != nil {
		return fmt.Errorf("mailgun: build check request: %w", err)
	}
	req.SetBasicAuth("api", s.apiKey)
	status, data, err := doRequest(s.client, req)
	if err != nil {
		return fmt.Errorf("mailgun: check request failed: %w", err)
	}
	if status < http.StatusOK || status >= http.StatusMultipleChoices {
		return providerHTTPError("mailgun", status, data, s.apiKey)
	}
	return nil
}
