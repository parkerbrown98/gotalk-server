package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/require"

	"github.com/parkerbrown98/gotalk-server/internal/config"
	"github.com/parkerbrown98/gotalk-server/internal/livekit"
	"github.com/parkerbrown98/gotalk-server/internal/permissions"
)

const (
	lkKey    = "gotalk-test"
	lkSecret = "test-secret-test-secret-test-secret-1234"
)

type lkCall struct {
	Method string
	Body   map[string]any
}

// fakeLiveKit implements the RoomService calls Gotalk makes, records them, and tracks
// which participants are "connected".
type fakeLiveKit struct {
	t      *testing.T
	srv    *httptest.Server
	client *livekit.Client

	mu    sync.Mutex
	calls []lkCall
	rooms map[string][]livekit.Participant
	fail  bool
}

func newFakeLiveKit(t *testing.T) *fakeLiveKit {
	t.Helper()
	f := &fakeLiveKit{t: t, rooms: map[string][]livekit.Participant{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	var err error
	f.client, err = livekit.New(f.srv.URL, "", lkKey, lkSecret)
	require.NoError(t, err)
	return f
}

func (f *fakeLiveKit) serve(w http.ResponseWriter, r *http.Request) {
	reply := func(status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	method, ok := strings.CutPrefix(r.URL.Path, "/twirp/livekit.RoomService/")
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	_, err := jwt.Parse(token, func(*jwt.Token) (any, error) { return []byte(lkSecret), nil },
		jwt.WithIssuer(lkKey), jwt.WithValidMethods([]string{"HS256"}), jwt.WithExpirationRequired())
	if !ok || err != nil {
		reply(http.StatusUnauthorized, map[string]any{"code": "unauthenticated", "msg": "invalid token"})
		return
	}
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, lkCall{Method: method, Body: body})
	if f.fail {
		reply(http.StatusInternalServerError, map[string]any{"code": "internal", "msg": "boom"})
		return
	}
	room, _ := body["room"].(string)
	switch method {
	case "ListParticipants":
		reply(200, map[string]any{"participants": f.rooms[room]})
	case "RemoveParticipant":
		identity, _ := body["identity"].(string)
		f.rooms[room] = slices.DeleteFunc(f.rooms[room], func(p livekit.Participant) bool { return p.Identity == identity })
		reply(200, map[string]any{})
	case "DeleteRoom":
		delete(f.rooms, room)
		reply(200, map[string]any{})
	case "UpdateParticipant", "ListRooms":
		reply(200, map[string]any{})
	default:
		reply(http.StatusNotFound, map[string]any{"code": "bad_route", "msg": method})
	}
}

func (f *fakeLiveKit) join(room, identity, sid string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rooms[room] = append(f.rooms[room], livekit.Participant{Identity: identity, SID: sid})
}

func (f *fakeLiveKit) leave(room, identity string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rooms[room] = slices.DeleteFunc(f.rooms[room], func(p livekit.Participant) bool { return p.Identity == identity })
}

func (f *fakeLiveKit) setFail(fail bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail = fail
}

// called returns the bodies of method calls whose room and identity match (empty
// strings match anything).
func (f *fakeLiveKit) called(method, room, identity string) []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []map[string]any
	for _, c := range f.calls {
		if c.Method == method && (room == "" || c.Body["room"] == room) && (identity == "" || c.Body["identity"] == identity) {
			out = append(out, c.Body)
		}
	}
	return out
}

func withVoice(lk *fakeLiveKit) func(*envOpts) {
	return func(o *envOpts) {
		prev := o.mutate
		o.mutate = func(c *config.Config) {
			if prev != nil {
				prev(c)
			}
			c.Voice.LiveKitURL = lk.srv.URL
			c.Voice.LiveKitAPIKey = lkKey
			c.Voice.LiveKitAPISecret = lkSecret
		}
	}
}

// webhook delivers a LiveKit webhook signed with signer.
func (e *env) webhook(signer *livekit.Client, event, room, identity, sid string) resp {
	e.t.Helper()
	body := map[string]any{"event": event, "id": "EV_test", "createdAt": "1700000000", "room": map[string]any{"name": room, "sid": "RM_test"}}
	if identity != "" {
		body["participant"] = map[string]any{"identity": identity, "sid": sid, "state": "ACTIVE"}
	}
	raw, err := json.Marshal(body)
	require.NoError(e.t, err)
	token, err := signer.WebhookToken(raw)
	require.NoError(e.t, err)
	req, err := http.NewRequest(http.MethodPost, e.srv.URL+"/api/v1/voice/webhook", bytes.NewReader(raw))
	require.NoError(e.t, err)
	req.Header.Set("Content-Type", "application/webhook+json")
	req.Header.Set("Authorization", token)
	res, err := http.DefaultClient.Do(req)
	require.NoError(e.t, err)
	defer func() { _ = res.Body.Close() }()
	out, _ := io.ReadAll(res.Body)
	return resp{Status: res.StatusCode, Header: res.Header, Raw: out}
}

func (e *env) joinVoice(token string, channel map[string]any) map[string]any {
	e.t.Helper()
	return e.expect(200, e.do("POST", "/api/v1/channels/"+id(channel)+"/voice", token, map[string]any{})).obj(e.t)
}

func (e *env) myVoice(token string) resp {
	e.t.Helper()
	return e.do("GET", "/api/v1/users/@me/voice", token, nil)
}

func voiceOf(userID string, match func(map[string]any) bool) func(map[string]any) bool {
	return func(d map[string]any) bool { return d["user_id"] == userID && (match == nil || match(d)) }
}

func inChannel(ch string) func(map[string]any) bool {
	return func(d map[string]any) bool { return d["channel_id"] == ch }
}

func leftVoice(d map[string]any) bool { return d["channel_id"] == nil }

func sources(t *testing.T, body map[string]any) []any {
	t.Helper()
	perm, ok := body["permission"].(map[string]any)
	require.True(t, ok, "no permission in %v", body)
	s, _ := perm["canPublishSources"].([]any)
	return s
}

func TestVoiceDisabled(t *testing.T) {
	e := newEnv(t)
	owner := e.setup()
	e.createPlace(owner, "quiet", "public")
	lounge := e.channel(owner, "quiet", map[string]any{"name": "lounge", "kind": "voice", "user_limit": 5})
	require.Equal(t, "voice", lounge["kind"])
	require.EqualValues(t, 5, lounge["user_limit"])

	// Voice channels can be set up ahead of time, but joining needs a LiveKit server.
	e.expect(503, e.do("POST", "/api/v1/channels/"+id(lounge)+"/voice", owner, map[string]any{}))
	e.expect(404, e.myVoice(owner))
	e.expect(204, e.do("DELETE", "/api/v1/users/@me/voice", owner, nil))
	e.expect(404, e.do("POST", "/api/v1/voice/webhook", "", map[string]any{}))

	info := e.expect(200, e.do("GET", "/api/v1/instance", "", nil)).obj(t)
	require.Equal(t, false, info["features"].(map[string]any)["voice"])
	ready := e.expect(200, e.do("GET", "/readyz", "", nil)).obj(t)
	require.Equal(t, "ready", ready["status"])
	require.Equal(t, "not_configured", ready["checks"].(map[string]any)["voice"])
	checks := e.expect(200, e.do("GET", "/api/v1/setup/status", "", nil)).obj(t)["checks"].([]any)
	require.Contains(t, checks, map[string]any{"name": "voice", "status": "skipped",
		"detail": "not configured; set GOTALK_VOICE_LIVEKIT_URL and API credentials to enable voice channels"})
}

func TestVoiceChannels(t *testing.T) {
	lk := newFakeLiveKit(t)
	e := newEnv(t, withVoice(lk))
	owner := e.setup()
	e.createPlace(owner, "hangout", "public")
	cat := e.channel(owner, "hangout", map[string]any{"name": "Voice", "kind": "category"})
	lounge := e.channel(owner, "hangout", map[string]any{"name": "lounge", "kind": "voice", "parent_id": id(cat), "user_limit": 2})
	general := e.channel(owner, "hangout", map[string]any{"name": "general"})
	def := e.defaultRoleID(owner, "hangout")

	// Voice channels are listed with the others and have no messages, read state or mutes.
	listed := byID(e.listChannels(owner, "hangout"), id(lounge))
	require.Equal(t, "voice", listed["kind"])
	require.EqualValues(t, 2, listed["user_limit"])
	require.Nil(t, listed["read_state"])
	e.expect(422, e.do("POST", "/api/v1/channels/"+id(lounge)+"/messages", owner, map[string]any{"content": "hi"}))
	e.expect(422, e.do("GET", "/api/v1/channels/"+id(lounge)+"/messages", owner, nil))
	e.expect(422, e.do("POST", "/api/v1/channels/"+id(lounge)+"/typing", owner, nil))
	e.expect(422, e.do("PUT", "/api/v1/channels/"+id(lounge)+"/subscription", owner, map[string]any{"level": "muted"}))
	e.expect(422, e.do("POST", "/api/v1/channels/"+id(general)+"/voice", owner, map[string]any{}))

	// User limits apply to voice channels only, and can be changed.
	e.expect(422, e.do("POST", "/api/v1/places/hangout/channels", owner, map[string]any{"name": "nope", "user_limit": 3}))
	e.expect(422, e.do("PATCH", "/api/v1/channels/"+id(general), owner, map[string]any{"user_limit": 3}))
	e.expect(422, e.do("POST", "/api/v1/places/hangout/channels", owner, map[string]any{"name": "big", "kind": "voice", "user_limit": 100}))
	updated := e.expect(200, e.do("PATCH", "/api/v1/channels/"+id(lounge), owner, map[string]any{"user_limit": 3})).obj(t)
	require.EqualValues(t, 3, updated["user_limit"])

	// Overwrites take the permissions that make sense for each kind of channel.
	e.expect(422, e.do("PUT", "/api/v1/channels/"+id(lounge)+"/overwrites/"+def, owner, map[string]any{"allow": 0, "deny": permissions.SendMessages}))
	e.expect(422, e.do("PUT", "/api/v1/channels/"+id(general)+"/overwrites/"+def, owner, map[string]any{"allow": 0, "deny": permissions.Speak}))
	e.expect(200, e.do("PUT", "/api/v1/channels/"+id(lounge)+"/overwrites/"+def, owner, map[string]any{"allow": 0, "deny": permissions.ShareScreen}))
	e.expect(200, e.do("PUT", "/api/v1/channels/"+id(cat)+"/overwrites/"+def, owner, map[string]any{"allow": 0, "deny": permissions.ConnectVoice | permissions.SendMessages}))

	// Deleting a category moves its voice channels to the top level.
	e.expect(204, e.do("DELETE", "/api/v1/channels/"+id(cat), owner, nil))
	require.Nil(t, byID(e.listChannels(owner, "hangout"), id(lounge))["parent_id"])

	info := e.expect(200, e.do("GET", "/api/v1/instance", "", nil)).obj(t)
	require.Equal(t, true, info["features"].(map[string]any)["voice"])
	ready := e.expect(200, e.do("GET", "/readyz", "", nil)).obj(t)
	require.Equal(t, "ok", ready["checks"].(map[string]any)["voice"])

	// An unreachable LiveKit degrades readiness without failing it.
	lk.setFail(true)
	ready = e.expect(200, e.do("GET", "/readyz", "", nil)).obj(t)
	require.Equal(t, "degraded", ready["status"])
	lk.setFail(false)
}

func TestVoiceLifecycle(t *testing.T) {
	lk := newFakeLiveKit(t)
	e := newEnv(t, withVoice(lk))
	owner := e.setup()
	ownerID := e.userID(owner)
	bob, bobID := e.register("bob")
	carol, _ := e.register("carol")
	e.createPlace(owner, "hangout", "public")
	e.expect(200, e.do("POST", "/api/v1/places/hangout/join", bob, nil))
	e.expect(200, e.do("POST", "/api/v1/places/hangout/join", carol, nil))
	lounge := e.channel(owner, "hangout", map[string]any{"name": "lounge", "kind": "voice", "user_limit": 2})
	lobby := e.channel(owner, "hangout", map[string]any{"name": "lobby", "kind": "voice"})
	def := e.defaultRoleID(owner, "hangout")
	oc := e.connect(owner)
	bc := e.connect(bob)

	// Joining returns a LiveKit token scoped to the channel's room and the caller's rights.
	conn := e.joinVoice(bob, lounge)
	require.True(t, strings.HasPrefix(conn["url"].(string), "ws://"), conn["url"])
	require.Equal(t, id(lounge), conn["room"])
	claims := jwt.MapClaims{}
	_, err := jwt.ParseWithClaims(conn["token"].(string), claims, func(*jwt.Token) (any, error) { return []byte(lkSecret), nil })
	require.NoError(t, err)
	require.Equal(t, bobID, claims["sub"])
	require.Equal(t, lkKey, claims["iss"])
	require.Equal(t, "bob", claims["name"])
	video := claims["video"].(map[string]any)
	require.Equal(t, id(lounge), video["room"])
	require.Equal(t, true, video["roomJoin"])
	require.Equal(t, false, video["canPublishData"])
	require.Equal(t, []any{"microphone", "camera", "screen_share", "screen_share_audio"}, video["canPublishSources"])
	state := conn["state"].(map[string]any)
	require.Equal(t, id(lounge), state["channel_id"])
	require.Equal(t, false, state["connected"])
	require.Equal(t, true, state["can_speak"])
	require.Equal(t, "bob", state["user"].(map[string]any)["username"])
	oc.expect("VOICE_STATE_UPDATE", voiceOf(bobID, inChannel(id(lounge))))

	listed := e.expect(200, e.do("GET", "/api/v1/places/hangout/voice-states", owner, nil)).list(t)
	require.Len(t, listed, 1)
	require.Equal(t, bobID, listed[0].(map[string]any)["user_id"])

	// LiveKit's webhook confirms the connection; forged webhooks are rejected.
	e.expect(200, e.webhook(lk.client, "participant_joined", id(lounge), bobID, "PA_bob"))
	oc.expect("VOICE_STATE_UPDATE", voiceOf(bobID, func(d map[string]any) bool { return d["connected"] == true }))
	forger, err := livekit.New(lk.srv.URL, "", lkKey, strings.Repeat("z", 40))
	require.NoError(t, err)
	e.expect(401, e.webhook(forger, "participant_left", id(lounge), bobID, "PA_bob"))
	e.expect(200, e.webhook(lk.client, "participant_joined", "not-a-gotalk-room", bobID, "PA_bob"))
	require.Equal(t, true, e.expect(200, e.myVoice(bob)).obj(t)["connected"])

	// New gateway sessions learn the user's voice state in READY.
	bc2 := e.connect(bob)
	require.Equal(t, id(lounge), bc2.ready["voice_state"].(map[string]any)["channel_id"])
	require.Nil(t, oc.ready["voice_state"])

	// Clients report their own mute/deafen/camera state; deafening also mutes.
	self := e.expect(200, e.do("PATCH", "/api/v1/users/@me/voice", bob, map[string]any{"self_deaf": true})).obj(t)
	require.Equal(t, true, self["self_mute"])
	oc.expect("VOICE_STATE_UPDATE", voiceOf(bobID, func(d map[string]any) bool { return d["self_deaf"] == true }))
	e.expect(200, e.do("PATCH", "/api/v1/users/@me/voice", bob, map[string]any{"self_mute": false, "self_deaf": false, "self_video": true}))
	e.expect(404, e.do("PATCH", "/api/v1/users/@me/voice", carol, map[string]any{"self_mute": true}))

	// Speaking indicators are relayed to everyone who can see the channel.
	writeFrame(t, bc.conn, map[string]any{"op": "voice_speaking", "d": map[string]any{"speaking": true}})
	sp := oc.expect("VOICE_SPEAKING", userField("user_id", bobID))
	require.Equal(t, true, sp["speaking"])
	require.Equal(t, id(lounge), sp["channel_id"])

	// User limits stop new joiners unless they can move members.
	e.joinVoice(owner, lounge)
	e.expect(409, e.do("POST", "/api/v1/channels/"+id(lounge)+"/voice", carol, map[string]any{}))

	// Permission changes reach LiveKit: denying SPEAK and SHARE_SCREEN revokes the
	// microphone, camera and screen share, and turns the camera flag off.
	e.expect(200, e.do("PUT", "/api/v1/channels/"+id(lounge)+"/overwrites/"+def, owner,
		map[string]any{"allow": 0, "deny": permissions.Speak | permissions.ShareScreen}))
	revoked := oc.expect("VOICE_STATE_UPDATE", voiceOf(bobID, func(d map[string]any) bool { return d["can_speak"] == false }))
	require.Equal(t, false, revoked["can_stream"])
	require.Equal(t, false, revoked["self_video"])
	updates := lk.called("UpdateParticipant", id(lounge), bobID)
	require.NotEmpty(t, updates)
	require.Empty(t, sources(t, updates[len(updates)-1]))
	require.Empty(t, lk.called("UpdateParticipant", id(lounge), ownerID), "owners keep every permission")
	e.expect(403, e.do("PATCH", "/api/v1/users/@me/voice", bob, map[string]any{"self_stream": true}))
	e.expect(204, e.do("DELETE", "/api/v1/channels/"+id(lounge)+"/overwrites/"+def, owner, nil))
	oc.expect("VOICE_STATE_UPDATE", voiceOf(bobID, func(d map[string]any) bool { return d["can_speak"] == true }))
	updates = lk.called("UpdateParticipant", id(lounge), bobID)
	require.Equal(t, []any{"MICROPHONE", "CAMERA", "SCREEN_SHARE", "SCREEN_SHARE_AUDIO"}, sources(t, updates[len(updates)-1]))

	// Call quality reports are averaged per session, for MANAGE_CHANNELS to diagnose.
	e.expect(204, e.do("POST", "/api/v1/users/@me/voice/telemetry", bob, map[string]any{"packet_loss": 0.1, "rtt_ms": 50, "bitrate_kbps": 32}))
	e.expect(204, e.do("POST", "/api/v1/users/@me/voice/telemetry", bob, map[string]any{"packet_loss": 0.3, "rtt_ms": 150, "bitrate_kbps": 64}))
	e.expect(422, e.do("POST", "/api/v1/users/@me/voice/telemetry", bob, map[string]any{"packet_loss": 2}))
	e.expect(404, e.do("POST", "/api/v1/users/@me/voice/telemetry", carol, map[string]any{"packet_loss": 0}))
	e.expect(403, e.do("GET", "/api/v1/channels/"+id(lounge)+"/voice/sessions", bob, nil))
	sessions := e.items(e.expect(200, e.do("GET", "/api/v1/channels/"+id(lounge)+"/voice/sessions", owner, nil)))
	var bobSession map[string]any
	for _, s := range sessions {
		if s["user"].(map[string]any)["id"] == bobID {
			bobSession = s
		}
	}
	require.NotNil(t, bobSession)
	require.NotNil(t, bobSession["connected_at"])
	q := bobSession["quality"].(map[string]any)
	require.EqualValues(t, 2, q["samples"])
	require.InDelta(t, 0.2, q["packet_loss_avg"], 1e-9)
	require.InDelta(t, 0.3, q["packet_loss_max"], 1e-9)
	require.InDelta(t, 100, q["rtt_ms_avg"], 1e-9)
	require.InDelta(t, 48, q["bitrate_kbps_avg"], 1e-9)

	// Joining another channel leaves the first one, in LiveKit too.
	e.joinVoice(bob, lobby)
	oc.expect("VOICE_STATE_UPDATE", voiceOf(bobID, leftVoice))
	oc.expect("VOICE_STATE_UPDATE", voiceOf(bobID, inChannel(id(lobby))))
	require.NotEmpty(t, lk.called("RemoveParticipant", id(lounge), bobID))
	// Joining the same channel again from the same session only refreshes the token.
	again := e.joinVoice(bob, lobby)
	require.Equal(t, again["state"].(map[string]any)["session_id"], e.expect(200, e.myVoice(bob)).obj(t)["session_id"])

	// Leaving.
	e.expect(204, e.do("DELETE", "/api/v1/users/@me/voice", bob, nil))
	oc.expect("VOICE_STATE_UPDATE", voiceOf(bobID, leftVoice))
	require.NotEmpty(t, lk.called("RemoveParticipant", id(lobby), bobID))
	e.expect(404, e.myVoice(bob))

	// LiveKit reporting a participant gone ends their stay, but only for the connection
	// we know about.
	e.expect(200, e.webhook(lk.client, "participant_joined", id(lounge), ownerID, "PA_owner"))
	e.expect(200, e.webhook(lk.client, "participant_left", id(lounge), ownerID, "PA_stale"))
	e.expect(200, e.myVoice(owner))
	e.expect(200, e.webhook(lk.client, "participant_left", id(lounge), ownerID, "PA_owner"))
	oc.expect("VOICE_STATE_UPDATE", voiceOf(ownerID, leftVoice))
	e.expect(404, e.myVoice(owner))

	reasons := map[string]bool{}
	for _, s := range e.items(e.expect(200, e.do("GET", "/api/v1/channels/"+id(lounge)+"/voice/sessions", owner, nil))) {
		require.NotNil(t, s["ended_at"])
		reasons[s["end_reason"].(string)] = true
	}
	require.Equal(t, map[string]bool{"switched": true, "disconnected": true}, reasons)

	// room_finished clears whoever was still connected.
	e.joinVoice(carol, lobby)
	e.expect(200, e.webhook(lk.client, "participant_joined", id(lobby), e.userID(carol), "PA_carol"))
	e.expect(200, e.webhook(lk.client, "room_finished", id(lobby), "", ""))
	e.expect(404, e.myVoice(carol))
}

func TestVoiceModeration(t *testing.T) {
	lk := newFakeLiveKit(t)
	e := newEnv(t, withVoice(lk))
	owner := e.setup()
	ownerID := e.userID(owner)
	mod, modID := e.register("dave")
	bob, bobID := e.register("bob")
	carol, carolID := e.register("carol")
	e.createPlace(owner, "stage", "public")
	for _, tok := range []string{mod, bob, carol} {
		e.expect(200, e.do("POST", "/api/v1/places/stage/join", tok, nil))
	}
	role := e.expect(201, e.do("POST", "/api/v1/places/stage/roles", owner, map[string]any{
		"name": "voice mod", "permissions": permissions.Default | permissions.MuteMembers | permissions.MoveMembers,
	})).obj(t)
	e.expect(200, e.do("PUT", "/api/v1/places/stage/members/"+modID+"/roles/"+id(role), owner, nil))
	a := e.channel(owner, "stage", map[string]any{"name": "a", "kind": "voice"})
	b := e.channel(owner, "stage", map[string]any{"name": "b", "kind": "voice"})
	text := e.channel(owner, "stage", map[string]any{"name": "text"})
	bobPath := "/api/v1/places/stage/members/" + bobID + "/voice"
	oc := e.connect(owner)
	bc := e.connect(bob)
	e.joinVoice(bob, a)

	// Server mute needs MUTE_MEMBERS and a higher rank; it revokes the microphone.
	e.expect(403, e.do("PATCH", bobPath, carol, map[string]any{"mute": true}))
	e.expect(403, e.do("PATCH", "/api/v1/places/stage/members/"+modID+"/voice", bob, map[string]any{"mute": true}))
	e.expect(422, e.do("PATCH", bobPath, mod, map[string]any{}))
	muted := e.expect(200, e.do("PATCH", bobPath, mod, map[string]any{"mute": true})).obj(t)
	require.Equal(t, true, muted["mute"])
	require.Equal(t, false, muted["state"].(map[string]any)["can_speak"])
	bc.expect("VOICE_STATE_UPDATE", voiceOf(bobID, func(d map[string]any) bool { return d["mute"] == true }))
	updates := lk.called("UpdateParticipant", id(a), bobID)
	require.NotEmpty(t, updates)
	require.NotContains(t, sources(t, updates[len(updates)-1]), "MICROPHONE")

	// Moving needs MOVE_MEMBERS; the moved member gets a token for the new room.
	e.expect(422, e.do("PATCH", bobPath, mod, map[string]any{"channel_id": id(text)}))
	e.expect(422, e.do("PATCH", "/api/v1/places/stage/members/"+carolID+"/voice", mod, map[string]any{"channel_id": id(b)}))
	moved := e.expect(200, e.do("PATCH", bobPath, mod, map[string]any{"channel_id": id(b)})).obj(t)
	require.Equal(t, id(b), moved["state"].(map[string]any)["channel_id"])
	require.Equal(t, true, moved["state"].(map[string]any)["mute"], "server mute follows the member")
	server := bc.expect("VOICE_SERVER_UPDATE", nil)
	require.Equal(t, id(b), server["room"])
	require.NotEmpty(t, server["token"])
	require.NotEmpty(t, lk.called("RemoveParticipant", id(a), bobID))
	got := e.expect(200, e.do("GET", bobPath, carol, nil)).obj(t)
	require.Equal(t, true, got["mute"])
	require.Equal(t, id(b), got["state"].(map[string]any)["channel_id"])

	// Server deafen stops the member from hearing anyone.
	e.expect(200, e.do("PATCH", bobPath, mod, map[string]any{"mute": false, "deaf": true}))
	updates = lk.called("UpdateParticipant", id(b), bobID)
	last := updates[len(updates)-1]["permission"].(map[string]any)
	require.Equal(t, false, last["canSubscribe"])
	require.Contains(t, last["canPublishSources"], "MICROPHONE")

	actions := map[string]bool{}
	for _, entry := range e.items(e.expect(200, e.do("GET", "/api/v1/places/stage/audit-log?action=member&target_id="+bobID, owner, nil))) {
		actions[entry["action"].(string)] = true
	}
	for _, want := range []string{"member.voice_mute", "member.voice_unmute", "member.voice_deafen", "member.voice_move"} {
		require.True(t, actions[want], "missing audit entry %s in %v", want, actions)
	}

	// Disconnecting needs MOVE_MEMBERS.
	e.expect(403, e.do("DELETE", bobPath, carol, nil))
	e.expect(204, e.do("DELETE", bobPath+"?reason=afk", mod, nil))
	bc.expect("VOICE_STATE_UPDATE", voiceOf(bobID, leftVoice))
	require.NotEmpty(t, lk.called("RemoveParticipant", id(b), bobID))
	require.Nil(t, e.expect(200, e.do("GET", bobPath, mod, nil)).obj(t)["state"])
	e.expect(404, e.do("DELETE", bobPath, mod, nil))

	// Rejoining keeps the server deafen; a timeout withholds speaking and streaming.
	rejoined := e.joinVoice(bob, a)["state"].(map[string]any)
	require.Equal(t, true, rejoined["deaf"])
	e.expect(200, e.do("PUT", "/api/v1/places/stage/members/"+bobID+"/timeout", owner, map[string]any{"duration": 600}))
	oc.expect("VOICE_STATE_UPDATE", voiceOf(bobID, func(d map[string]any) bool { return d["can_speak"] == false && d["can_stream"] == false }))
	e.expect(200, e.do("DELETE", "/api/v1/places/stage/members/"+bobID+"/timeout", owner, nil))
	oc.expect("VOICE_STATE_UPDATE", voiceOf(bobID, func(d map[string]any) bool { return d["can_stream"] == true }))

	// Losing access to the channel, or the place, disconnects.
	e.expect(204, e.do("DELETE", "/api/v1/places/stage/members/"+bobID, owner, nil))
	bc.expect("VOICE_STATE_UPDATE", voiceOf(bobID, leftVoice))
	require.Len(t, lk.called("RemoveParticipant", id(a), bobID), 2, "once when moved, once when kicked")
	e.expect(404, e.myVoice(bob))

	e.joinVoice(carol, a)
	e.expect(200, e.do("PUT", "/api/v1/channels/"+id(a)+"/overwrites/"+e.defaultRoleID(owner, "stage"), owner,
		map[string]any{"allow": 0, "deny": permissions.ConnectVoice}))
	oc.expect("VOICE_STATE_UPDATE", voiceOf(carolID, leftVoice))
	e.expect(403, e.do("POST", "/api/v1/channels/"+id(a)+"/voice", carol, map[string]any{}))

	// Ending the session that joined voice disconnects it.
	e.joinVoice(carol, b)
	e.expect(204, e.do("POST", "/api/v1/auth/logout", carol, nil))
	oc.expect("VOICE_STATE_UPDATE", voiceOf(carolID, leftVoice))
	require.NotEmpty(t, lk.called("RemoveParticipant", id(b), carolID))

	// Deleting a voice channel disconnects everyone and closes the room.
	e.joinVoice(owner, b)
	e.expect(204, e.do("DELETE", "/api/v1/channels/"+id(b), owner, nil))
	oc.expect("VOICE_STATE_UPDATE", voiceOf(ownerID, leftVoice))
	require.NotEmpty(t, lk.called("DeleteRoom", id(b), ""))
	e.expect(404, e.myVoice(owner))

	// So does deleting the place.
	e.joinVoice(owner, a)
	e.expect(204, e.do("DELETE", "/api/v1/places/stage", owner, nil))
	oc.expect("VOICE_STATE_UPDATE", voiceOf(ownerID, leftVoice))
	e.expect(404, e.myVoice(owner))
}

func TestVoiceReconciliation(t *testing.T) {
	lk := newFakeLiveKit(t)
	e := newEnv(t, withVoice(lk))
	owner := e.setup()
	bob, bobID := e.register("bob")
	carol, _ := e.register("carol")
	e.createPlace(owner, "sync", "public")
	e.expect(200, e.do("POST", "/api/v1/places/sync/join", bob, nil))
	e.expect(200, e.do("POST", "/api/v1/places/sync/join", carol, nil))
	lounge := e.channel(owner, "sync", map[string]any{"name": "lounge", "kind": "voice"})
	ctx := context.Background()

	// Without webhooks, the sweep learns about connections from LiveKit itself.
	e.joinVoice(bob, lounge)
	lk.join(id(lounge), bobID, "PA_bob")
	require.NoError(t, e.svc.SweepVoice(ctx))
	require.Equal(t, true, e.expect(200, e.myVoice(bob)).obj(t)["connected"])

	// LiveKit being unreachable never drops anyone.
	lk.setFail(true)
	require.NoError(t, e.svc.SweepVoice(ctx))
	e.expect(200, e.myVoice(bob))
	lk.setFail(false)

	// Connected participants who disappeared from the room are removed.
	lk.leave(id(lounge), bobID)
	require.NoError(t, e.svc.SweepVoice(ctx))
	e.expect(404, e.myVoice(bob))

	// Joins that never connect expire after voice.join_timeout.
	e.joinVoice(carol, lounge)
	require.NoError(t, e.svc.SweepVoice(ctx))
	e.expect(200, e.myVoice(carol))
	e.cfg.Voice.JoinTimeout = time.Nanosecond
	require.NoError(t, e.svc.SweepVoice(ctx))
	e.expect(404, e.myVoice(carol))

	reasons := map[string]bool{}
	for _, s := range e.items(e.expect(200, e.do("GET", "/api/v1/channels/"+id(lounge)+"/voice/sessions", owner, nil))) {
		reasons[s["end_reason"].(string)] = true
	}
	require.Equal(t, map[string]bool{"disconnected": true, "timeout": true}, reasons)

	// Ended sessions are pruned after voice.session_retention.
	e.cfg.Voice.SessionRetention = time.Nanosecond
	require.NoError(t, e.svc.SweepVoice(ctx))
	require.Empty(t, e.items(e.expect(200, e.do("GET", "/api/v1/channels/"+id(lounge)+"/voice/sessions", owner, nil))))
}
