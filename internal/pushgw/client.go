package pushgw

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Gateway error codes the server reacts to (gateway docs/protocol.md, "Errors").
const (
	CodeInvalidSignature     = "invalid_signature"
	CodeUnknownKey           = "unknown_key"
	CodeStaleTimestamp       = "stale_timestamp"
	CodeReplayedNonce        = "replayed_nonce"
	CodeInstanceNotVerified  = "instance_not_verified"
	CodeInstanceBlocked      = "instance_blocked"
	CodeInstanceNotFound     = "instance_not_found"
	CodeVerificationFailed   = "verification_failed"
	CodeSubscriptionNotFound = "subscription_not_found"
	CodeSubscriptionGone     = "subscription_gone"
	CodeRateLimited          = "rate_limited"
	CodeQuotaExceeded        = "quota_exceeded"
	CodeProviderUnavailable  = "provider_unavailable"
	CodeInternalError        = "internal_error"
)

// Instance statuses.
const (
	StatusPending   = "pending"
	StatusActive    = "active"
	StatusSuspended = "suspended"
	StatusBlocked   = "blocked"
)

// Push modes and urgencies.
const (
	ModeEncrypted = "encrypted"
	ModeIDOnly    = "id_only"

	UrgencyLow    = "low"
	UrgencyNormal = "normal"
	UrgencyHigh   = "high"
)

// MaxBatch is the most entries one POST /v1/push/batch accepts.
const MaxBatch = 100

// Error is a gateway problem response, or a response that was not one.
type Error struct {
	Status int
	// Code is the gateway's machine-readable code; empty when the response was not a
	// problem document (a proxy error page, say).
	Code   string
	Detail string
	// RetryAfter is the gateway's Retry-After, if any.
	RetryAfter time.Duration
}

func (e *Error) Error() string {
	msg := fmt.Sprintf("push gateway answered %d", e.Status)
	if e.Code != "" {
		msg += " " + e.Code
	}
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	return msg
}

// Temporary reports whether the request may succeed if retried later as it is.
func (e *Error) Temporary() bool {
	switch e.Code {
	case CodeRateLimited, CodeQuotaExceeded, CodeProviderUnavailable, CodeInternalError, CodeReplayedNonce, CodeStaleTimestamp:
		return true
	}
	return e.Status == http.StatusTooManyRequests || e.Status >= 500 || e.Status == 0
}

// AsError returns err as a gateway *Error, if it is one.
func AsError(err error) (*Error, bool) {
	var e *Error
	ok := errors.As(err, &e)
	return e, ok
}

// Credentials sign requests: the gateway's key ID and the instance's private key.
type Credentials struct {
	KeyID string
	Key   ed25519.PrivateKey
}

// Client calls one gateway. It is safe for concurrent use.
type Client struct {
	base      string
	http      *http.Client
	userAgent string
	now       func() time.Time
}

// Options configure a Client.
type Options struct {
	// HTTPClient overrides the default client (15 s timeout, no redirects).
	HTTPClient *http.Client
	UserAgent  string
	Now        func() time.Time
}

