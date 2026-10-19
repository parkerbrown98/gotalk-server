package api_test

import (
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/parkerbrown98/gotalk-server/internal/config"
	"github.com/parkerbrown98/gotalk-server/internal/pushgw"
	"github.com/parkerbrown98/gotalk-server/internal/service"
)

// fakeGateway plays a Gotalk push gateway: it registers the instance, verifies it through
// the instance's well-known document, checks every signature and records pushes.
type fakeGateway struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	origin   string
	pub      ed25519.PublicKey
	status   string
	pushes   []pushgw.Push
	deleted  []string
	reject   map[string]string // subscription → error code
	blockAll string            // request-level error code
	outage   bool              // answer every signed request with 503
}

func newFakeGateway(t *testing.T) *fakeGateway {
	f := &fakeGateway{t: t, reject: map[string]string{}, status: pushgw.StatusPending}
	f.srv = httptest.NewServer(f)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGateway) problem(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"status": status, "code": code, "detail": code})
}

func (f *fakeGateway) reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeGateway) instance() map[string]any {
	return map[string]any{"instance_id": "inst_fake", "origin": f.origin, "status": f.status,
		"keys": []any{map[string]any{"key_id": "key_fake", "status": map[bool]string{true: "active", false: "pending"}[f.status == "active"]}}}
}

func (f *fakeGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.URL.Path == "/.well-known/gotalk-gateway":
		f.reply(w, 200, map[string]any{"software": "gotalk-gateway", "name": "Fake Gateway", "version": "1", "api_versions": []string{"v1"},
			"apps": []any{map[string]any{"id": "gotalk", "name": "Gotalk", "platforms": map[string]any{
				"android": map[string]any{"package_name": "app.example"}, "web": map[string]any{"vapid_public_key": "BFakeVapidKey"}}}}})
		return
	case r.Method == http.MethodPost && r.URL.Path == "/v1/instances":
		var in pushgw.RegisterInput
		require.NoError(f.t, json.Unmarshal(body, &in))
		require.Equal(f.t, "gotalk", in.Software)
		require.Equal(f.t, "owner@example.com", in.ContactEmail, "defaults to the oldest administrator")
		raw, err := base64.RawURLEncoding.DecodeString(in.PublicKey)
		require.NoError(f.t, err)
		f.origin, f.pub = in.Origin, raw
		f.reply(w, 201, map[string]any{"instance_id": "inst_fake", "key_id": "key_fake", "status": "pending"})
		return
	}
	ts, _ := strconv.ParseInt(r.Header.Get(pushgw.HeaderTimestamp), 10, 64)
	sr := pushgw.SignedRequest{Method: r.Method, Host: r.Host, Target: r.URL.RequestURI(), Timestamp: ts,
		Nonce: r.Header.Get(pushgw.HeaderNonce), KeyID: r.Header.Get(pushgw.HeaderKeyID), Body: body}
	if f.pub == nil || sr.KeyID != "key_fake" || !pushgw.Verify(f.pub, sr, r.Header.Get(pushgw.HeaderSignature)) {
		f.problem(w, 401, pushgw.CodeInvalidSignature)
		return
	}
	if f.blockAll != "" {
		f.problem(w, 403, f.blockAll)
		return
	}
	if f.outage {
		f.problem(w, 503, pushgw.CodeProviderUnavailable)
		return
	}
	switch {
	case r.URL.Path == "/v1/instances/inst_fake/verify":
		// The well-known document must list the registered key.
		res, err := http.Get(f.origin + "/.well-known/gotalk-instance") //nolint:noctx // test helper
		require.NoError(f.t, err)
		var doc struct {
			Push struct {
				Keys []struct {
					Alg       string `json:"alg"`
					PublicKey string `json:"public_key"`
				} `json:"keys"`
			} `json:"push"`
		}
		require.NoError(f.t, json.NewDecoder(res.Body).Decode(&doc))
		_ = res.Body.Close()
		for _, k := range doc.Push.Keys {
			if k.Alg == "Ed25519" && k.PublicKey == base64.RawURLEncoding.EncodeToString(f.pub) {
				f.status = pushgw.StatusActive
			}
		}
		if f.status != pushgw.StatusActive {
			f.problem(w, 422, pushgw.CodeVerificationFailed)
			return
		}
		f.reply(w, 200, f.instance())
	case r.URL.Path == "/v1/instances/inst_fake":
		f.reply(w, 200, f.instance())
	case r.URL.Path == "/v1/push/batch":
		var in struct{ Pushes []pushgw.Push }
		require.NoError(f.t, json.Unmarshal(body, &in))
		results := make([]map[string]any, len(in.Pushes))
		for i, p := range in.Pushes {
			if code := f.reject[p.SubscriptionID]; code != "" {
				results[i] = map[string]any{"index": i, "status": "rejected", "subscription_id": p.SubscriptionID,
					"error": map[string]any{"code": code, "message": code}}
				continue
			}
			f.pushes = append(f.pushes, p)
			results[i] = map[string]any{"index": i, "status": "queued", "subscription_id": p.SubscriptionID, "push_id": "push_" + strconv.Itoa(len(f.pushes))}
		}
		f.reply(w, 202, map[string]any{"results": results})
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/devices/"):
		f.deleted = append(f.deleted, strings.TrimPrefix(r.URL.Path, "/v1/devices/"))
		w.WriteHeader(204)
	default:
		f.problem(w, 404, "not_found")
	}
}

