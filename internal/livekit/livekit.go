// Package livekit is a minimal client for a LiveKit SFU. It signs participant access
// tokens, calls the RoomService Twirp API, and verifies webhooks.
//
// It implements the few calls Gotalk needs directly on top of net/http and golang-jwt
// instead of depending on the LiveKit server SDK, whose root package also pulls in a full
// WebRTC client stack and would roughly double the size of the server binary. Wire formats
// follow github.com/livekit/protocol (auth, webhook and livekit_room.proto).
package livekit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Source is a kind of track a participant may publish.
type Source string

const (
	SourceCamera           Source = "camera"
	SourceMicrophone       Source = "microphone"
	SourceScreenShare      Source = "screen_share"
	SourceScreenShareAudio Source = "screen_share_audio"
)

// Permission is what a participant may do in a room.
type Permission struct {
	CanSubscribe bool
	// Sources lists what the participant may publish; empty means nothing.
	Sources []Source
}

// Participant is a connected participant as reported by LiveKit.
type Participant struct {
	Identity string `json:"identity"`
	SID      string `json:"sid"`
}

// Client talks to one LiveKit deployment with one API key.
type Client struct {
	url    string
	apiURL string
	key    string
	secret string
	http   *http.Client
}

// New creates a client. rawURL is what clients connect to (ws, wss, http or https);
// apiURL is how this server reaches LiveKit's HTTP API and defaults to rawURL.
func New(rawURL, apiURL, key, secret string) (*Client, error) {
	if key == "" || secret == "" {
		return nil, errors.New("livekit: API key and secret are required")
	}
	wsURL, err := normalize(rawURL, map[string]string{"http": "ws", "https": "wss"})
	if err != nil {
		return nil, err
	}
	if apiURL == "" {
		apiURL = rawURL
	}
	httpURL, err := normalize(apiURL, map[string]string{"ws": "http", "wss": "https"})
	if err != nil {
		return nil, err
	}
	return &Client{url: wsURL, apiURL: httpURL, key: key, secret: secret, http: &http.Client{Timeout: 10 * time.Second}}, nil
}

// normalize validates raw and rewrites its scheme through convert.
func normalize(raw string, convert map[string]string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("livekit: invalid URL %q", raw)
	}
	switch u.Scheme {
	case "ws", "wss", "http", "https":
	default:
		return "", fmt.Errorf("livekit: URL %q must use ws, wss, http or https", raw)
	}
	if s, ok := convert[u.Scheme]; ok {
		u.Scheme = s
	}
	return strings.TrimRight(u.String(), "/"), nil
}

// URL is the WebSocket URL clients connect to.
func (c *Client) URL() string { return c.url }

type videoGrant struct {
	RoomCreate           bool     `json:"roomCreate,omitempty"`
	RoomList             bool     `json:"roomList,omitempty"`
	RoomAdmin            bool     `json:"roomAdmin,omitempty"`
	RoomJoin             bool     `json:"roomJoin,omitempty"`
	Room                 string   `json:"room,omitempty"`
	CanPublish           *bool    `json:"canPublish,omitempty"`
	CanSubscribe         *bool    `json:"canSubscribe,omitempty"`
	CanPublishData       *bool    `json:"canPublishData,omitempty"`
	CanPublishSources    []string `json:"canPublishSources,omitempty"`
	CanUpdateOwnMetadata *bool    `json:"canUpdateOwnMetadata,omitempty"`
}

type claims struct {
	jwt.RegisteredClaims
	Identity string      `json:"identity,omitempty"`
	Name     string      `json:"name,omitempty"`
	Video    *videoGrant `json:"video,omitempty"`
	Sha256   string      `json:"sha256,omitempty"`
}

func (c *Client) sign(cl claims, ttl time.Duration) (string, time.Time, error) {
	now := time.Now()
	exp := now.Add(ttl)
	cl.Issuer = c.key
	cl.IssuedAt = jwt.NewNumericDate(now)
	cl.NotBefore = jwt.NewNumericDate(now)
	cl.ExpiresAt = jwt.NewNumericDate(exp)
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, cl).SignedString([]byte(c.secret))
	return token, exp, err
}

// TokenOptions describes a participant token.
type TokenOptions struct {
	Identity   string
	Name       string
	Room       string
	Permission Permission
	// TTL bounds when the token may be used to connect; LiveKit refreshes the credentials
	// of connected participants itself.
	TTL time.Duration
}

// ParticipantToken signs a token that lets one participant join one room.
func (c *Client) ParticipantToken(o TokenOptions) (string, time.Time, error) {
	if o.Identity == "" || o.Room == "" {
		return "", time.Time{}, errors.New("livekit: identity and room are required")
	}
	return c.sign(claims{
		RegisteredClaims: jwt.RegisteredClaims{Subject: o.Identity},
		Identity:         o.Identity,
		Name:             o.Name,
		Video:            participantGrant(o.Room, o.Permission),
	}, o.TTL)
}

func participantGrant(room string, p Permission) *videoGrant {
	f := false
	publish := len(p.Sources) > 0
	g := &videoGrant{
		RoomJoin: true, Room: room,
		CanPublish: &publish, CanSubscribe: &p.CanSubscribe,
		CanPublishData: &f, CanUpdateOwnMetadata: &f,
	}
	for _, s := range p.Sources {
		g.CanPublishSources = append(g.CanPublishSources, string(s))
	}
	return g
}

// Error is a Twirp error returned by LiveKit.
type Error struct {
	Status int    `json:"-"`
	Code   string `json:"code"`
	Msg    string `json:"msg"`
}

func (e *Error) Error() string {
	return fmt.Sprintf("livekit: %s (HTTP %d): %s", e.Code, e.Status, e.Msg)
}

