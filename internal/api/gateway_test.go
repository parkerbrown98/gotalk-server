package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"

	"github.com/parkerbrown98/gotalk-server/internal/permissions"
)

type wsEvent struct {
	Op   string         `json:"op"`
	Type string         `json:"t"`
	Data map[string]any `json:"d"`
}

type wsClient struct {
	t      *testing.T
	conn   *websocket.Conn
	ready  map[string]any
	events chan wsEvent
	err    chan error
}

func (e *env) dial() *websocket.Conn {
	e.t.Helper()
	conn, status, err := dialGateway(e, nil)
	require.NoError(e.t, err)
	require.Equal(e.t, http.StatusSwitchingProtocols, status)
	e.t.Cleanup(func() { _ = conn.CloseNow() })
	return conn
}

// dialGateway connects to the gateway with optional extra headers and reports the
// handshake's HTTP status.
func dialGateway(e *env, header map[string][]string) (*websocket.Conn, int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	u := "ws" + strings.TrimPrefix(e.srv.URL, "http") + "/api/v1/gateway"
	conn, res, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPHeader: header})
	status := 0
	if res != nil {
		status = res.StatusCode
		if res.Body != nil {
			_ = res.Body.Close()
		}
	}
	return conn, status, err
}

func readFrame(t *testing.T, conn *websocket.Conn) wsEvent {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, raw, err := conn.Read(ctx)
	require.NoError(t, err)
	var ev wsEvent
	require.NoError(t, json.Unmarshal(raw, &ev))
	return ev
}

func writeFrame(t *testing.T, conn *websocket.Conn, v any) {
	t.Helper()
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, conn.Write(ctx, websocket.MessageText, raw))
}

// connect opens an identified gateway session and starts collecting its events.
func (e *env) connect(token string) *wsClient {
	e.t.Helper()
	conn := e.dial()
	hello := readFrame(e.t, conn)
	require.Equal(e.t, "hello", hello.Op)
	require.NotZero(e.t, hello.Data["heartbeat_interval"])
	writeFrame(e.t, conn, map[string]any{"op": "identify", "d": map[string]any{"token": token}})
	ready := readFrame(e.t, conn)
	require.Equal(e.t, "READY", ready.Type)

	c := &wsClient{t: e.t, conn: conn, ready: ready.Data, events: make(chan wsEvent, 256), err: make(chan error, 1)}
	go func() {
		for {
			_, raw, err := conn.Read(context.Background())
			if err != nil {
				c.err <- err
				close(c.events)
				return
			}
			var ev wsEvent
			if json.Unmarshal(raw, &ev) == nil {
				c.events <- ev
			}
		}
	}()
	return c
}

// expect waits for the next event of a type matching match, skipping others.
func (c *wsClient) expect(eventType string, match func(map[string]any) bool) map[string]any {
	c.t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-c.events:
			if !ok {
				c.t.Fatalf("connection closed while waiting for %s", eventType)
			}
			if ev.Type == eventType && (match == nil || match(ev.Data)) {
				return ev.Data
			}
		case <-deadline:
			c.t.Fatalf("timed out waiting for %s", eventType)
		}
	}
}

// waitMessage waits for a MESSAGE_CREATE with the given content, failing if a message from
// any forbidden channel arrives first.
func (c *wsClient) waitMessage(content string, forbidden ...string) map[string]any {
	c.t.Helper()
	for {
		d := c.expect("MESSAGE_CREATE", nil)
		for _, ch := range forbidden {
			require.NotEqual(c.t, ch, d["channel_id"], "received %q from a channel it should not see", d["content"])
		}
		if d["content"] == content {
			return d
		}
	}
}

func (c *wsClient) closeCode() websocket.StatusCode {
	c.t.Helper()
	for {
		select {
		case _, ok := <-c.events:
			if !ok {
				return websocket.CloseStatus(<-c.err)
			}
		case <-time.After(5 * time.Second):
			c.t.Fatal("connection did not close")
		}
	}
}

func userField(key, want string) func(map[string]any) bool {
	return func(d map[string]any) bool { return d[key] == want }
}

