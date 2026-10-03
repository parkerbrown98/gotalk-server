package api

import (
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/parkerbrown98/gotalk-server/internal/config"
)

func proxyServer(t *testing.T, trust bool) *Server {
	t.Helper()
	cfg, err := config.Load("")
	require.NoError(t, err)
	cfg.Server.TrustProxy = trust
	cfg.Server.PublicURL = ""
	return &Server{Deps: Deps{Config: cfg}, proxies: cfg.Server.TrustedProxyPrefixes()}
}

func TestClientIPResolution(t *testing.T) {
	cases := []struct {
		name     string
		trust    bool
		peer     string
		xff      string
		wantIP   string
		viaProxy bool
	}{
		{"trust disabled ignores headers", false, "10.0.0.2:5000", "198.51.100.7", "10.0.0.2", false},
		{"untrusted peer cannot spoof", true, "203.0.113.5:5000", "198.51.100.7", "203.0.113.5", false},
		{"right-most untrusted hop wins over forged left entries", true, "10.0.0.2:5000", "1.2.3.4, 198.51.100.7", "198.51.100.7", true},
		{"internal proxy hops are skipped", true, "10.0.0.2:5000", "198.51.100.7, 10.0.0.9", "198.51.100.7", true},
		{"trusted peer without header", true, "127.0.0.1:5000", "", "127.0.0.1", true},
		{"garbage stops the walk", true, "10.0.0.2:5000", "198.51.100.7, not-an-ip", "10.0.0.2", true},
		{"ipv6 peer", true, "[::1]:5000", "2001:db8::1", "2001:db8::1", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := proxyServer(t, tc.trust)
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = tc.peer
			if tc.xff != "" {
				r.Header.Set("X-Forwarded-For", tc.xff)
			}
			ip, via := s.clientIP(r)
			assert.Equal(t, tc.wantIP, ip)
			assert.Equal(t, tc.viaProxy, via)
		})
	}
}

func TestBaseURLHonorsForwardedHeadersOnlyViaProxy(t *testing.T) {
	s := proxyServer(t, true)
	r := httptest.NewRequest("GET", "http://internal:8080/", nil)
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("X-Forwarded-Host", "forum.example.com")

	assert.Equal(t, "https://forum.example.com", s.baseURL(r, true))
	assert.Equal(t, "http://internal:8080", s.baseURL(r, false))

	s.Config.Server.PublicURL = "https://configured.example"
	assert.Equal(t, "https://configured.example", s.baseURL(r, true))
}
