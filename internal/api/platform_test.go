package api_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"

	"github.com/parkerbrown98/gotalk-server/internal/config"
	"github.com/parkerbrown98/gotalk-server/internal/permissions"
	"github.com/parkerbrown98/gotalk-server/internal/service"
)

// doAuth sends a request with a raw Authorization header (e.g. "Bot <token>").
func (e *env) doAuth(method, path, authorization string, body any) resp {
	e.t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(e.t, err)
		rdr = strings.NewReader(string(b))
	}
	req, err := http.NewRequest(method, e.srv.URL+path, rdr)
	require.NoError(e.t, err)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", authorization)
	res, err := noRedirect.Do(req)
	require.NoError(e.t, err)
	defer func() { _ = res.Body.Close() }()
	raw, err := io.ReadAll(res.Body)
	require.NoError(e.t, err)
	return resp{Status: res.StatusCode, Header: res.Header, Raw: raw}
}

func (e *env) pat(token string, scopes ...string) string {
	e.t.Helper()
	r := e.expect(201, e.do("POST", "/api/v1/users/@me/tokens", token, map[string]any{"name": "script", "scopes": scopes})).obj(e.t)
	return r["token"].(string)
}

// identifyFails connects to the gateway with token and returns the close code.
func (e *env) identifyFails(token string) websocket.StatusCode {
	e.t.Helper()
	conn := e.dial()
	readFrame(e.t, conn)
	writeFrame(e.t, conn, map[string]any{"op": "identify", "d": map[string]any{"token": token}})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err := conn.Read(ctx)
	return websocket.CloseStatus(err)
}

func TestPersonalAccessTokens(t *testing.T) {
	e := newEnv(t)
	admin := e.setup()
	alice, _ := e.register("alice")

	readOnly := e.pat(alice, "read")
	require.True(t, strings.HasPrefix(readOnly, "gtp_"))
	me := e.expect(200, e.do("GET", "/api/v1/users/@me", readOnly, nil)).obj(t)
	require.Equal(t, "alice", me["username"])
	require.Equal(t, false, me["bot"])

	// Scopes: read covers GET, write everything else.
	r := e.expect(403, e.do("POST", "/api/v1/places", readOnly, map[string]any{"slug": "nope", "name": "Nope"}))
	require.Contains(t, r.Header.Get("WWW-Authenticate"), "insufficient_scope")
	full := e.pat(alice, "write", "read", "gateway", "read")
	e.createPlace(full, "scripted", "public")

	// Credential management needs a login session.
	e.expect(403, e.do("GET", "/api/v1/users/@me/sessions", full, nil))
	e.expect(403, e.do("POST", "/api/v1/users/@me/tokens", full, map[string]any{"name": "x", "scopes": []string{"read"}}))
	e.expect(403, e.do("POST", "/api/v1/auth/logout", full, nil))
	e.expect(422, e.do("POST", "/api/v1/users/@me/tokens", alice, map[string]any{"name": "x", "scopes": []string{"everything"}}))
	e.expect(403, e.do("POST", "/api/v1/users/@me/tokens", alice, map[string]any{"name": "x", "scopes": []string{"admin"}}))

	tokens := e.expect(200, e.do("GET", "/api/v1/users/@me/tokens", alice, nil)).list(t)
	require.Len(t, tokens, 2)
	first := tokens[0].(map[string]any)
	require.Equal(t, []any{"read", "write", "gateway"}, first["scopes"])
	require.Nil(t, first["token"], "listings must never include the token")
	require.True(t, strings.HasPrefix(full, first["hint"].(string)))
	require.NotNil(t, first["last_used_at"])

	// The gateway needs the gateway scope; revoking a token closes its connections.
	require.Equal(t, websocket.StatusCode(4004), e.identifyFails(readOnly))
	c := e.connect(full)
	require.Equal(t, first["id"], c.ready["session_id"])
	e.expect(204, e.do("DELETE", "/api/v1/users/@me/tokens/"+first["id"].(string), alice, nil))
	require.Equal(t, websocket.StatusCode(4010), c.closeCode())
	e.expect(401, e.do("GET", "/api/v1/users/@me", full, nil))
	e.expect(404, e.do("DELETE", "/api/v1/users/@me/tokens/"+first["id"].(string), alice, nil))

	// Administrator powers need the admin scope.
	plain := e.pat(admin, "read", "write")
	require.Equal(t, false, e.expect(200, e.do("GET", "/api/v1/users/@me", plain, nil)).obj(t)["is_instance_admin"])
	e.expect(403, e.do("PATCH", "/api/v1/instance", plain, map[string]any{"name": "Hijacked"}))
	adminToken := e.pat(admin, "read", "write", "admin")
	e.expect(200, e.do("PATCH", "/api/v1/instance", adminToken, map[string]any{"name": "Renamed"}))

	// Changing the password revokes personal access tokens.
	e.expect(200, e.do("GET", "/api/v1/users/@me", readOnly, nil))
	e.expect(204, e.do("POST", "/api/v1/users/@me/password", alice, map[string]any{
		"current_password": "alice-password-1", "new_password": "alice-password-2",
	}))
	e.expect(401, e.do("GET", "/api/v1/users/@me", readOnly, nil))
	require.Empty(t, e.expect(200, e.do("GET", "/api/v1/users/@me/tokens", alice, nil)).list(t))
}