// New returns a client for the gateway at baseURL (for example https://push.gotalk.sh).
func New(baseURL string, o Options) *Client {
	c := &Client{base: strings.TrimRight(baseURL, "/"), http: o.HTTPClient, userAgent: o.UserAgent, now: o.Now}
	if c.http == nil {
		c.http = &http.Client{
			Timeout:       15 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	if c.userAgent == "" {
		c.userAgent = "gotalk-server"
	}
	if c.now == nil {
		c.now = time.Now
	}
	return c
}

// BaseURL is the gateway's base URL, without a trailing slash.
func (c *Client) BaseURL() string { return c.base }

// do sends a request and decodes a 2xx JSON body into out (when not nil). creds signs it.
func (c *Client) do(ctx context.Context, method, path string, in, out any, creds *Credentials) error {
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if creds != nil {
		SignHTTP(req, body, creds.KeyID, creds.Key, c.now())
	}
	res, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("reaching the push gateway: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("reading the push gateway's response: %w", err)
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return problemOf(res, data, c.now())
	}
	if out == nil || res.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("the push gateway's response is not the expected JSON: %w", err)
	}
	return nil
}

func problemOf(res *http.Response, data []byte, now time.Time) *Error {
	e := &Error{Status: res.StatusCode, RetryAfter: retryAfter(res.Header, now)}
	if mt, _, _ := mime.ParseMediaType(res.Header.Get("Content-Type")); mt == "application/problem+json" || mt == "application/json" {
		var p struct {
			Code   string `json:"code"`
			Detail string `json:"detail"`
			Title  string `json:"title"`
		}
		if json.Unmarshal(data, &p) == nil {
			e.Code, e.Detail = p.Code, p.Detail
			if e.Detail == "" {
				e.Detail = p.Title
			}
		}
	}
	if e.Detail == "" {
		e.Detail = http.StatusText(res.StatusCode)
	}
	return e
}

func retryAfter(h http.Header, now time.Time) time.Duration {
	v := h.Get("Retry-After")
	if v == "" {
		return 0
	}
	if s, err := strconv.Atoi(v); err == nil && s > 0 {
		return time.Duration(s) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil && t.After(now) {
		return t.Sub(now)
	}
	return 0
}

// Discovery is the gateway's discovery document.
type Discovery struct {
	Software    string   `json:"software"`
	Name        string   `json:"name"`
	Version     string   `json:"version"`
	APIVersions []string `json:"api_versions"`
	APIBaseURL  string   `json:"api_base_url"`
	Platforms   []string `json:"platforms"`
	Apps        []App    `json:"apps"`
	Limits      struct {
		MaxPlaintextBytes     int `json:"max_plaintext_bytes"`
		MaxEncryptedBodyBytes int `json:"max_encrypted_body_bytes"`
		MaxBatchSize          int `json:"max_batch_size"`
	} `json:"limits"`
	TermsURL   string `json:"terms_url,omitempty"`
	PrivacyURL string `json:"privacy_url,omitempty"`
}

// App is an app the gateway delivers for.
type App struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Platforms struct {
		IOS *struct {
			BundleID string `json:"bundle_id"`
		} `json:"ios,omitempty"`
		Android *struct {
			PackageName string `json:"package_name"`
		} `json:"android,omitempty"`
		Web *struct {
			VAPIDPublicKey string `json:"vapid_public_key"`
		} `json:"web,omitempty"`
	} `json:"platforms"`
}

// Discover fetches the discovery document and checks it is a v1 Gotalk gateway.
func (c *Client) Discover(ctx context.Context) (*Discovery, error) {
	var d Discovery
	if err := c.do(ctx, http.MethodGet, "/.well-known/gotalk-gateway", nil, &d, nil); err != nil {
		return nil, err
	}
	if d.Software != "gotalk-gateway" {
		return nil, fmt.Errorf("%s is not a Gotalk push gateway (software %q)", c.base, d.Software)
	}
	for _, v := range d.APIVersions {
		if v == "v1" {
			return &d, nil
		}
	}
	return nil, fmt.Errorf("the push gateway at %s does not serve API v1 (it serves %s)", c.base, strings.Join(d.APIVersions, ", "))
}

// RegisterInput registers an instance origin and key.
type RegisterInput struct {
	Origin          string `json:"origin"`
	PublicKey       string `json:"public_key"`
	ContactEmail    string `json:"contact_email"`
	Software        string `json:"software,omitempty"`
	SoftwareVersion string `json:"software_version,omitempty"`
}

// Registration is the answer to a registration: the instance and key IDs.
type Registration struct {
	InstanceID string `json:"instance_id"`
	KeyID      string `json:"key_id"`
	Status     string `json:"status"`
}

// Register registers the instance (or starts recovery of a verified origin with a new key).
func (c *Client) Register(ctx context.Context, in RegisterInput) (*Registration, error) {
	var r Registration
	if err := c.do(ctx, http.MethodPost, "/v1/instances", in, &r, nil); err != nil {
		return nil, err
	}
	return &r, nil
}

// Instance is the gateway's view of the instance.
type Instance struct {
	InstanceID   string  `json:"instance_id"`
	Origin       string  `json:"origin"`
	Status       string  `json:"status"`
	StatusReason *string `json:"status_reason"`
	Keys         []struct {
		KeyID     string `json:"key_id"`
		PublicKey string `json:"public_key"`
		Status    string `json:"status"`
	} `json:"keys"`
	VerifiedAt *time.Time `json:"verified_at"`
	Usage      *struct {
		PushesAccepted      int64 `json:"pushes_accepted"`
		PushesFailed        int64 `json:"pushes_failed"`
		ActiveSubscriptions int64 `json:"active_subscriptions"`
	} `json:"usage,omitempty"`
}

// KeyStatus returns the status of keyID, or "" when the gateway does not list it.
func (i *Instance) KeyStatus(keyID string) string {
	for _, k := range i.Keys {
		if k.KeyID == keyID {
			return k.Status
		}
	}
	return ""
}

// Verify asks the gateway to fetch the instance's well-known document now.
func (c *Client) Verify(ctx context.Context, creds Credentials, instanceID string) (*Instance, error) {
	var i Instance
	if err := c.do(ctx, http.MethodPost, "/v1/instances/"+url.PathEscape(instanceID)+"/verify", nil, &i, &creds); err != nil {
		return nil, err
	}
	return &i, nil
}

// GetInstance returns the instance's status, keys and usage.
func (c *Client) GetInstance(ctx context.Context, creds Credentials, instanceID string) (*Instance, error) {
	var i Instance
	if err := c.do(ctx, http.MethodGet, "/v1/instances/"+url.PathEscape(instanceID), nil, &i, &creds); err != nil {
		return nil, err
	}
	return &i, nil
}

// Push is one push request.
type Push struct {
	SubscriptionID string `json:"subscription_id"`
	Mode           string `json:"mode"`
	NotificationID string `json:"notification_id"`
	// Payload is the base64url RFC 8291 body (encrypted mode only).
	Payload     string `json:"payload,omitempty"`
	Urgency     string `json:"urgency,omitempty"`
	TTL         *int   `json:"ttl,omitempty"`
	CollapseKey string `json:"collapse_key,omitempty"`
}

// Result is the outcome of one batch entry.
type Result struct {
	Index          int    `json:"index"`
	Status         string `json:"status"`
	SubscriptionID string `json:"subscription_id"`
	PushID         string `json:"push_id,omitempty"`
	Error          *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// PushBatch queues up to MaxBatch pushes. Per-entry rejections are in the results; an error
// means the whole request failed.
func (c *Client) PushBatch(ctx context.Context, creds Credentials, pushes []Push) ([]Result, error) {
	if len(pushes) == 0 || len(pushes) > MaxBatch {
		return nil, fmt.Errorf("a batch holds 1 to %d pushes, not %d", MaxBatch, len(pushes))
	}
	var out struct {
		Results []Result `json:"results"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/push/batch", map[string]any{"pushes": pushes}, &out, &creds); err != nil {
		return nil, err
	}
	if len(out.Results) != len(pushes) {
		return nil, fmt.Errorf("the push gateway returned %d results for %d pushes", len(out.Results), len(pushes))
	}
	return out.Results, nil
}

// DeleteDevice deletes a subscription bound to this instance. A subscription the gateway
// does not know (already deleted, or never bound to this instance) is not an error.
func (c *Client) DeleteDevice(ctx context.Context, creds Credentials, subscriptionID string) error {
	err := c.do(ctx, http.MethodDelete, "/v1/devices/"+url.PathEscape(subscriptionID), nil, nil, &creds)
	if e, ok := AsError(err); ok && e.Code == CodeSubscriptionNotFound {
		return nil
	}
	return err
}
