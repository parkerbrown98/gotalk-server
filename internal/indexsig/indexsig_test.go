package indexsig

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The vector is copied from gotalk-platform's packages/index-protocol/fixtures, so this
// test fails if the Go and TypeScript implementations ever disagree.
type vector struct {
	Seed, Method, Host, Path, Nonce, Body string
	PublicKey                             string `json:"public_key"`
	KeyID                                 string `json:"key_id"`
	Timestamp                             int64
	BodySHA256                            string `json:"body_sha256"`
	SigningString                         string `json:"signing_string"`
	ContentDigest                         string `json:"content_digest"`
	Signature                             string
}

func loadVector(t *testing.T) vector {
	raw, err := os.ReadFile("testdata/signing-vector.json")
	require.NoError(t, err)
	var v vector
	require.NoError(t, json.Unmarshal(raw, &v))
	return v
}

func TestSigningVector(t *testing.T) {
	v := loadVector(t)
	key, err := KeyFromSeed(v.Seed)
	require.NoError(t, err)
	require.Equal(t, v.KeyID, KeyID(key))
	require.Equal(t, v.PublicKey, KeyID(key))

	r := Request{Method: v.Method, Host: v.Host, Target: v.Path, Timestamp: v.Timestamp, Nonce: v.Nonce, KeyID: v.KeyID, Body: []byte(v.Body)}
	require.Equal(t, v.BodySHA256, BodyHash(r.Body))
	require.Equal(t, v.SigningString, r.String())
	require.Equal(t, v.ContentDigest, ContentDigest(r.Body))
	require.Equal(t, v.Signature, Sign(key, r))

	pub := key.Public().(ed25519.PublicKey)
	require.True(t, Verify(pub, r, v.Signature))
	for _, tamper := range []func(*Request){
		func(r *Request) { r.Body = []byte(`{"origin":"https://evil.example"}`) },
		func(r *Request) { r.Host = "gotalk.app" },
		func(r *Request) { r.Timestamp++ },
	} {
		bad := r
		tamper(&bad)
		require.False(t, Verify(pub, bad, v.Signature))
	}
}

func TestSignHTTP(t *testing.T) {
	v := loadVector(t)
	key, err := KeyFromSeed(v.Seed)
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPost, "https://gotalk.sh"+v.Path, bytes.NewReader([]byte(v.Body)))
	require.NoError(t, err)
	SignHTTP(req, []byte(v.Body), key, time.Unix(v.Timestamp, 0))
	require.Equal(t, v.KeyID, req.Header.Get(HeaderKeyID))
	require.Equal(t, v.ContentDigest, req.Header.Get(HeaderContentDigest))
	r := Request{Method: "POST", Host: "gotalk.sh", Target: v.Path, Timestamp: v.Timestamp,
		Nonce: req.Header.Get(HeaderNonce), KeyID: v.KeyID, Body: []byte(v.Body)}
	require.True(t, Verify(key.Public().(ed25519.PublicKey), r, req.Header.Get(HeaderSignature)))
}