func TestBotsAndSlashCommands(t *testing.T) {
	e := newEnv(t)
	owner := e.setup()
	alice, _ := e.register("alice")
	e.createPlace(owner, "guild", "public")
	general := e.channel(owner, "guild", map[string]any{"name": "general"})

	e.expect(422, e.do("POST", "/api/v1/applications", alice, map[string]any{"name": "Helper", "bot_username": "admin"}))
	e.expect(409, e.do("POST", "/api/v1/applications", alice, map[string]any{"name": "Helper", "bot_username": "Owner"}))
	app := e.expect(201, e.do("POST", "/api/v1/applications", alice, map[string]any{
		"name": "Helper", "description": "Answers commands", "bot_username": "helperbot",
	})).obj(t)
	appID := id(app)
	botToken := app["bot_token"].(string)
	require.True(t, strings.HasPrefix(botToken, "gtb_"))
	bot := app["bot"].(map[string]any)
	require.Equal(t, true, bot["bot"])
	require.Equal(t, "Helper", bot["display_name"])
	botID := id(bot)

	// Bots are a separate identity: no password login, no self-service membership.
	e.expect(401, e.do("POST", "/api/v1/auth/login", "", map[string]any{"login": "helperbot", "password": "whatever-password"}))
	me := e.expect(200, e.doAuth("GET", "/api/v1/users/@me", "Bot "+botToken, nil)).obj(t)
	require.Equal(t, true, me["bot"])
	require.Equal(t, "", me["email"])
	e.expect(403, e.do("POST", "/api/v1/places", botToken, map[string]any{"slug": "botland", "name": "Botland"}))
	e.expect(403, e.do("POST", "/api/v1/places/guild/join", botToken, nil))
	e.expect(403, e.do("DELETE", "/api/v1/users/@me", botToken, map[string]any{"password": "x"}))
	e.expect(403, e.do("POST", "/api/v1/applications", botToken, map[string]any{"name": "Spawn", "bot_username": "spawnbot"}))
	e.expect(403, e.do("POST", "/api/v1/users/@me/tokens", botToken, map[string]any{"name": "x", "scopes": []string{"read"}}))

	// Private applications can only be added by their owner.
	e.expect(404, e.do("POST", "/api/v1/places/guild/bots", owner, map[string]any{"application_id": appID}))
	e.expect(404, e.do("GET", "/api/v1/applications/"+appID, owner, nil))
	e.expect(200, e.do("PATCH", "/api/v1/applications/"+appID, alice, map[string]any{"is_public": true}))
	e.expect(403, e.do("PATCH", "/api/v1/applications/"+appID, owner, map[string]any{"name": "Mine"}))
	require.Equal(t, "Helper", e.expect(200, e.do("GET", "/api/v1/applications/"+appID, owner, nil)).obj(t)["name"])
	e.expect(200, e.do("POST", "/api/v1/places/guild/join", alice, nil))
	e.expect(403, e.do("POST", "/api/v1/places/guild/bots", alice, map[string]any{"application_id": appID}))
	member := e.expect(200, e.do("POST", "/api/v1/places/guild/bots", owner, map[string]any{"application_id": appID})).obj(t)
	require.Equal(t, true, member["user"].(map[string]any)["bot"])
	e.expect(200, e.do("POST", "/api/v1/places/guild/bots", owner, map[string]any{"application_id": appID}))
	audit := e.items(e.expect(200, e.do("GET", "/api/v1/places/guild/audit-log?action=member.bot_add", owner, nil)))
	require.Len(t, audit, 1)
	e.expect(422, e.do("POST", "/api/v1/places/guild/transfer", owner, map[string]any{"user_id": botID}))

	// The bot registers its commands with its own token.
	e.expect(422, e.do("PUT", "/api/v1/applications/"+appID+"/commands", botToken, []map[string]any{
		{"name": "Bad Name", "description": "x"},
	}))
	cmds := e.expect(200, e.do("PUT", "/api/v1/applications/"+appID+"/commands", botToken, []map[string]any{
		{"name": "roll", "description": "Roll dice", "options": []map[string]any{
			{"name": "sides", "description": "Sides", "type": "integer", "required": true},
			{"name": "label", "description": "Label", "type": "string"},
		}},
		{"name": "ping", "description": "Check the bot"},
	})).list(t)
	require.Len(t, cmds, 2)
	require.Equal(t, "ping", cmds[0].(map[string]any)["name"])
	e.expect(403, e.do("PUT", "/api/v1/applications/"+appID+"/commands", owner, []map[string]any{}))

	listed := e.expect(200, e.do("GET", "/api/v1/channels/"+id(general)+"/commands", alice, nil)).list(t)
	require.Len(t, listed, 2)
	require.Equal(t, "Helper", listed[0].(map[string]any)["application_name"])

	bc := e.connect(botToken)
	require.Equal(t, true, bc.ready["user"].(map[string]any)["bot"])
	invoke := func(token string, body map[string]any) resp {
		body["application_id"] = appID
		return e.do("POST", "/api/v1/channels/"+id(general)+"/interactions", token, body)
	}
	e.expect(422, invoke(alice, map[string]any{"command": "roll"}))
	e.expect(422, invoke(alice, map[string]any{"command": "roll", "options": map[string]any{"sides": 2.5}}))
	e.expect(422, invoke(alice, map[string]any{"command": "roll", "options": map[string]any{"sides": 6, "color": "red"}}))
	e.expect(404, invoke(alice, map[string]any{"command": "missing"}))
	inter := e.expect(202, invoke(alice, map[string]any{"command": "roll", "options": map[string]any{"sides": 20}})).obj(t)
	got := bc.expect("INTERACTION_CREATE", nil)
	require.Equal(t, inter["id"], got["id"])
	require.Equal(t, "roll", got["command"])
	require.Equal(t, 20.0, got["options"].(map[string]any)["sides"])
	require.Equal(t, "alice", got["user"].(map[string]any)["username"])

	// The bot answers with an ordinary message.
	reply := e.send(botToken, general, "You rolled 7")
	require.Equal(t, true, reply["author"].(map[string]any)["bot"])

	// A bot hidden from a channel offers no commands there.
	staff := e.channel(owner, "guild", map[string]any{"name": "staff"})
	e.expect(200, e.do("PUT", "/api/v1/channels/"+id(staff)+"/overwrites/"+e.defaultRoleID(owner, "guild"), owner,
		map[string]any{"allow": 0, "deny": permissions.ViewChannels}))
	require.Empty(t, e.expect(200, e.do("GET", "/api/v1/channels/"+id(staff)+"/commands", owner, nil)).list(t))
	e.expect(404, e.do("POST", "/api/v1/channels/"+id(staff)+"/interactions", owner, map[string]any{"application_id": appID, "command": "ping"}))

	// Moderation by a bot is attributed to its application in the audit log.
	role := e.expect(201, e.do("POST", "/api/v1/places/guild/roles", owner, map[string]any{
		"name": "Bots", "permissions": permissions.ManageMessages,
	})).obj(t)
	e.expect(200, e.do("PUT", "/api/v1/places/guild/members/"+botID+"/roles/"+id(role), owner, nil))
	spam := e.send(alice, general, "buy now")
	e.expect(204, e.do("DELETE", "/api/v1/messages/"+id(spam)+"?reason=spam", botToken, nil))
	entry := e.items(e.expect(200, e.do("GET", "/api/v1/places/guild/audit-log?action=message.delete", owner, nil)))[0]
	require.Equal(t, botID, entry["actor_id"])
	require.Equal(t, "bot", entry["metadata"].(map[string]any)["via"])
	require.Equal(t, appID, entry["metadata"].(map[string]any)["application_id"])

	// Resetting the token disconnects the bot and invalidates the old token.
	newToken := e.expect(200, e.do("POST", "/api/v1/applications/"+appID+"/bot/token", alice, nil)).obj(t)["bot_token"].(string)
	require.Equal(t, websocket.StatusCode(4010), bc.closeCode())
	e.expect(401, e.do("GET", "/api/v1/users/@me", botToken, nil))
	e.expect(200, e.do("GET", "/api/v1/users/@me", newToken, nil))

	// Deleting the application removes the bot from its places; its messages stay.
	e.expect(204, e.do("DELETE", "/api/v1/applications/"+appID, alice, nil))
	e.expect(401, e.do("GET", "/api/v1/users/@me", newToken, nil))
	e.expect(404, e.do("GET", "/api/v1/places/guild/members/"+botID, owner, nil))
	require.Equal(t, "You rolled 7", byID(e.messages(owner, general, ""), id(reply))["content"])
	require.Empty(t, e.expect(200, e.do("GET", "/api/v1/applications", alice, nil)).list(t))
	require.Equal(t, 2.0, e.expect(200, e.do("GET", "/api/v1/instance", "", nil)).obj(t)["stats"].(map[string]any)["users"])
}