// IsNotFound reports whether err means the room or participant does not exist.
func IsNotFound(err error) bool {
	var e *Error
	return errors.As(err, &e) && (e.Code == "not_found" || e.Status == http.StatusNotFound)
}

func (c *Client) call(ctx context.Context, method string, grant videoGrant, req, resp any) error {
	token, _, err := c.sign(claims{Video: &grant}, time.Minute)
	if err != nil {
		return err
	}
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiURL+"/twirp/livekit.RoomService/"+method, bytes.NewReader(body))
	if err != nil {
		return err
	}
	hr.Header.Set("Authorization", "Bearer "+token)
	hr.Header.Set("Content-Type", "application/json")
	res, err := c.http.Do(hr)
	if err != nil {
		return fmt.Errorf("livekit: %s: %w", method, err)
	}
	defer func() { _ = res.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return err
	}
	if res.StatusCode != http.StatusOK {
		e := &Error{}
		if json.Unmarshal(raw, e) != nil || e.Code == "" {
			e.Code, e.Msg = "unknown", strings.TrimSpace(string(raw))
		}
		e.Status = res.StatusCode
		return e
	}
	if resp == nil {
		return nil
	}
	return json.Unmarshal(raw, resp)
}

func ignoreNotFound(err error) error {
	if IsNotFound(err) {
		return nil
	}
	return err
}

// RemoveParticipant disconnects a participant. Missing rooms or participants are not errors.
func (c *Client) RemoveParticipant(ctx context.Context, room, identity string) error {
	return ignoreNotFound(c.call(ctx, "RemoveParticipant", videoGrant{RoomAdmin: true, Room: room},
		map[string]any{"room": room, "identity": identity}, nil))
}

// UpdatePermission replaces a connected participant's permissions. LiveKit unpublishes
// tracks from sources that are no longer allowed. Missing participants are not errors.
func (c *Client) UpdatePermission(ctx context.Context, room, identity string, p Permission) error {
	sources := make([]string, len(p.Sources))
	for i, s := range p.Sources {
		sources[i] = strings.ToUpper(string(s))
	}
	return ignoreNotFound(c.call(ctx, "UpdateParticipant", videoGrant{RoomAdmin: true, Room: room}, map[string]any{
		"room": room, "identity": identity,
		"permission": map[string]any{
			"canSubscribe": p.CanSubscribe, "canPublish": len(sources) > 0, "canPublishData": false,
			"canPublishSources": sources,
		},
	}, nil))
}

// DeleteRoom closes a room and disconnects everyone in it. Missing rooms are not errors.
func (c *Client) DeleteRoom(ctx context.Context, room string) error {
	return ignoreNotFound(c.call(ctx, "DeleteRoom", videoGrant{RoomCreate: true}, map[string]any{"room": room}, nil))
}

// ListParticipants returns who is connected to a room; nobody when the room does not exist.
func (c *Client) ListParticipants(ctx context.Context, room string) ([]Participant, error) {
	var out struct {
		Participants []Participant `json:"participants"`
	}
	err := c.call(ctx, "ListParticipants", videoGrant{RoomAdmin: true, Room: room}, map[string]any{"room": room}, &out)
	if err != nil && !IsNotFound(err) {
		return nil, err
	}
	if out.Participants == nil {
		out.Participants = []Participant{}
	}
	return out.Participants, nil
}

// Ping checks that LiveKit is reachable and accepts the API key.
func (c *Client) Ping(ctx context.Context) error {
	return c.call(ctx, "ListRooms", videoGrant{RoomList: true}, map[string]any{"names": []string{"gotalk-health-check"}}, nil)
}

// WebhookEvent is the part of a LiveKit webhook Gotalk uses.
type WebhookEvent struct {
	ID          string       `json:"id"`
	Event       string       `json:"event"`
	Room        *Room        `json:"room"`
	Participant *Participant `json:"participant"`
}

type Room struct {
	Name string `json:"name"`
	SID  string `json:"sid"`
}

// ErrInvalidWebhook is returned for webhooks that are unsigned, signed with another key,
// expired, or whose body does not match the signature.
var ErrInvalidWebhook = errors.New("livekit: invalid webhook signature")

const maxWebhookBody = 1 << 20

// ReceiveWebhook reads and verifies a webhook request. LiveKit signs each webhook with a
// token from the shared API key whose sha256 claim is the base64 SHA-256 of the body.
func (c *Client) ReceiveWebhook(r *http.Request) (WebhookEvent, error) {
	var ev WebhookEvent
	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBody+1))
	if err != nil {
		return ev, err
	}
	if len(body) > maxWebhookBody {
		return ev, ErrInvalidWebhook
	}
	raw := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if raw == "" {
		return ev, ErrInvalidWebhook
	}
	var cl claims
	_, err = jwt.ParseWithClaims(raw, &cl, func(*jwt.Token) (any, error) { return []byte(c.secret), nil },
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(c.key),
		jwt.WithLeeway(time.Minute),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		return ev, ErrInvalidWebhook
	}
	sum := sha256.Sum256(body)
	if subtle.ConstantTimeCompare([]byte(cl.Sha256), []byte(base64.StdEncoding.EncodeToString(sum[:]))) != 1 {
		return ev, ErrInvalidWebhook
	}
	if err := json.Unmarshal(body, &ev); err != nil {
		return ev, fmt.Errorf("livekit: decoding webhook: %w", err)
	}
	return ev, nil
}

// WebhookToken signs body the way LiveKit signs webhooks, for tests and tools that
// simulate LiveKit.
func (c *Client) WebhookToken(body []byte) (string, error) {
	sum := sha256.Sum256(body)
	token, _, err := c.sign(claims{Sha256: base64.StdEncoding.EncodeToString(sum[:])}, 5*time.Minute)
	return token, err
}
