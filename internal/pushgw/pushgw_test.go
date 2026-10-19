package pushgw

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func b64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(s)
	require.NoError(t, err)
	return b
}

// TestSigningVector checks GOTALK-SIG-V1 against Appendix A of the gateway protocol, so the
// server and gateway cannot disagree.
func TestSigningVector(t *testing.T) {
	key, err := KeyFromSeed("AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8")
	require.NoError(t, err)
	assert.Equal(t, "A6EHv_POEL4dcN0Y50vAmWfk1jCbpQ1fHdyGZBJVMbg", PublicKey(key))
	body := []byte(`{"subscription_id":"sub_example","mode":"id_only","notification_id":"n_1"}`)
	r := SignedRequest{Method: "post", Host: "push.example", Target: "/v1/push", Timestamp: 1760719200,
		Nonce: "AAECAwQFBgcICQoLDA0ODw", KeyID: "key_example", Body: body}
	assert.Equal(t, "SAUX3kZI6oRsw2ZIemu0vm_ENHcqIi_nr3Mtwf_zBvo", BodyHash(body))
	assert.Equal(t, strings.Join([]string{"GOTALK-SIG-V1", "POST", "push.example", "/v1/push", "1760719200",
		"AAECAwQFBgcICQoLDA0ODw", "key_example", "SAUX3kZI6oRsw2ZIemu0vm_ENHcqIi_nr3Mtwf_zBvo"}, "\n"), r.String())
	sig := Sign(key, r)
	assert.Equal(t, "3-vRa4hvPv4thLZdk2BO6zN_MPuYP8x9sYVISDDvaA7eyStJU6eE_R3-lJsYlZ2Y8IpNzlOPgwEJ8kAPpba2Aw", sig)
	pub := key.Public().(ed25519.PublicKey)
	assert.True(t, Verify(pub, r, sig))
	r.Host = "other.example"
	assert.False(t, Verify(pub, r, sig), "the host is bound")

	_, err = KeyFromSeed("short")
	assert.Error(t, err)
}

// TestRFC8291Vector checks the encryptor against RFC 8291 Appendix A.
func TestRFC8291Vector(t *testing.T) {
	as, err := ecdh.P256().NewPrivateKey(b64(t, "yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw"))
	require.NoError(t, err)
	ua, err := ecdh.P256().NewPrivateKey(b64(t, "q1dXpw3UpT5VOmu_cf_v6ih07Aems3njxI-JWgLcM94"))
	require.NoError(t, err)
	auth := b64(t, "BTBZMqHH6r4Tts7J_aSIgg")
	plaintext := []byte("When I grow up, I want to be a watermelon")
	body, err := EncryptWith(as, b64(t, "DGv6ra1nlYgDCS1FRnbzlw"), ua.PublicKey().Bytes(), auth, plaintext, 0)
	require.NoError(t, err)
	assert.Equal(t, "DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A_yl95bQpu6cVPTpK4Mqgkf1CXztLVBSt2Ks3oZwbuwXPXLWyouBWLVWGNWQexSgSxsj_Qulcy4a-fN",
		base64.RawURLEncoding.EncodeToString(body))
	got, err := Decrypt(ua, auth, body)
	require.NoError(t, err)
	assert.Equal(t, plaintext, got)
}

func TestEncryptPadsAndRoundTrips(t *testing.T) {
	ua, err := ecdh.P256().GenerateKey(nil)
	require.NoError(t, err)
	auth := bytes.Repeat([]byte{7}, 16)
	for _, n := range []int{1, 255, 256, 700, 2048} {
		plaintext := bytes.Repeat([]byte("a"), n)
		body, err := Encrypt(ua.PublicKey().Bytes(), auth, plaintext)
		require.NoError(t, err, n)
		assert.Equal(t, 86+n+Padding(n)+1+16, len(body))
		padded := n + Padding(n)
		assert.True(t, padded%PadTo == 0 || padded == MaxPlaintext, "padded to a block, never past the limit")
		assert.LessOrEqual(t, padded, MaxPlaintext)
		assert.LessOrEqual(t, len(body), 2200, "within the gateway's body limit")
		got, err := Decrypt(ua, auth, body)
		require.NoError(t, err)
		assert.Equal(t, plaintext, got)
	}
	_, err = Encrypt(ua.PublicKey().Bytes(), auth, make([]byte, MaxPlaintext+1))
	assert.Error(t, err)
	assert.Error(t, ValidateDeviceKeys(make([]byte, 65), auth), "not on the curve")
	assert.Error(t, ValidateDeviceKeys(ua.PublicKey().Bytes(), auth[:15]))
}

// fakeGateway checks signatures with the key it registered and records pushes.
type fakeGateway struct {
	t      *testing.T
	pub    ed25519.PublicKey
	pushes [][]Push
	status int
	code   string
}