type webhookHit struct {
	header http.Header
	body   map[string]any
	raw    []byte
}

type webhookReceiver struct {
	srv    *httptest.Server
	hits   chan webhookHit
	mu     sync.Mutex
	status int
}

func newWebhookReceiver(t *testing.T) *webhookReceiver {
	r := &webhookReceiver{hits: make(chan webhookHit, 64), status: http.StatusOK}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		raw, _ := io.ReadAll(req.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		r.mu.Lock()
		status := r.status
		r.mu.Unlock()
		w.WriteHeader(status)
		r.hits <- webhookHit{header: req.Header.Clone(), body: body, raw: raw}
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *webhookReceiver) setStatus(code int) {
	r.mu.Lock()
	r.status = code
	r.mu.Unlock()
}

// next waits for the next delivery of eventType, skipping others.
func (r *webhookReceiver) next(t *testing.T, eventType string) webhookHit {
	t.Helper()
	return r.nextMatching(t, eventType, nil)
}

// nextMatching waits for a delivery of eventType whose data satisfies match.
func (r *webhookReceiver) nextMatching(t *testing.T, eventType string, match func(map[string]any) bool) webhookHit {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case h := <-r.hits:
			data, _ := h.body["data"].(map[string]any)
			if h.body["type"] == eventType && (match == nil || match(data)) {
				return h
			}
		case <-deadline:
			t.Fatalf("timed out waiting for webhook %s", eventType)
		}
	}
}

