package service

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/parkerbrown98/gotalk-server/internal/apperr"
	"github.com/parkerbrown98/gotalk-server/internal/config"
)

func TestPublicAddress(t *testing.T) {
	for addr, want := range map[string]bool{
		"8.8.8.8":         true,
		"2606:4700::1111": true,
		"127.0.0.1":       false,
		"10.0.0.5":        false,
		"172.16.0.1":      false,
		"192.168.1.1":     false,
		"169.254.169.254": false,
		"100.64.0.1":      false,
		"0.0.0.0":         false,
		"::1":             false,
		"fe80::1":         false,
		"fd00::1":         false,
		"::ffff:10.0.0.1": false,
		"224.0.0.1":       false,
		"198.18.0.1":      false,
	} {
		require.Equal(t, want, publicAddress(netip.MustParseAddr(addr)), addr)
	}
}

func TestWebhookClientRefusesPrivateAddresses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer srv.Close()

	guarded := newWebhookClient(config.Webhooks{Timeout: 2 * time.Second})
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	require.NoError(t, err)
	_, err = guarded.Do(req) //nolint:bodyclose // the request must fail
	var blocked *blockedAddressError
	require.True(t, errors.As(err, &blocked), "got %v", err)

	open := newWebhookClient(config.Webhooks{Timeout: 2 * time.Second, AllowPrivateNetworks: true})
	res, err := open.Do(req)
	require.NoError(t, err)
	require.NoError(t, res.Body.Close())
	require.Equal(t, http.StatusOK, res.StatusCode)
}

func TestWebhookClientDoesNotFollowRedirects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/", http.StatusFound)
	}))
	defer srv.Close()
	client := newWebhookClient(config.Webhooks{Timeout: 2 * time.Second, AllowPrivateNetworks: true})
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL, nil)
	require.NoError(t, err)
	res, err := client.Do(req)
	require.NoError(t, err)
	require.NoError(t, res.Body.Close())
	require.Equal(t, http.StatusFound, res.StatusCode)
}

func TestSignWebhook(t *testing.T) {
	body := []byte(`{"type":"ping"}`)
	mac := hmac.New(sha256.New, []byte("whsec_test"))
	mac.Write([]byte("1700000000." + string(body)))
	require.Equal(t, "t=1700000000,v1="+hex.EncodeToString(mac.Sum(nil)), SignWebhook("whsec_test", 1700000000, body))
	require.NotEqual(t, SignWebhook("whsec_test", 1700000000, body), SignWebhook("whsec_other", 1700000000, body))
}

func TestNormalizeScopes(t *testing.T) {
	got, err := normalizeScopes([]string{"gateway", "read", "read"})
	require.NoError(t, err)
	require.Equal(t, []string{"read", "gateway"}, got)
	_, err = normalizeScopes(nil)
	require.Error(t, err)
	_, err = normalizeScopes([]string{"root"})
	require.Error(t, err)
}

func TestInteractionOptions(t *testing.T) {
	defs := []CommandOption{
		{Name: "count", Type: "integer", Required: true},
		{Name: "ratio", Type: "number"},
		{Name: "loud", Type: "boolean"},
		{Name: "who", Type: "user"},
		{Name: "note", Type: "string"},
	}
	ok, err := interactionOptions(defs, map[string]any{
		"count": 3.0, "ratio": 0.5, "loud": true, "who": "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b", "note": "hi",
	})
	require.NoError(t, err)
	require.Len(t, ok, 5)

	invalid := func(opts map[string]any) {
		t.Helper()
		_, err := interactionOptions(defs, opts)
		var ae *apperr.Error
		require.True(t, errors.As(err, &ae) && ae.Kind == apperr.KindInvalid, "%v: %v", opts, err)
	}
	invalid(map[string]any{})
	invalid(map[string]any{"count": 1.5})
	invalid(map[string]any{"count": "3"})
	invalid(map[string]any{"count": 1.0, "loud": "yes"})
	invalid(map[string]any{"count": 1.0, "who": "bob"})
	invalid(map[string]any{"count": 1.0, "extra": 1.0})
}

func TestValidateCommands(t *testing.T) {
	require.NoError(t, validateCommands([]CommandInput{{Name: "roll", Description: "Roll dice",
		Options: []CommandOption{{Name: "sides", Description: "Sides", Type: "integer"}}}}))
	require.Error(t, validateCommands([]CommandInput{{Name: "Roll", Description: "x"}}))
	require.Error(t, validateCommands([]CommandInput{{Name: "a", Description: "x"}, {Name: "a", Description: "y"}}))
	require.Error(t, validateCommands([]CommandInput{{Name: "a", Description: "x",
		Options: []CommandOption{{Name: "o", Description: "d", Type: "date"}}}}))
	require.Error(t, validateCommands([]CommandInput{{Name: "a", Description: ""}}))
}