func TestGatewayHandshake(t *testing.T) {
	e := newEnv(t, func(o *envOpts) { o.heartbeat = 300 * time.Millisecond })
	owner := e.setup()

	// Identify must come first and carry a valid token.
	conn := e.dial()
	readFrame(t, conn)
	writeFrame(t, conn, map[string]any{"op": "heartbeat"})
	_, _, err := conn.Read(context.Background())
	require.Equal(t, websocket.StatusCode(4002), websocket.CloseStatus(err))

	conn = e.dial()
	readFrame(t, conn)
	writeFrame(t, conn, map[string]any{"op": "identify", "d": map[string]any{"token": "nope"}})
	_, _, err = conn.Read(context.Background())
	require.Equal(t, websocket.StatusCode(4004), websocket.CloseStatus(err))

	// Heartbeats are acknowledged; missing them closes the session.
	c := e.connect(owner)
	require.Equal(t, "owner", c.ready["user"].(map[string]any)["username"])
	require.Equal(t, []any{}, c.ready["place_ids"])
	writeFrame(t, c.conn, map[string]any{"op": "heartbeat"})
	for acked := false; !acked; {
		select {
		case ev := <-c.events:
			acked = ev.Op == "heartbeat_ack"
		case <-time.After(5 * time.Second):
			t.Fatal("no heartbeat ack")
		}
	}
	require.Equal(t, websocket.StatusCode(4009), c.closeCode())

	// Unknown ops are rejected.
	c = e.connect(owner)
	writeFrame(t, c.conn, map[string]any{"op": "dance"})
	require.Equal(t, websocket.StatusCode(4001), c.closeCode())

	// Browsers from origins outside the CORS list are refused.
	e.cfg.Server.CORSAllowedOrigins = []string{"https://app.example.com"}
	require.NoError(t, e.svc.ReloadProviders(context.Background()))
	_, status, err := dialGateway(e, map[string][]string{"Origin": {"https://evil.example"}})
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, status)
	conn, _, err = dialGateway(e, map[string][]string{"Origin": {"https://app.example.com"}})
	require.NoError(t, err)
	_ = conn.CloseNow()

	info := e.expect(200, e.do("GET", "/api/v1/instance", "", nil)).obj(t)
	require.Equal(t, true, info["features"].(map[string]any)["chat"])
	require.True(t, strings.HasPrefix(info["api"].(map[string]any)["gateway_url"].(string), "ws://"))
}