func verifySignature(t *testing.T, h webhookHit, secret string) {
	t.Helper()
	sig := h.header.Get("Gotalk-Signature")
	tsPart, _, _ := strings.Cut(sig, ",")
	ts, err := strconv.ParseInt(strings.TrimPrefix(tsPart, "t="), 10, 64)
	require.NoError(t, err)
	require.InDelta(t, time.Now().Unix(), ts, 60)
	require.Equal(t, service.SignWebhook(secret, ts, h.raw), sig)
}

func allowPrivateWebhooks(o *envOpts) {
	o.mutate = func(c *config.Config) { c.Webhooks.AllowPrivateNetworks = true }
}

func TestWebhooks(t *testing.T) {
	e := newEnv(t, allowPrivateWebhooks)
	owner := e.setup()
	alice, aliceID := e.register("alice")
	e.createPlace(owner, "hooks", "public")
	e.expect(200, e.do("POST", "/api/v1/places/hooks/join", alice, nil))
	general := e.channel(owner, "hooks", map[string]any{"name": "general"})
	staff := e.channel(owner, "hooks", map[string]any{"name": "staff"})
	def := e.defaultRoleID(owner, "hooks")
	e.expect(200, e.do("PUT", "/api/v1/channels/"+id(staff)+"/overwrites/"+def, owner, map[string]any{"allow": 0, "deny": permissions.ViewChannels}))
	board := e.board(owner, "hooks", map[string]any{"name": "General", "slug": "general"})
	rcv := newWebhookReceiver(t)

	create := func(token string, events ...string) resp {
		return e.do("POST", "/api/v1/places/hooks/webhooks", token, map[string]any{"name": "CI", "url": rcv.srv.URL + "/hook", "events": events})
	}
	e.expect(403, create(alice, "message.create"))
	e.expect(422, create(owner, "message.explode"))
	e.expect(422, e.do("POST", "/api/v1/places/hooks/webhooks", owner, map[string]any{"name": "x", "url": "ftp://example.com", "events": []string{"ping"}}))

	// Privileged events need the matching permission, beyond MANAGE_WEBHOOKS.
	role := e.expect(201, e.do("POST", "/api/v1/places/hooks/roles", owner, map[string]any{"name": "Integrations", "permissions": permissions.ManageWebhooks})).obj(t)
	e.expect(200, e.do("PUT", "/api/v1/places/hooks/members/"+aliceID+"/roles/"+id(role), owner, nil))
	e.expect(403, create(alice, "moderation.action"))
	aliceHook := e.expect(201, create(alice, "member.join")).obj(t)
	e.expect(204, e.do("DELETE", "/api/v1/webhooks/"+id(aliceHook), alice, nil))

	hook := e.expect(201, create(owner, "message.create", "message.delete", "member.join", "topic.create", "post.create", "moderation.action")).obj(t)
	secret := hook["secret"].(string)
	require.True(t, strings.HasPrefix(secret, "whsec_"))
	hookID := id(hook)
	require.Nil(t, e.expect(200, e.do("GET", "/api/v1/webhooks/"+hookID, owner, nil)).obj(t)["secret"])
	require.Len(t, e.expect(200, e.do("GET", "/api/v1/places/hooks/webhooks", owner, nil)).list(t), 1)

	// A webhook that receives audit entries stays out of reach of managers without
	// VIEW_AUDIT_LOG: no payloads, no URL changes, not even in the list.
	e.expect(403, e.do("GET", "/api/v1/webhooks/"+hookID, alice, nil))
	e.expect(403, e.do("GET", "/api/v1/webhooks/"+hookID+"/deliveries", alice, nil))
	e.expect(403, e.do("PATCH", "/api/v1/webhooks/"+hookID, alice, map[string]any{"url": "https://attacker.example/"}))
	e.expect(403, e.do("DELETE", "/api/v1/webhooks/"+hookID, alice, nil))
	require.Empty(t, e.expect(200, e.do("GET", "/api/v1/places/hooks/webhooks", alice, nil)).list(t))

	// Ping, with signature and delivery headers.
	e.expect(202, e.do("POST", "/api/v1/webhooks/"+hookID+"/ping", owner, nil))
	ping := rcv.next(t, "ping")
	verifySignature(t, ping, secret)
	require.Equal(t, hookID, ping.header.Get("Gotalk-Webhook-Id"))
	require.Equal(t, "ping", ping.header.Get("Gotalk-Event"))
	require.Equal(t, "1", ping.header.Get("Gotalk-Delivery-Attempt"))
	require.Equal(t, ping.body["id"], ping.header.Get("Gotalk-Delivery-Id"))

	// Messages in channels @everyone can see are delivered; staff-only ones are not.
	e.send(owner, staff, "secret plans")
	m := e.send(alice, general, "hello hooks", map[string]any{"nonce": "n-1"})
	got := rcv.next(t, "message.create")
	verifySignature(t, got, secret)
	data := got.body["data"].(map[string]any)
	require.Equal(t, "hello hooks", data["content"])
	require.Nil(t, data["nonce"])
	require.Equal(t, id(m), data["id"])
	e.expect(204, e.do("DELETE", "/api/v1/messages/"+id(m), alice, nil))
	require.Equal(t, id(m), rcv.next(t, "message.delete").body["data"].(map[string]any)["id"])

	// Forum and membership events.
	topicID, _ := e.topic(alice, board, "Release notes", "v1 is out")
	created := rcv.next(t, "topic.create").body["data"].(map[string]any)
	require.Equal(t, topicID, created["topic"].(map[string]any)["id"])
	require.Equal(t, "v1 is out", created["post"].(map[string]any)["content"])
	e.reply(owner, topicID, "Congrats", "")
	require.Equal(t, "Congrats", rcv.next(t, "post.create").body["data"].(map[string]any)["post"].(map[string]any)["content"])
	bob, bobID := e.register("bob")
	e.expect(200, e.do("POST", "/api/v1/places/hooks/join", bob, nil))
	require.Equal(t, bobID, rcv.next(t, "member.join").body["data"].(map[string]any)["user"].(map[string]any)["id"])
	e.expect(204, e.do("DELETE", "/api/v1/places/hooks/members/"+bobID+"?reason=bye", owner, nil))
	kick := rcv.nextMatching(t, "moderation.action", func(d map[string]any) bool { return d["action"] == "member.kick" }).body["data"].(map[string]any)
	require.Equal(t, "bye", kick["reason"])
	require.Equal(t, bobID, kick["target_id"])

	// The delivery log records each attempt; nothing from the staff channel was queued.
	deliveries := e.items(e.expect(200, e.do("GET", "/api/v1/webhooks/"+hookID+"/deliveries", owner, nil)))
	require.NotEmpty(t, deliveries)
	for _, d := range deliveries {
		require.NotContains(t, string(mustMarshal(t, d["payload"])), "secret plans")
	}
	require.Eventually(t, func() bool {
		ds := e.items(e.expect(200, e.do("GET", "/api/v1/webhooks/"+hookID+"/deliveries?status=succeeded", owner, nil)))
		return len(ds) == len(deliveries)
	}, 10*time.Second, 50*time.Millisecond)

	// Failures are recorded and retried later; redelivery queues a fresh attempt.
	rcv.setStatus(http.StatusInternalServerError)
	e.send(owner, general, "will fail")
	rcv.next(t, "message.create")
	var failed map[string]any
	require.Eventually(t, func() bool {
		ds := e.items(e.expect(200, e.do("GET", "/api/v1/webhooks/"+hookID+"/deliveries?status=pending", owner, nil)))
		if len(ds) == 1 && ds[0]["attempts"] == 1.0 {
			failed = ds[0]
			return true
		}
		return false
	}, 10*time.Second, 50*time.Millisecond)
	require.Equal(t, 500.0, failed["response_status"])
	require.Contains(t, failed["error"], "HTTP 500")
	require.NotNil(t, failed["next_attempt_at"])
	rcv.setStatus(http.StatusNoContent)
	e.expect(202, e.do("POST", "/api/v1/webhooks/"+hookID+"/deliveries/"+id(failed)+"/redeliver", owner, nil))
	require.Equal(t, "will fail", rcv.next(t, "message.create").body["data"].(map[string]any)["content"])

	// Rotating the secret signs later deliveries with the new one.
	rotated := e.expect(200, e.do("POST", "/api/v1/webhooks/"+hookID+"/secret", owner, nil)).obj(t)["secret"].(string)
	require.NotEqual(t, secret, rotated)
	e.expect(202, e.do("POST", "/api/v1/webhooks/"+hookID+"/ping", owner, nil))
	verifySignature(t, rcv.next(t, "ping"), rotated)

	// Disabling drops the queue and stops new deliveries.
	updated := e.expect(200, e.do("PATCH", "/api/v1/webhooks/"+hookID, owner, map[string]any{"active": false, "events": []string{"member.join"}})).obj(t)
	require.Equal(t, false, updated["active"])
	require.Equal(t, []any{"member.join"}, updated["events"])
	require.Empty(t, e.items(e.expect(200, e.do("GET", "/api/v1/webhooks/"+hookID+"/deliveries?status=pending", owner, nil))))
	e.expect(409, e.do("POST", "/api/v1/webhooks/"+hookID+"/ping", owner, nil))
	e.expect(422, e.do("PATCH", "/api/v1/webhooks/"+hookID, owner, map[string]any{"events": []string{}}))

	// Non-members cannot even learn that a webhook exists. Once the webhook no longer receives
	// audit entries, plain MANAGE_WEBHOOKS is enough to manage it.
	e.expect(404, e.do("GET", "/api/v1/webhooks/"+hookID, bob, nil))
	e.expect(200, e.do("GET", "/api/v1/webhooks/"+hookID, alice, nil))
	require.Len(t, e.expect(200, e.do("GET", "/api/v1/places/hooks/webhooks", alice, nil)).list(t), 1)
	e.expect(200, e.do("DELETE", "/api/v1/places/hooks/members/"+aliceID+"/roles/"+id(role), owner, nil))
	e.expect(403, e.do("GET", "/api/v1/webhooks/"+hookID, alice, nil))
	e.expect(403, e.do("GET", "/api/v1/places/hooks/webhooks", alice, nil))
	actions := field[string](e.items(e.expect(200, e.do("GET", "/api/v1/places/hooks/audit-log?action=webhook", owner, nil))), "action")
	require.Contains(t, actions, "webhook.create")
	require.Contains(t, actions, "webhook.rotate_secret")
	e.expect(204, e.do("DELETE", "/api/v1/webhooks/"+hookID, owner, nil))
	e.expect(404, e.do("GET", "/api/v1/webhooks/"+hookID, owner, nil))
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	return raw
}