func (f *fakeGateway) received() []pushgw.Push {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]pushgw.Push(nil), f.pushes...)
}

func (f *fakeGateway) deletions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.deleted...)
}

// waitPushes waits until the gateway received n pushes in total.
func (f *fakeGateway) waitPushes(n int) []pushgw.Push {
	f.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		got := f.received()
		if len(got) >= n || time.Now().After(deadline) {
			require.Len(f.t, got, n)
			return got
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// device is a phone's key material for one account.
type device struct {
	key  *ecdh.PrivateKey
	auth []byte
}

func newDevice(t *testing.T) device {
	k, err := ecdh.P256().GenerateKey(rand.Reader)
	require.NoError(t, err)
	auth := make([]byte, 16)
	_, _ = rand.Read(auth)
	return device{key: k, auth: auth}
}

func (d device) keys() map[string]any {
	return map[string]any{"p256dh": base64.RawURLEncoding.EncodeToString(d.key.PublicKey().Bytes()),
		"auth": base64.RawURLEncoding.EncodeToString(d.auth)}
}

// open decrypts a push the way the device does.
func (d device) open(t *testing.T, p pushgw.Push) map[string]any {
	t.Helper()
	body, err := base64.RawURLEncoding.DecodeString(p.Payload)
	require.NoError(t, err)
	plain, err := pushgw.Decrypt(d.key, d.auth, body)
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(plain, &m))
	return m
}

// pushEnv is an instance with push turned on against a fake gateway, verified.
func pushEnv(t *testing.T) (*env, *fakeGateway, string) {
	prevPoll, prevBackoff := service.PushPoll, service.PushBackoff
	service.PushPoll, service.PushBackoff = 50*time.Millisecond, []time.Duration{100 * time.Millisecond}
	t.Cleanup(func() { service.PushPoll, service.PushBackoff = prevPoll, prevBackoff })
	fake := newFakeGateway(t)
	e := newEnv(t, func(o *envOpts) {
		// The fake gateway fetches the instance's well-known document from its public URL.
		o.publicURL = true
		o.mutate = func(c *config.Config) {
			c.Push = config.Push{Enabled: true, GatewayURL: fake.srv.URL}
			c.MarkSet("push.enabled")
		}
	})
	owner := e.setup()
	st := e.expect(200, e.do("POST", "/api/v1/instance/push/verify", owner, nil)).obj(t)
	require.Equal(t, "active", st["status"], st)
	return e, fake, owner
}

func TestPushRegistrationAndMetadata(t *testing.T) {
	e, fake, owner := pushEnv(t)

	st := e.expect(200, e.do("GET", "/api/v1/instance/push", owner, nil)).obj(t)
	require.Equal(t, "inst_fake", st["instance_id"])
	require.Equal(t, "key_fake", st["key_id"])
	require.Equal(t, "generated", st["key_source"])
	require.Equal(t, fake.srv.URL, st["gateway_url"])
	require.Equal(t, e.srv.URL, st["origin"])
	require.Equal(t, "config", st["source"])

	wk := e.expect(200, e.do("GET", "/.well-known/gotalk-instance", "", nil)).obj(t)
	push := wk["push"].(map[string]any)
	require.Equal(t, st["public_key"], push["keys"].([]any)[0].(map[string]any)["public_key"])
	require.Equal(t, fake.srv.URL, push["gateway_url"])

	info := e.expect(200, e.do("GET", "/api/v1/instance", "", nil)).obj(t)
	ip := info["push"].(map[string]any)
	require.Equal(t, true, ip["enabled"])
	require.Equal(t, true, ip["available"])
	require.Equal(t, fake.srv.URL, ip["gateway_url"])
	require.Equal(t, true, info["features"].(map[string]any)["push"])
	require.Eventually(t, func() bool {
		apps := e.expect(200, e.do("GET", "/api/v1/instance", "", nil)).obj(t)["push"].(map[string]any)["web_apps"].([]any)
		return len(apps) == 1 && apps[0].(map[string]any)["vapid_public_key"] == "BFakeVapidKey"
	}, 5*time.Second, 50*time.Millisecond, "the gateway's VAPID key is advertised")

	checks := e.expect(200, e.do("GET", "/api/v1/instance/checks", owner, nil)).obj(t)["checks"].([]any)
	var pushCheck map[string]any
	for _, c := range checks {
		if c.(map[string]any)["name"] == "push" {
			pushCheck = c.(map[string]any)
		}
	}
	require.Equal(t, "ok", pushCheck["status"], pushCheck)

	cfg := e.expect(200, e.do("GET", "/api/v1/instance/config", owner, nil)).obj(t)
	require.Equal(t, true, cfg["push"].(map[string]any)["settings"].(map[string]any)["enabled"])
	e.expect(409, e.do("PATCH", "/api/v1/instance/config", owner, map[string]any{"push": map[string]any{"enabled": false}}))

	audit := e.expect(200, e.do("GET", "/api/v1/instance/audit-log", owner, nil)).list(t)
	actions := make([]string, len(audit))
	for i, a := range audit {
		actions[i] = a.(map[string]any)["action"].(string)
	}
	require.Contains(t, actions, "push.status", "status changes are audited")
	require.Contains(t, actions, "push.verify")

	// A gateway blip does not mark push unavailable.
	fake.mu.Lock()
	fake.outage = true
	fake.mu.Unlock()
	e.expect(503, e.do("POST", "/api/v1/instance/push/verify", owner, nil))
	fake.mu.Lock()
	fake.outage = false
	fake.mu.Unlock()
	require.Equal(t, "active", e.expect(200, e.do("GET", "/api/v1/instance/push", owner, nil)).obj(t)["status"])

	bob, _ := e.register("bob")
	e.expect(403, e.do("GET", "/api/v1/instance/push", bob, nil))
	e.expect(403, e.do("POST", "/api/v1/instance/push/verify", bob, nil))
}

func TestPushSubscriptionsAndDelivery(t *testing.T) {
	e, fake, owner := pushEnv(t)
	bob, bobID := e.register("bob")
	phone := newDevice(t)

	sub := func(token, id string, d device, extra map[string]any) resp {
		body := map[string]any{"subscription_id": id, "app_id": "gotalk", "platform": "android", "keys": d.keys(), "device_name": "Pixel"}
		for k, v := range extra {
			body[k] = v
		}
		return e.do("POST", "/api/v1/users/@me/push-subscriptions", token, body)
	}
	created := e.expect(201, sub(bob, "sub_bobphone01", phone, nil)).obj(t)
	require.Equal(t, true, created["current"])
	require.Equal(t, "encrypted", created["mode"])
	require.NotContains(t, created, "keys", "key material is never returned")

	e.expect(422, sub(bob, "nope", phone, nil))
	e.expect(422, sub(bob, "sub_bobphone02", phone, map[string]any{"keys": map[string]any{"p256dh": strings.Repeat("A", 87), "auth": "AAAAAAAAAAAAAAAAAAAAAA"}}))
	e.expect(422, sub(bob, "sub_bobweb0001", phone, map[string]any{"platform": "web", "mode": "id_only"}))
	e.expect(422, sub(bob, "sub_bobphone02", phone, map[string]any{"gateway_url": "https://other-gateway.example"}))
	e.expect(201, sub(bob, "sub_bobphone01", phone, map[string]any{"gateway_url": fake.srv.URL + "/"}))
	require.Len(t, e.expect(200, e.do("GET", "/api/v1/users/@me/push-subscriptions", bob, nil)).list(t), 1, "registering again updates")

	// A mention reaches bob's phone, encrypted to its key.
	e.createPlace(owner, "news", "public")
	e.expect(200, e.do("POST", "/api/v1/places/news/join", bob, nil))
	general := e.board(owner, "news", map[string]any{"slug": "general", "name": "General"})
	topicID, _ := e.topic(owner, general, "Launch day", "Ready, @bob? The doors open at nine.")
	got := fake.waitPushes(1)
	p := got[0]
	require.Equal(t, "sub_bobphone01", p.SubscriptionID)
	require.Equal(t, pushgw.ModeEncrypted, p.Mode)
	require.Equal(t, pushgw.UrgencyHigh, p.Urgency)
	require.Equal(t, 86400, *p.TTL)
	require.Regexp(t, `^t[A-Za-z0-9_-]{22}$`, p.CollapseKey, "collapse keys never carry IDs in the clear")
	plain := phone.open(t, p)
	require.EqualValues(t, 1, plain["v"])
	require.Equal(t, e.srv.URL, plain["instance"])
	require.Equal(t, bobID, plain["user_id"])
	require.Equal(t, p.NotificationID, plain["notification_id"])
	require.Equal(t, "mention", plain["type"])
	require.Equal(t, "mentions", plain["category"])
	require.Equal(t, "owner mentioned you in Launch day", plain["title"])
	require.Contains(t, plain["body"], "The doors open at nine")
	require.Equal(t, topicID, plain["thread_id"])
	require.Equal(t, topicID, plain["target"].(map[string]any)["topic_id"])
	require.EqualValues(t, 1, plain["badge"])

	// Direct messages too, with a per-conversation collapse key.
	dm := e.expect(200, e.do("POST", "/api/v1/users/@me/channels", owner, map[string]any{"recipient_ids": []string{bobID}})).obj(t)
	e.send(owner, dm, "lunch?")
	got = fake.waitPushes(2)
	dmPlain := phone.open(t, got[1])
	require.Equal(t, "direct_message", dmPlain["type"])
	require.Equal(t, "owner", dmPlain["title"])
	require.Equal(t, "lunch?", dmPlain["body"])
	require.Equal(t, dm["id"], dmPlain["target"].(map[string]any)["conversation_id"])
	require.NotEqual(t, p.CollapseKey, got[1].CollapseKey)

	// Turned-off categories and pauses hold pushes back; notifications still arrive.
	prefs := e.expect(200, e.do("PUT", "/api/v1/users/@me/push-preferences", bob, map[string]any{
		"categories": map[string]any{"mentions": false}, "paused_until": nil,
	})).obj(t)
	require.Equal(t, false, prefs["categories"].(map[string]any)["mentions"])
	require.Equal(t, true, prefs["categories"].(map[string]any)["replies"])
	require.NotContains(t, prefs, "quiet_hours")
	e.topic(owner, general, "Second", "@bob again")
	e.waitOutboxEmpty()
	require.Len(t, fake.received(), 2, "mentions are off")
	e.expect(422, e.do("PUT", "/api/v1/users/@me/push-preferences", bob, map[string]any{"categories": map[string]any{"memes": false}}))
	e.expect(422, e.do("PUT", "/api/v1/users/@me/push-preferences", bob, map[string]any{
		"categories": map[string]any{}, "quiet_hours": map[string]any{"start": "22:00", "end": "07:00", "timezone": "Mars/Olympus"}}))
	e.expect(200, e.do("PUT", "/api/v1/users/@me/push-preferences", bob, map[string]any{
		"categories": map[string]any{}, "paused_until": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}))
	e.topic(owner, general, "Third", "@bob paused?")
	e.waitOutboxEmpty()
	require.Len(t, fake.received(), 2, "paused")
	e.expect(200, e.do("PUT", "/api/v1/users/@me/push-preferences", bob, map[string]any{"categories": map[string]any{}}))

	// A second account on the same phone shares the gateway subscription with its own keys.
	carol, carolID := e.register("carol")
	e.expect(200, e.do("POST", "/api/v1/places/news/join", carol, nil))
	carolKeys := newDevice(t)
	carolSub := e.expect(201, sub(carol, "sub_bobphone01", carolKeys, nil)).obj(t)
	e.topic(owner, general, "Fourth", "@bob and @carol")
	got = fake.waitPushes(4)
	opened := map[string]map[string]any{}
	for _, q := range got[2:] {
		for _, d := range []device{phone, carolKeys} {
			body, _ := base64.RawURLEncoding.DecodeString(q.Payload)
			if plainText, err := pushgw.Decrypt(d.key, d.auth, body); err == nil {
				var m map[string]any
				require.NoError(t, json.Unmarshal(plainText, &m))
				opened[m["user_id"].(string)] = m
			}
		}
	}
	require.Contains(t, opened, bobID)
	require.Contains(t, opened, carolID, "each account's push opens only with its own keys")

	// Removing one account keeps the shared gateway subscription; removing the last deletes it.
	e.expect(204, e.do("DELETE", "/api/v1/users/@me/push-subscriptions/"+carolSub["id"].(string), carol, nil))
	require.Empty(t, fake.deletions())
	e.expect(404, e.do("DELETE", "/api/v1/users/@me/push-subscriptions/"+created["id"].(string), carol, nil))
	e.expect(204, e.do("DELETE", "/api/v1/users/@me/push-subscriptions/"+created["id"].(string), bob, nil))
	require.Equal(t, []string{"sub_bobphone01"}, fake.deletions())

	// A dead token removes the subscription for every account.
	e.expect(201, sub(bob, "sub_bobphone03", phone, nil))
	fake.mu.Lock()
	fake.reject["sub_bobphone03"] = pushgw.CodeSubscriptionGone
	fake.mu.Unlock()
	e.topic(owner, general, "Fifth", "@bob gone?")
	require.Eventually(t, func() bool {
		return len(e.expect(200, e.do("GET", "/api/v1/users/@me/push-subscriptions", bob, nil)).list(t)) == 0
	}, 10*time.Second, 50*time.Millisecond)

	// Signing out stops pushes to that session's devices.
	bob2 := e.login("bob", "bob-password-1")
	e.expect(201, sub(bob2, "sub_bobphone04", phone, nil))
	e.expect(204, e.do("POST", "/api/v1/auth/logout", bob2, map[string]any{}))
	before := len(fake.received())
	e.topic(owner, general, "Sixth", "@bob signed out")
	e.waitOutboxEmpty()
	require.Len(t, fake.received(), before)

	// API tokens cannot register devices.
	r := sub(e.pat(bob, "read", "write"), "sub_bobphone05", phone, nil)
	require.Contains(t, []int{401, 403}, r.Status, string(r.Raw))
}

func TestPushWaitsForVerificationAndRecoversFromBlocks(t *testing.T) {
	e, fake, owner := pushEnv(t)
	bob, _ := e.register("bob")
	phone := newDevice(t)
	e.expect(201, e.do("POST", "/api/v1/users/@me/push-subscriptions", bob, map[string]any{
		"subscription_id": "sub_bobphone01", "app_id": "gotalk", "platform": "ios", "keys": phone.keys(), "mode": "id_only"}))
	e.createPlace(owner, "news", "public")
	e.expect(200, e.do("POST", "/api/v1/places/news/join", bob, nil))
	general := e.board(owner, "news", map[string]any{"slug": "general", "name": "General"})

	// The gateway suspends the instance: the push is held and the problem surfaces.
	fake.mu.Lock()
	fake.blockAll = pushgw.CodeInstanceNotVerified
	fake.mu.Unlock()
	e.topic(owner, general, "Held", "@bob held")
	require.Eventually(t, func() bool {
		st := e.expect(200, e.do("GET", "/api/v1/instance/push", owner, nil)).obj(t)
		return st["status"] == "suspended"
	}, 10*time.Second, 50*time.Millisecond)
	info := e.expect(200, e.do("GET", "/api/v1/instance", "", nil)).obj(t)
	require.Equal(t, false, info["push"].(map[string]any)["available"])
	require.Contains(t, info["degraded_features"], "push")

	// Once the gateway accepts the instance again, the held push goes out (id_only: no payload).
	fake.mu.Lock()
	fake.blockAll = ""
	fake.mu.Unlock()
	e.expect(200, e.do("POST", "/api/v1/instance/push/verify", owner, nil))
	var pushes []pushgw.Push
	require.Eventually(t, func() bool {
		pushes = fake.received()
		return len(pushes) == 1
	}, 10*time.Second, 50*time.Millisecond)
	require.Equal(t, pushgw.ModeIDOnly, pushes[0].Mode)
	require.Empty(t, pushes[0].Payload)
}

func TestPushRegistersAfterSetup(t *testing.T) {
	fake := newFakeGateway(t)
	e := newEnv(t, func(o *envOpts) {
		o.publicURL = true
		o.mutate = func(c *config.Config) {
			c.Push = config.Push{Enabled: true, GatewayURL: fake.srv.URL}
			c.MarkSet("push.enabled")
		}
	})
	// Before setup there is no administrator to contact, so push only warns and waits.
	status := e.expect(200, e.do("GET", "/api/v1/setup/status", "", nil)).obj(t)
	for _, c := range status["checks"].([]any) {
		if c := c.(map[string]any); c["name"] == "push" {
			require.Equal(t, "warning", c["status"], c)
		}
	}
	fake.mu.Lock()
	require.Nil(t, fake.pub, "nothing registers before setup")
	fake.mu.Unlock()

	owner := e.setup()
	require.Eventually(t, func() bool {
		return e.expect(200, e.do("GET", "/api/v1/instance/push", owner, nil)).obj(t)["status"] == "active"
	}, 10*time.Second, 50*time.Millisecond, "completing setup registers with the gateway")
}

func TestPushRequiresEnabling(t *testing.T) {
	e := newEnv(t, func(o *envOpts) { o.publicURL = true })
	owner := e.setup()
	phone := newDevice(t)
	r := e.do("POST", "/api/v1/users/@me/push-subscriptions", owner, map[string]any{
		"subscription_id": "sub_ownerphone1", "app_id": "gotalk", "platform": "android", "keys": phone.keys()})
	e.expect(409, r)
	info := e.expect(200, e.do("GET", "/api/v1/instance", "", nil)).obj(t)
	require.Equal(t, false, info["push"].(map[string]any)["enabled"])
	_, has := e.expect(200, e.do("GET", "/.well-known/gotalk-instance", "", nil)).obj(t)["push"]
	require.False(t, has, "no push keys are published while push is off")
	e.expect(409, e.do("POST", "/api/v1/instance/push/verify", owner, nil))

	// The settings check reports an unreachable gateway before anything is saved.
	checks := e.expect(200, e.do("POST", "/api/v1/instance/config/test", owner, map[string]any{
		"settings": map[string]any{"push": map[string]any{"enabled": true, "gateway_url": "http://127.0.0.1:1"}}})).obj(t)["checks"].([]any)
	require.Equal(t, "error", checks[0].(map[string]any)["status"])
}

// waitOutboxEmpty waits until every queued push was handled.
func (e *env) waitOutboxEmpty() {
	e.t.Helper()
	require.Eventually(e.t, func() bool {
		var n int
		require.NoError(e.t, e.svc.Pool().QueryRow(context.Background(), "SELECT count(*) FROM push_outbox").Scan(&n))
		return n == 0
	}, 10*time.Second, 20*time.Millisecond)
}
