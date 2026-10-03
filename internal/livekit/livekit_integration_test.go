package livekit

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// TestAgainstLiveKit checks the wire format against a real LiveKit server.
func TestAgainstLiveKit(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a LiveKit container")
	}
	testcontainers.SkipIfProviderIsNotHealthy(t)
	ctx := context.Background()
	ctr, err := testcontainers.Run(ctx, "livekit/livekit-server:v1.13",
		testcontainers.WithEnv(map[string]string{"LIVEKIT_KEYS": testKey + ": " + testSecret}),
		testcontainers.WithCmd("--dev", "--bind", "0.0.0.0"),
		testcontainers.WithExposedPorts("7880/tcp"),
		testcontainers.WithWaitStrategy(wait.ForHTTP("/").WithPort("7880/tcp").WithStartupTimeout(60*time.Second)),
	)
	testcontainers.CleanupContainer(t, ctr)
	require.NoError(t, err)
	endpoint, err := ctr.PortEndpoint(ctx, "7880/tcp", "http")
	require.NoError(t, err)

	c, err := New(endpoint, "", testKey, testSecret)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(c.URL(), "ws://"))

	require.NoError(t, c.Ping(ctx))
	ps, err := c.ListParticipants(ctx, "nobody-here")
	require.NoError(t, err)
	require.Empty(t, ps)
	require.NoError(t, c.RemoveParticipant(ctx, "nobody-here", "u1"))
	require.NoError(t, c.UpdatePermission(ctx, "nobody-here", "u1", Permission{CanSubscribe: true, Sources: []Source{SourceMicrophone, SourceScreenShare}}))
	require.NoError(t, c.DeleteRoom(ctx, "nobody-here"))

	// LiveKit validates participant tokens the same way it does when a client connects.
	token, _, err := c.ParticipantToken(TokenOptions{
		Identity: "u1", Name: "User One", Room: "room-1", TTL: time.Minute,
		Permission: Permission{CanSubscribe: true, Sources: []Source{SourceMicrophone}},
	})
	require.NoError(t, err)
	res, err := http.Get(endpoint + "/rtc/validate?access_token=" + url.QueryEscape(token))
	require.NoError(t, err)
	body, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	require.Equal(t, http.StatusOK, res.StatusCode, string(body))

	// A different secret is rejected.
	wrong, err := New(endpoint, "", testKey, strings.Repeat("y", 40))
	require.NoError(t, err)
	require.Error(t, wrong.Ping(ctx))
}