func TestWebhookURLsMustBePublic(t *testing.T) {
	e := newEnv(t)
	owner := e.setup()
	e.createPlace(owner, "safe", "public")
	for _, u := range []string{
		"http://127.0.0.1:8080/hook", "http://localhost/hook", "http://10.1.2.3/", "http://169.254.169.254/latest/meta-data",
		"http://[::1]/", "https://user:pass@example.com/",
	} {
		e.expect(422, e.do("POST", "/api/v1/places/safe/webhooks", owner, map[string]any{"name": "x", "url": u, "events": []string{"member.join"}}))
	}
	e.expect(201, e.do("POST", "/api/v1/places/safe/webhooks", owner, map[string]any{"name": "x", "url": "https://hooks.example.com/in", "events": []string{"member.join"}}))
}

func TestPoliciesAndConsent(t *testing.T) {
	e := newEnv(t)
	admin := e.setup()
	alice, _ := e.register("alice")

	require.Empty(t, e.expect(200, e.do("GET", "/api/v1/policies", "", nil)).list(t))
	e.expect(404, e.do("GET", "/api/v1/policies/terms", "", nil))
	e.expect(422, e.do("GET", "/api/v1/policies/cookies", "", nil))
	e.expect(403, e.do("POST", "/api/v1/policies/terms", alice, map[string]any{"content": "Be nice."}))
	e.expect(422, e.do("POST", "/api/v1/policies/terms", admin, map[string]any{"content": "x", "effective_at": time.Now().Add(-time.Hour)}))

	terms := e.expect(201, e.do("POST", "/api/v1/policies/terms", admin, map[string]any{
		"content": "# Terms\nBe nice.", "requires_consent": true,
	})).obj(t)
	require.Equal(t, 1.0, terms["version"])
	require.Equal(t, "Terms of Service", terms["title"])
	e.expect(201, e.do("POST", "/api/v1/policies/privacy", admin, map[string]any{"title": "Privacy", "content": "We keep little."}))
	e.expect(201, e.do("POST", "/api/v1/policies/guidelines", admin, map[string]any{
		"content": "Coming soon", "effective_at": time.Now().Add(time.Hour),
	}))
	require.Len(t, e.expect(200, e.do("GET", "/api/v1/policies", "", nil)).list(t), 2)
	e.expect(404, e.do("GET", "/api/v1/policies/guidelines", "", nil))
	require.Len(t, e.expect(200, e.do("GET", "/api/v1/policies/guidelines/versions", "", nil)).list(t), 1)
	require.Equal(t, "# Terms\nBe nice.", e.expect(200, e.do("GET", "/api/v1/policies/terms", "", nil)).obj(t)["content"])

	info := e.expect(200, e.do("GET", "/api/v1/instance", "", nil)).obj(t)["policies"].(map[string]any)
	require.Equal(t, e.srv.URL+"/api/v1/policies/terms", info["terms_url"])
	require.Nil(t, info["guidelines_url"])
	require.Len(t, info["current"], 2)

	// Registering with accept_policies records consent; older accounts are asked.
	r := e.expect(201, e.do("POST", "/api/v1/auth/register", "", map[string]any{
		"username": "bob", "email": "bob@example.com", "password": "bob-password-1", "accept_policies": true,
	})).obj(t)
	bob := r["access_token"].(string)
	status := e.expect(200, e.do("GET", "/api/v1/users/@me/consents", bob, nil)).obj(t)
	require.Empty(t, status["outstanding"])
	require.Equal(t, "terms", status["consents"].([]any)[0].(map[string]any)["purpose"])
	outstanding := e.expect(200, e.do("GET", "/api/v1/users/@me/consents", alice, nil)).obj(t)["outstanding"].([]any)
	require.Len(t, outstanding, 1)
	require.Equal(t, "terms", outstanding[0].(map[string]any)["kind"])

	rec := e.expect(201, e.do("POST", "/api/v1/users/@me/consents", alice, map[string]any{"purpose": "terms", "granted": true})).obj(t)
	require.Equal(t, 1.0, rec["policy_version"])
	require.Empty(t, e.expect(200, e.do("GET", "/api/v1/users/@me/consents", alice, nil)).obj(t)["outstanding"])

	// A new version needs fresh consent and shows up in the changelog.
	e.expect(201, e.do("POST", "/api/v1/policies/terms", admin, map[string]any{
		"content": "# Terms\nBe very nice.", "summary": "Raised the bar", "requires_consent": true,
	}))
	versions := e.expect(200, e.do("GET", "/api/v1/policies/terms/versions", "", nil)).list(t)
	require.Equal(t, []any{2.0, 1.0}, []any{versions[0].(map[string]any)["version"], versions[1].(map[string]any)["version"]})
	require.Equal(t, "Raised the bar", versions[0].(map[string]any)["summary"])
	require.Equal(t, "# Terms\nBe nice.", e.expect(200, e.do("GET", "/api/v1/policies/terms/versions/1", "", nil)).obj(t)["content"])
	require.Len(t, e.expect(200, e.do("GET", "/api/v1/users/@me/consents", alice, nil)).obj(t)["outstanding"], 1)

	// Other purposes, withdrawals and the history log.
	e.expect(201, e.do("POST", "/api/v1/users/@me/consents", alice, map[string]any{"purpose": "analytics", "granted": false}))
	e.expect(422, e.do("POST", "/api/v1/users/@me/consents", alice, map[string]any{"purpose": "analytics", "granted": true, "policy_version": 1}))
	e.expect(404, e.do("POST", "/api/v1/users/@me/consents", alice, map[string]any{"purpose": "terms", "granted": true, "policy_version": 9}))
	history := e.items(e.expect(200, e.do("GET", "/api/v1/users/@me/consents/history", alice, nil)))
	require.Equal(t, []string{"analytics", "terms"}, field[string](history, "purpose"))
	e.expect(403, e.do("POST", "/api/v1/users/@me/consents", e.pat(alice, "read", "write"), map[string]any{"purpose": "terms", "granted": true}))
}

