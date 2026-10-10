// Package sigv4 implements AWS Signature Version 4 request signing, shared by the S3
// storage driver and the Amazon SES mail driver. It covers what those need: signed
// headers and presigned query strings, without pulling in the AWS SDK.
package sigv4

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	algorithm = "AWS4-HMAC-SHA256"
	// UnsignedPayload skips hashing the body; S3 accepts it (over TLS it is still
	// integrity-protected by the transport).
	UnsignedPayload = "UNSIGNED-PAYLOAD"
	// EmptyPayloadHash is the SHA-256 of an empty body.
	EmptyPayloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	timeFormat       = "20060102T150405Z"
	dateFormat       = "20060102"
)

// Credentials identify the signer.
type Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
	// SessionToken is sent as X-Amz-Security-Token when set (temporary credentials).
	SessionToken string
}

// Signer signs requests for one service in one region.
type Signer struct {
	Credentials Credentials
	Region      string
	Service     string
}

// HashPayload returns the hex SHA-256 of body, for the X-Amz-Content-Sha256 header.
func HashPayload(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// Sign adds the Authorization and X-Amz-Date headers (plus X-Amz-Content-Sha256 for S3)
// to req.
// payloadHash is HashPayload(body), EmptyPayloadHash or UnsignedPayload. The Host header
// and every X-Amz-* and Content-* header present are signed.
func (s Signer) Sign(req *http.Request, payloadHash string, now time.Time) {
	now = now.UTC()
	amzDate := now.Format(timeFormat)
	req.Header.Set("X-Amz-Date", amzDate)
	if s.Service == "s3" {
		// Only S3 requires the payload hash header; other services just sign the hash.
		req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	}
	if s.Credentials.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", s.Credentials.SessionToken)
	}

	headers := map[string]string{"host": hostOf(req)}
	for name, values := range req.Header {
		lower := strings.ToLower(name)
		if strings.HasPrefix(lower, "x-amz-") || strings.HasPrefix(lower, "content-") {
			trimmed := make([]string, len(values))
			for i, v := range values {
				trimmed[i] = strings.Join(strings.Fields(v), " ")
			}
			headers[lower] = strings.Join(trimmed, ",")
		}
	}
	names := make([]string, 0, len(headers))
	for n := range headers {
		names = append(names, n)
	}
	slices.Sort(names)
	var canonHeaders strings.Builder
	for _, n := range names {
		canonHeaders.WriteString(n + ":" + headers[n] + "\n")
	}
	signedHeaders := strings.Join(names, ";")

	canonical := strings.Join([]string{
		req.Method,
		CanonicalPath(req.URL),
		CanonicalQuery(req.URL.Query()),
		canonHeaders.String(),
		signedHeaders,
		payloadHash,
	}, "\n")
	scope := s.scope(now)
	sig := s.signature(now, stringToSign(amzDate, scope, canonical))
	req.Header.Set("Authorization", algorithm+" Credential="+s.Credentials.AccessKeyID+"/"+scope+
		", SignedHeaders="+signedHeaders+", Signature="+sig)
}

// Presign returns a URL for method on u that is valid for expires without further
// credentials. Only the host header is signed.
func (s Signer) Presign(method string, u *url.URL, expires time.Duration, now time.Time) *url.URL {
	now = now.UTC()
	amzDate := now.Format(timeFormat)
	scope := s.scope(now)
	out := *u
	q := out.Query()
	q.Set("X-Amz-Algorithm", algorithm)
	q.Set("X-Amz-Credential", s.Credentials.AccessKeyID+"/"+scope)
	q.Set("X-Amz-Date", amzDate)
	q.Set("X-Amz-Expires", strconv.FormatInt(int64(expires/time.Second), 10))
	q.Set("X-Amz-SignedHeaders", "host")
	if s.Credentials.SessionToken != "" {
		q.Set("X-Amz-Security-Token", s.Credentials.SessionToken)
	}
	canonical := strings.Join([]string{
		method,
		CanonicalPath(&out),
		CanonicalQuery(q),
		"host:" + out.Host + "\n",
		"host",
		UnsignedPayload,
	}, "\n")
	q.Set("X-Amz-Signature", s.signature(now, stringToSign(amzDate, scope, canonical)))
	out.RawQuery = CanonicalQuery(q)
	return &out
}

func (s Signer) scope(now time.Time) string {
	return now.Format(dateFormat) + "/" + s.Region + "/" + s.Service + "/aws4_request"
}

func (s Signer) signature(now time.Time, toSign string) string {
	key := hmacSHA256([]byte("AWS4"+s.Credentials.SecretAccessKey), now.Format(dateFormat))
	key = hmacSHA256(key, s.Region)
	key = hmacSHA256(key, s.Service)
	key = hmacSHA256(key, "aws4_request")
	return hex.EncodeToString(hmacSHA256(key, toSign))
}

func stringToSign(amzDate, scope, canonical string) string {
	sum := sha256.Sum256([]byte(canonical))
	return algorithm + "\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(sum[:])
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

func hostOf(req *http.Request) string {
	if req.Host != "" {
		return req.Host
	}
	return req.URL.Host
}

// CanonicalPath URI-encodes each path segment once (S3 style; slashes are kept).
func CanonicalPath(u *url.URL) string {
	p := u.Path
	if p == "" {
		return "/"
	}
	segs := strings.Split(p, "/")
	for i, seg := range segs {
		segs[i] = Escape(seg)
	}
	return strings.Join(segs, "/")
}

// CanonicalQuery sorts parameters and encodes them as SigV4 requires.
func CanonicalQuery(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var parts []string
	for _, k := range keys {
		vals := slices.Clone(q[k])
		slices.Sort(vals)
		for _, v := range vals {
			parts = append(parts, Escape(k)+"="+Escape(v))
		}
	}
	return strings.Join(parts, "&")
}

// Escape percent-encodes everything except the RFC 3986 unreserved characters.
func Escape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if ('A' <= c && c <= 'Z') || ('a' <= c && c <= 'z') || ('0' <= c && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
			continue
		}
		b.WriteString("%" + strings.ToUpper(hex.EncodeToString([]byte{c})))
	}
	return b.String()
}
