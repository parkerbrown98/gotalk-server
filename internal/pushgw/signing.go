// Package pushgw talks to a Gotalk push gateway (gotalk-gateway docs/protocol.md): request
// signing with GOTALK-SIG-V1, RFC 8291 payload encryption to a device's keys, and a client
// for instance registration, pushes and device deletion. It has no knowledge of the rest of
// the server.
package pushgw

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Signing header names.
const (
	HeaderKeyID         = "Gotalk-Key-Id"
	HeaderTimestamp     = "Gotalk-Timestamp"
	HeaderNonce         = "Gotalk-Nonce"
	HeaderSignature     = "Gotalk-Signature"
	HeaderContentDigest = "Content-Digest"
)

// SigPrefix is the gateway protocol's domain separator. Index pings use a different one,
// so the two signatures can never be swapped even when an instance reuses a key.
const SigPrefix = "GOTALK-SIG-V1"

var b64url = base64.RawURLEncoding

// SignedRequest holds the signed parts of an HTTP request.
type SignedRequest struct {
	Method string
	// Host is the gateway's Host header value.
	Host string
	// Target is the path and query exactly as sent.
	Target    string
	Timestamp int64
	Nonce     string
	// KeyID is the gateway's key_… ID for the signing key.
	KeyID string
	Body  []byte
}

// BodyHash is the unpadded base64url SHA-256 of body.
func BodyHash(body []byte) string {
	sum := sha256.Sum256(body)
	return b64url.EncodeToString(sum[:])
}

// String builds the signing string: the fields joined by "\n" with no trailing newline.
func (r SignedRequest) String() string {
	return strings.Join([]string{
		SigPrefix,
		strings.ToUpper(r.Method),
		strings.ToLower(r.Host),
		r.Target,
		strconv.FormatInt(r.Timestamp, 10),
		r.Nonce,
		r.KeyID,
		BodyHash(r.Body),
	}, "\n")
}

// Sign returns the base64url signature of r.
func Sign(key ed25519.PrivateKey, r SignedRequest) string {
	return b64url.EncodeToString(ed25519.Sign(key, []byte(r.String())))
}

// Verify reports whether sig is a valid signature of r by key.
func Verify(key ed25519.PublicKey, r SignedRequest, sig string) bool {
	raw, err := b64url.DecodeString(sig)
	return err == nil && len(key) == ed25519.PublicKeySize && ed25519.Verify(key, []byte(r.String()), raw)
}

// ContentDigest returns the RFC 9530 Content-Digest header value for body.
func ContentDigest(body []byte) string {
	sum := sha256.Sum256(body)
	return "sha-256=:" + base64.StdEncoding.EncodeToString(sum[:]) + ":"
}

// NewNonce returns 16 random bytes, base64url encoded.
func NewNonce() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return b64url.EncodeToString(b[:])
}

// PublicKey is the key's public half in unpadded base64url, as registered with the gateway
// and published in the well-known document's push.keys.
func PublicKey(key ed25519.PrivateKey) string {
	return b64url.EncodeToString(key.Public().(ed25519.PublicKey))
}

// KeyFromSeed decodes an unpadded base64url Ed25519 seed.
func KeyFromSeed(seed string) (ed25519.PrivateKey, error) {
	raw, err := b64url.DecodeString(seed)
	if err != nil || len(raw) != ed25519.SeedSize {
		return nil, errors.New("signing key must be a 32-byte Ed25519 seed in unpadded base64url")
	}
	return ed25519.NewKeyFromSeed(raw), nil
}

// SignHTTP signs req with key and sets the signing headers. body is the request body.
func SignHTTP(req *http.Request, body []byte, keyID string, key ed25519.PrivateKey, now time.Time) {
	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	r := SignedRequest{
		Method: req.Method, Host: host, Target: req.URL.RequestURI(), Timestamp: now.Unix(),
		Nonce: NewNonce(), KeyID: keyID, Body: body,
	}
	req.Header.Set(HeaderKeyID, keyID)
	req.Header.Set(HeaderTimestamp, strconv.FormatInt(r.Timestamp, 10))
	req.Header.Set(HeaderNonce, r.Nonce)
	if len(body) > 0 {
		req.Header.Set(HeaderContentDigest, ContentDigest(body))
	}
	req.Header.Set(HeaderSignature, Sign(key, r))
}