func TestTransparencyReports(t *testing.T) {
	e := newEnv(t)
	owner := e.setup()
	alice, aliceID := e.register("alice")
	bob, bobID := e.register("bob")
	e.createPlace(owner, "open", "public")
	e.createPlace(owner, "hidden", "private")
	for _, tok := range []string{alice, bob} {
		e.expect(200, e.do("POST", "/api/v1/places/open/join", tok, nil))
	}
	e.expect(201, e.do("POST", "/api/v1/places/open/reports", bob, map[string]any{"user_id": aliceID, "reason": "harassment"}))
	e.expect(204, e.do("POST", "/api/v1/places/open/members/"+aliceID+"/warnings", owner, map[string]any{"reason": "Be civil"}))
	e.expect(204, e.do("DELETE", "/api/v1/places/open/members/"+bobID, owner, nil))

	check := func(r map[string]any) {
		reports := r["reports"].(map[string]any)
		require.Equal(t, 1.0, reports["total"])
		require.Equal(t, 1.0, reports["by_reason"].(map[string]any)["harassment"])
		require.Equal(t, 1.0, reports["by_status"].(map[string]any)["open"])
		actions := r["actions"].(map[string]any)
		require.Equal(t, 1.0, actions["member.warn"])
		require.Equal(t, 1.0, actions["member.kick"])
		require.Equal(t, 0.0, r["content_removed"])
	}
	instance := e.expect(200, e.do("GET", "/api/v1/transparency", "", nil)).obj(t)
	check(instance)
	require.Nil(t, instance["place_id"])
	check(e.expect(200, e.do("GET", "/api/v1/places/open/transparency", "", nil)).obj(t))

	e.expect(404, e.do("GET", "/api/v1/places/hidden/transparency", alice, nil))
	empty := e.expect(200, e.do("GET", "/api/v1/places/hidden/transparency", owner, nil)).obj(t)
	require.Equal(t, 0.0, empty["reports"].(map[string]any)["total"])

	past := time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339)
	older := time.Now().Add(-72 * time.Hour).UTC().Format(time.RFC3339)
	old := e.expect(200, e.do("GET", "/api/v1/transparency?since="+older+"&until="+past, "", nil)).obj(t)
	require.Equal(t, 0.0, old["reports"].(map[string]any)["total"])
	e.expect(422, e.do("GET", "/api/v1/transparency?since="+past+"&until="+older, "", nil))
	e.expect(422, e.do("GET", "/api/v1/transparency?since=2020-01-01T00:00:00Z", "", nil))
}

