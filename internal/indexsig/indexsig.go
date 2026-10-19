// Package indexsig signs pings to the public index with GOTALK-INDEX-SIG-V1, the Ed25519
// request signature defined by the index protocol (gotalk-platform docs/index-protocol.md,
// "Ping"). It mirrors the push gateway's GOTALK-SIG-V1 with its own domain separator, and
// the key ID is the public key itself.
package indexsig

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

// Header names, shared with the gateway protocol.
const (
	HeaderKeyID         = "Gotalk-Key-Id"
	HeaderTimestamp     = "Gotalk-Timestamp"
	HeaderNonce         = "Gotalk-Nonce"
	HeaderSignature     = "Gotalk-Signature"
	HeaderContentDigest = "Content-Digest"
)

// Prefix is the domain separator, so index and gateway signatures cannot be swapped even
// when an instance uses one key for both.
const Prefix = "GOTALK-INDEX-SIG-V1"

var b64url = base64.RawURLEncoding

// Request holds the signed parts of an HTTP request.
type Request struct {
	Method string
	// Host is the index's host, as in the Host header.
	Host string
	// Target is the path and query exactly as sent.
	Target    string
	Timestamp int64
	Nonce     string
	KeyID     string
	Body      []byte
}

// BodyHash is the unpadded base64url SHA-256 of body.
func BodyHash(body []byte) string {
	sum := sha256.Sum256(body)
	return b64url.EncodeToString(sum[:])
}

// String builds the signing string: the fields joined by "\n" with no trailing newline.
func (r Request) String() string {
	return strings.Join([]string{
		Prefix,
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
func Sign(key ed25519.PrivateKey, r Request) string {
	return b64url.EncodeToString(ed25519.Sign(key, []byte(r.String())))
}

// Verify reports whether sig (base64url) is a valid signature of r by key.
func Verify(key ed25519.PublicKey, r Request, sig string) bool {
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

// KeyID is the key's public half in unpadded base64url, as published in the well-known
// document's directory.keys.
func KeyID(key ed25519.PrivateKey) string {
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
func SignHTTP(req *http.Request, body []byte, key ed25519.PrivateKey, now time.Time) {
	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	r := Request{
		Method:    req.Method,
		Host:      host,
		Target:    req.URL.RequestURI(),
		Timestamp: now.Unix(),
		Nonce:     NewNonce(),
		KeyID:     KeyID(key),
		Body:      body,
	}
	req.Header.Set(HeaderKeyID, r.KeyID)
	req.Header.Set(HeaderTimestamp, strconv.FormatInt(r.Timestamp, 10))
	req.Header.Set(HeaderNonce, r.Nonce)
	req.Header.Set(HeaderContentDigest, ContentDigest(body))
	req.Header.Set(HeaderSignature, Sign(key, r))
}
