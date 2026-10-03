package livekit

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/require"
)

const (
	testKey    = "testkey"
	testSecret = "0123456789abcdef0123456789abcdef-test"
)

func parse(t *testing.T, token, secret string) jwt.MapClaims {
	t.Helper()
	cl := jwt.MapClaims{}
	_, err := jwt.ParseWithClaims(token, cl, func(*jwt.Token) (any, error) { return []byte(secret), nil },
		jwt.WithValidMethods([]string{"HS256"}))
	require.NoError(t, err)
	return cl
}

func TestNewNormalizesURLs(t *testing.T) {
	c, err := New("https://voice.example.com/", "", testKey, testSecret)
	require.NoError(t, err)
	require.Equal(t, "wss://voice.example.com", c.URL())
	require.Equal(t, "https://voice.example.com", c.apiURL)

	c, err = New("ws://localhost:7880", "http://livekit:7880", testKey, testSecret)
	require.NoError(t, err)
	require.Equal(t, "ws://localhost:7880", c.URL())
	require.Equal(t, "http://livekit:7880", c.apiURL)

	_, err = New("ftp://x", "", testKey, testSecret)
	require.Error(t, err)
	_, err = New("wss://x", "", "", "")
	require.Error(t, err)
}

func TestParticipantToken(t *testing.T) {
	c, err := New("wss://voice.example.com", "", testKey, testSecret)
	require.NoError(t, err)
	token, exp, err := c.ParticipantToken(TokenOptions{
		Identity: "user-1", Name: "Alice", Room: "room-1", TTL: 10 * time.Minute,
		Permission: Permission{CanSubscribe: true, Sources: []Source{SourceMicrophone, SourceScreenShare}},
	})
	require.NoError(t, err)
	require.WithinDuration(t, time.Now().Add(10*time.Minute), exp, 5*time.Second)

	cl := parse(t, token, testSecret)
	require.Equal(t, testKey, cl["iss"])
	require.Equal(t, "user-1", cl["sub"])
	require.Equal(t, "Alice", cl["name"])
	video := cl["video"].(map[string]any)
	require.Equal(t, true, video["roomJoin"])
	require.Equal(t, "room-1", video["room"])
	require.Equal(t, true, video["canPublish"])
	require.Equal(t, true, video["canSubscribe"])
	require.Equal(t, false, video["canPublishData"])
	require.Equal(t, []any{"microphone", "screen_share"}, video["canPublishSources"])
	require.Nil(t, video["roomAdmin"])

	// Nothing to publish means no publishing at all, not "every source".
	token, _, err = c.ParticipantToken(TokenOptions{Identity: "u", Room: "r", TTL: time.Minute})
	require.NoError(t, err)
	video = parse(t, token, testSecret)["video"].(map[string]any)
	require.Equal(t, false, video["canPublish"])
	require.Equal(t, false, video["canSubscribe"])
	require.Nil(t, video["canPublishSources"])
}

type twirpCall struct {
	method string
	grant  map[string]any
	body   map[string]any
}

func fakeTwirp(t *testing.T, handle func(method string, body map[string]any) (int, any)) (*Client, *[]twirpCall) {
	t.Helper()
	var calls []twirpCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, ok := strings.CutPrefix(r.URL.Path, "/twirp/livekit.RoomService/")
		require.True(t, ok, r.URL.Path)
		cl := parse(t, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), testSecret)
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		calls = append(calls, twirpCall{method: method, grant: cl["video"].(map[string]any), body: body})
		status, resp := handle(method, body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	c, err := New(srv.URL, "", testKey, testSecret)
	require.NoError(t, err)
	return c, &calls
}