func (f *fakeGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	problem := func(status int, code string) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"code": code, "detail": "nope", "status": status})
	}
	reply := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	if r.URL.Path == "/.well-known/gotalk-gateway" {
		reply(map[string]any{"software": "gotalk-gateway", "api_versions": []string{"v1"}, "apps": []any{
			map[string]any{"id": "gotalk", "platforms": map[string]any{"web": map[string]any{"vapid_public_key": "BKey"}}}}})
		return
	}
	if r.URL.Path == "/v1/instances" {
		var in RegisterInput
		require.NoError(f.t, json.Unmarshal(body, &in))
		f.pub = b64(f.t, in.PublicKey)
		reply(Registration{InstanceID: "inst_1", KeyID: "key_1", Status: StatusPending})
		return
	}
	ts, _ := strconv.ParseInt(r.Header.Get(HeaderTimestamp), 10, 64)
	sr := SignedRequest{Method: r.Method, Host: r.Host, Target: r.URL.RequestURI(), Timestamp: ts,
		Nonce: r.Header.Get(HeaderNonce), KeyID: r.Header.Get(HeaderKeyID), Body: body}
	if sr.KeyID != "key_1" || !Verify(f.pub, sr, r.Header.Get(HeaderSignature)) {
		problem(401, CodeInvalidSignature)
		return
	}
	if len(body) > 0 && r.Header.Get(HeaderContentDigest) != ContentDigest(body) {
		problem(401, CodeInvalidSignature)
		return
	}
	if f.status != 0 {
		problem(f.status, f.code)
		return
	}
	switch {
	case r.URL.Path == "/v1/instances/inst_1/verify" || r.URL.Path == "/v1/instances/inst_1":
		reply(map[string]any{"instance_id": "inst_1", "status": StatusActive,
			"keys": []any{map[string]any{"key_id": "key_1", "status": "active"}}})
	case r.URL.Path == "/v1/push/batch":
		var in struct{ Pushes []Push }
		require.NoError(f.t, json.Unmarshal(body, &in))
		f.pushes = append(f.pushes, in.Pushes)
		results := make([]map[string]any, len(in.Pushes))
		for i, p := range in.Pushes {
			results[i] = map[string]any{"index": i, "status": "queued", "subscription_id": p.SubscriptionID, "push_id": "push_" + strconv.Itoa(i)}
			if p.SubscriptionID == "sub_gone0000" {
				results[i] = map[string]any{"index": i, "status": "rejected", "subscription_id": p.SubscriptionID,
					"error": map[string]any{"code": CodeSubscriptionGone, "message": "gone"}}
			}
		}
		w.WriteHeader(http.StatusAccepted)
		reply(map[string]any{"results": results})
	case r.Method == http.MethodDelete && r.URL.Path == "/v1/devices/sub_missing":
		problem(404, CodeSubscriptionNotFound)
	case r.Method == http.MethodDelete:
		w.WriteHeader(http.StatusNoContent)
	default:
		problem(404, "not_found")
	}
}

func TestClient(t *testing.T) {
	fake := &fakeGateway{t: t}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	c := New(srv.URL+"/", Options{})
	ctx := context.Background()

	d, err := c.Discover(ctx)
	require.NoError(t, err)
	assert.Equal(t, "BKey", d.Apps[0].Platforms.Web.VAPIDPublicKey)

	_, priv, _ := ed25519.GenerateKey(nil)
	reg, err := c.Register(ctx, RegisterInput{Origin: "https://forum.example", PublicKey: PublicKey(priv), ContactEmail: "ops@forum.example"})
	require.NoError(t, err)
	creds := Credentials{KeyID: reg.KeyID, Key: priv}
	inst, err := c.Verify(ctx, creds, reg.InstanceID)
	require.NoError(t, err)
	assert.Equal(t, StatusActive, inst.Status)
	assert.Equal(t, "active", inst.KeyStatus("key_1"))
	_, err = c.GetInstance(ctx, creds, reg.InstanceID)
	require.NoError(t, err)

	ttl := 60
	results, err := c.PushBatch(ctx, creds, []Push{
		{SubscriptionID: "sub_ok000000", Mode: ModeIDOnly, NotificationID: "n1", TTL: &ttl},
		{SubscriptionID: "sub_gone0000", Mode: ModeIDOnly, NotificationID: "n1"},
	})
	require.NoError(t, err)
	assert.Equal(t, "queued", results[0].Status)
	assert.Equal(t, CodeSubscriptionGone, results[1].Error.Code)
	require.Len(t, fake.pushes, 1)
	assert.Equal(t, 60, *fake.pushes[0][0].TTL)

	require.NoError(t, c.DeleteDevice(ctx, creds, "sub_ok000000"))
	require.NoError(t, c.DeleteDevice(ctx, creds, "sub_missing"), "an unknown subscription is already gone")

	_, otherKey, _ := ed25519.GenerateKey(nil)
	_, err = c.GetInstance(ctx, Credentials{KeyID: "key_1", Key: otherKey}, "inst_1")
	e, ok := AsError(err)
	require.True(t, ok, err)
	assert.Equal(t, CodeInvalidSignature, e.Code)
	assert.False(t, e.Temporary())

	fake.status, fake.code = 429, CodeRateLimited
	_, err = c.PushBatch(ctx, creds, []Push{{SubscriptionID: "sub_ok000000", Mode: ModeIDOnly, NotificationID: "n2"}})
	e, ok = AsError(err)
	require.True(t, ok)
	assert.True(t, e.Temporary())
	assert.Equal(t, 7*time.Second, e.RetryAfter)

	_, err = c.PushBatch(ctx, creds, nil)
	assert.Error(t, err)
}

func TestDiscoverRefusesOtherSoftware(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"software":"something-else","api_versions":["v1"]}`))
	}))
	t.Cleanup(srv.Close)
	_, err := New(srv.URL, Options{}).Discover(context.Background())
	assert.ErrorContains(t, err, "not a Gotalk push gateway")
}