func TestCapabilityNegotiation(t *testing.T) {
	e := newEnv(t)
	e.setup()

	r := e.expect(200, e.do("GET", "/api/v1/instance", "", nil))
	require.Equal(t, "v1", r.Header.Get("Gotalk-Api-Version"))
	require.Equal(t, "default", r.Header.Get("X-RateLimit-Tier"))
	info := r.obj(t)
	api := info["api"].(map[string]any)
	require.Equal(t, "v1", api["min_version"])
	require.Equal(t, "v1", api["max_version"])
	require.Equal(t, "Gotalk-Api-Version", api["version_header"])
	require.Equal(t, []any{"read", "write", "gateway", "admin"}, api["token_scopes"])
	features := info["features"].(map[string]any)
	for _, f := range []string{"api_tokens", "bots", "webhooks", "policies", "transparency"} {
		require.Equal(t, true, features[f], f)
	}
	require.Equal(t, 4000.0, info["limits"].(map[string]any)["message_length"])
	require.Contains(t, info["webhooks"].(map[string]any)["events"], "message.create")

	req, err := http.NewRequest("GET", e.srv.URL+"/api/v1/instance", nil)
	require.NoError(t, err)
	req.Header.Set("Gotalk-Api-Version", "v2")
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = res.Body.Close()
	require.Equal(t, http.StatusBadRequest, res.StatusCode)

	limits := e.expect(200, e.do("GET", "/api/v1/rate-limits", "", nil)).list(t)
	require.Len(t, limits, 4)
	first := limits[0].(map[string]any)
	require.Equal(t, "default", first["tier"])
	require.Less(t, first["remaining"].(float64), first["limit"].(float64))

	// Permission bits include the new webhook permission.
	perms := e.expect(200, e.do("GET", "/api/v1/permissions", "", nil)).list(t)
	found := false
	for _, p := range perms {
		if p.(map[string]any)["name"] == "MANAGE_WEBHOOKS" {
			found = p.(map[string]any)["value"] == float64(permissions.ManageWebhooks)
		}
	}
	require.True(t, found)
}