func TestRoomService(t *testing.T) {
	notFound := map[string]any{"code": "not_found", "msg": "participant not found"}
	c, calls := fakeTwirp(t, func(method string, body map[string]any) (int, any) {
		switch method {
		case "ListParticipants":
			if body["room"] == "missing" {
				return http.StatusNotFound, notFound
			}
			return 200, map[string]any{"participants": []any{map[string]any{"identity": "u1", "sid": "PA_1", "state": "ACTIVE"}}}
		case "RemoveParticipant", "DeleteRoom":
			return http.StatusNotFound, notFound
		case "UpdateParticipant":
			return 200, map[string]any{"identity": body["identity"]}
		case "ListRooms":
			return http.StatusUnauthorized, map[string]any{"code": "unauthenticated", "msg": "invalid token"}
		}
		return 500, nil
	})
	ctx := context.Background()

	ps, err := c.ListParticipants(ctx, "room-1")
	require.NoError(t, err)
	require.Equal(t, []Participant{{Identity: "u1", SID: "PA_1"}}, ps)
	ps, err = c.ListParticipants(ctx, "missing")
	require.NoError(t, err)
	require.Empty(t, ps)

	require.NoError(t, c.RemoveParticipant(ctx, "room-1", "u1"))
	require.NoError(t, c.DeleteRoom(ctx, "room-1"))
	require.NoError(t, c.UpdatePermission(ctx, "room-1", "u1", Permission{CanSubscribe: true, Sources: []Source{SourceMicrophone}}))
	err = c.Ping(ctx)
	require.Error(t, err)
	require.False(t, IsNotFound(err))
	require.Contains(t, err.Error(), "unauthenticated")

	byMethod := map[string]twirpCall{}
	for _, call := range *calls {
		byMethod[call.method] = call
	}
	require.Equal(t, map[string]any{"room": "room-1", "identity": "u1"}, byMethod["RemoveParticipant"].body)
	require.Equal(t, true, byMethod["RemoveParticipant"].grant["roomAdmin"])
	require.Equal(t, "room-1", byMethod["RemoveParticipant"].grant["room"])
	require.Equal(t, true, byMethod["DeleteRoom"].grant["roomCreate"])
	require.Equal(t, true, byMethod["ListRooms"].grant["roomList"])
	perm := byMethod["UpdateParticipant"].body["permission"].(map[string]any)
	require.Equal(t, []any{"MICROPHONE"}, perm["canPublishSources"])
	require.Equal(t, true, perm["canPublish"])
	require.Equal(t, false, perm["canPublishData"])
}

func webhookRequest(t *testing.T, body []byte, auth string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/webhook+json")
	if auth != "" {
		r.Header.Set("Authorization", auth)
	}
	return r
}

func TestReceiveWebhook(t *testing.T) {
	c, err := New("ws://localhost:7880", "", testKey, testSecret)
	require.NoError(t, err)
	body := []byte(`{"event":"participant_joined","id":"EV_1","createdAt":"1700000000",
		"room":{"sid":"RM_1","name":"room-1","emptyTimeout":300},
		"participant":{"sid":"PA_1","identity":"u1","state":"ACTIVE","joinedAt":"1700000000"}}`)
	token, err := c.WebhookToken(body)
	require.NoError(t, err)

	ev, err := c.ReceiveWebhook(webhookRequest(t, body, token))
	require.NoError(t, err)
	require.Equal(t, "participant_joined", ev.Event)
	require.Equal(t, "room-1", ev.Room.Name)
	require.Equal(t, Participant{Identity: "u1", SID: "PA_1"}, *ev.Participant)

	// A "Bearer " prefix is tolerated.
	_, err = c.ReceiveWebhook(webhookRequest(t, body, "Bearer "+token))
	require.NoError(t, err)

	// Missing, tampered, foreign or mismatched signatures are rejected.
	_, err = c.ReceiveWebhook(webhookRequest(t, body, ""))
	require.ErrorIs(t, err, ErrInvalidWebhook)
	tampered := bytes.Replace(body, []byte("joined"), []byte("left\x20\x20"), 1)
	_, err = c.ReceiveWebhook(webhookRequest(t, tampered, token))
	require.ErrorIs(t, err, ErrInvalidWebhook)
	other, err := New("ws://localhost:7880", "", testKey, strings.Repeat("x", 40))
	require.NoError(t, err)
	foreign, err := other.WebhookToken(body)
	require.NoError(t, err)
	_, err = c.ReceiveWebhook(webhookRequest(t, body, foreign))
	require.ErrorIs(t, err, ErrInvalidWebhook)
	wrongKey, err := New("ws://localhost:7880", "", "otherkey", testSecret)
	require.NoError(t, err)
	foreign, err = wrongKey.WebhookToken(body)
	require.NoError(t, err)
	_, err = c.ReceiveWebhook(webhookRequest(t, body, foreign))
	require.ErrorIs(t, err, ErrInvalidWebhook)

	// Tokens without an expiry are never accepted.
	sum := parse(t, token, testSecret)["sha256"].(string)
	noExp, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"iss": testKey, "sha256": sum}).SignedString([]byte(testSecret))
	require.NoError(t, err)
	_, err = c.ReceiveWebhook(webhookRequest(t, body, noExp))
	require.ErrorIs(t, err, ErrInvalidWebhook)

	// Oversized bodies are rejected without being buffered whole.
	_, err = c.ReceiveWebhook(webhookRequest(t, bytes.Repeat([]byte(" "), maxWebhookBody+10), token))
	require.ErrorIs(t, err, ErrInvalidWebhook)
}