func TestGatewayEvents(t *testing.T) {
	e := newEnv(t)
	owner := e.setup()
	ownerID := e.expect(200, e.do("GET", "/api/v1/users/@me", owner, nil)).obj(t)["id"].(string)
	bob, bobID := e.register("bob")
	carol, carolID := e.register("carol")
	place := e.createPlace(owner, "live", "public")
	e.expect(200, e.do("POST", "/api/v1/places/live/join", bob, nil))
	e.expect(200, e.do("POST", "/api/v1/places/live/join", carol, nil))
	general := e.channel(owner, "live", map[string]any{"name": "general"})
	staff := e.channel(owner, "live", map[string]any{"name": "staff"})
	def := e.defaultRoleID(owner, "live")
	e.expect(200, e.do("PUT", "/api/v1/channels/"+id(staff)+"/overwrites/"+def, owner, map[string]any{"allow": 0, "deny": permissions.ViewChannels}))

	oc := e.connect(owner)
	bc := e.connect(bob)
	require.Equal(t, []any{id(place)}, bc.ready["place_ids"])
	oc.expect("PRESENCE_UPDATE", func(d map[string]any) bool { return d["user_id"] == bobID && d["status"] == "online" })
	pres := e.expect(200, e.do("GET", "/api/v1/presences?user_ids="+bobID, owner, nil)).list(t)
	require.Equal(t, "online", pres[0].(map[string]any)["status"])

	// Messages reach everyone who can see the channel, with the sender's nonce.
	e.send(owner, general, "hello live", map[string]any{"nonce": "n1"})
	got := bc.expect("MESSAGE_CREATE", nil)
	require.Equal(t, "hello live", got["content"])
	require.Equal(t, "n1", got["nonce"])
	require.Equal(t, "owner", got["author"].(map[string]any)["username"])
	oc.waitMessage("hello live")

	// Hidden channels stay hidden.
	e.send(owner, staff, "staff only")
	e.send(owner, general, "after staff")
	bc.waitMessage("after staff", id(staff))

	// Overwrite changes take effect immediately.
	e.expect(204, e.do("DELETE", "/api/v1/channels/"+id(staff)+"/overwrites/"+def, owner, nil))
	bc.expect("CHANNEL_UPDATE", userField("id", id(staff)))
	e.send(owner, staff, "staff is open")
	bc.waitMessage("staff is open")

	// New channels are announced to those who can see them, and so are deletions.
	random := e.channel(owner, "live", map[string]any{"name": "random"})
	require.Equal(t, "random", bc.expect("CHANNEL_CREATE", nil)["name"])
	e.expect(204, e.do("DELETE", "/api/v1/channels/"+id(random), owner, nil))
	bc.expect("CHANNEL_DELETE", userField("id", id(random)))

	// Typing, reactions, edits and deletes.
	e.expect(204, e.do("POST", "/api/v1/channels/"+id(general)+"/typing", bob, nil))
	oc.expect("TYPING_START", userField("user_id", bobID))
	m := e.send(bob, general, "react to me")
	e.expect(204, e.do("PUT", "/api/v1/messages/"+id(m)+"/reactions/heart", owner, nil))
	r := bc.expect("MESSAGE_REACTION_ADD", nil)
	require.Equal(t, "heart", r["emoji"])
	require.Equal(t, ownerID, r["user_id"])
	e.expect(200, e.do("PATCH", "/api/v1/messages/"+id(m), bob, map[string]any{"content": "edited"}))
	require.Equal(t, "edited", oc.expect("MESSAGE_UPDATE", userField("id", id(m)))["content"])
	e.expect(204, e.do("DELETE", "/api/v1/messages/"+id(m), bob, nil))
	oc.expect("MESSAGE_DELETE", userField("id", id(m)))

	// Notifications are pushed in real time.
	e.send(owner, general, "@bob look")
	require.Equal(t, "mention", bc.expect("NOTIFICATION_CREATE", nil)["kind"])

	// Direct messages only reach their participants; reads sync and send receipts.
	cc := e.connect(carol)
	dm := e.expect(200, e.do("POST", "/api/v1/users/@me/channels", owner, map[string]any{"recipient_ids": []string{carolID}})).obj(t)
	cc.expect("CHANNEL_CREATE", userField("id", id(dm)))
	dmMsg := e.send(owner, dm, "psst carol")
	cc.waitMessage("psst carol")
	require.Equal(t, "direct_message", cc.expect("NOTIFICATION_CREATE", nil)["kind"])
	e.send(owner, general, "public again")
	bc.waitMessage("public again", id(dm))
	e.expect(200, e.do("PUT", "/api/v1/channels/"+id(dm)+"/read", carol, map[string]any{"message_id": id(dmMsg)}))
	cc.expect("CHANNEL_READ", userField("channel_id", id(dm)))
	oc.expect("READ_RECEIPT", userField("user_id", carolID))

	// Clients set their own status.
	writeFrame(t, bc.conn, map[string]any{"op": "presence_update", "d": map[string]any{"status": "idle"}})
	oc.expect("PRESENCE_UPDATE", func(d map[string]any) bool { return d["user_id"] == bobID && d["status"] == "idle" })

	// Kicked members stop receiving the place's events.
	e.expect(204, e.do("DELETE", "/api/v1/places/live/members/"+carolID, owner, nil))
	cc.expect("PLACE_LEAVE", userField("place_id", id(place)))
	e.send(owner, general, "carol is gone")
	e.send(owner, dm, "still here?")
	cc.waitMessage("still here?", id(general))

	// Ending a session disconnects its gateway connection.
	e.expect(204, e.do("POST", "/api/v1/auth/logout", bob, nil))
	require.Equal(t, websocket.StatusCode(4010), bc.closeCode())
	oc.expect("PRESENCE_UPDATE", func(d map[string]any) bool { return d["user_id"] == bobID && d["status"] == "offline" })
}

func TestGatewayAcrossReplicas(t *testing.T) {
	rdb := sharedRedis(t)
	dbURL := freshDatabase(t)
	withShared := func(o *envOpts) { o.dbURL, o.redisURL = dbURL, rdb }
	a := newEnv(t, withShared)
	b := newEnv(t, withShared)
	owner := a.setup()
	bob, bobID := a.register("bob")
	a.createPlace(owner, "cluster", "public")
	a.expect(200, a.do("POST", "/api/v1/places/cluster/join", bob, nil))
	general := a.channel(owner, "cluster", map[string]any{"name": "general"})

	// bob is connected to replica B; the message is sent through replica A.
	bc := b.connect(bob)
	a.send(owner, general, "across the cluster")
	bc.waitMessage("across the cluster")

	// Presence is shared through Redis.
	pres := a.expect(200, a.do("GET", "/api/v1/presences?user_ids="+bobID, owner, nil)).list(t)
	require.Equal(t, "online", pres[0].(map[string]any)["status"])

	// So is session revocation.
	a.expect(204, a.do("POST", "/api/v1/auth/logout", bob, nil))
	require.Equal(t, websocket.StatusCode(4010), bc.closeCode())
}
