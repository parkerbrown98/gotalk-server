package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

var ErrInvalidToken = errors.New("auth: invalid token")

const issuer = "gotalk"

// AccessClaims are carried in short-lived JWT access tokens.
type AccessClaims struct {
	SessionID uuid.UUID `json:"sid"`
	jwt.RegisteredClaims
}

type TokenIssuer struct {
	secret []byte
	ttl    time.Duration
}

func NewTokenIssuer(secret []byte, ttl time.Duration) *TokenIssuer {
	return &TokenIssuer{secret: secret, ttl: ttl}
}

func (t *TokenIssuer) TTL() time.Duration { return t.ttl }

// Issue returns a signed access token for the given user/session and its expiry.
func (t *TokenIssuer) Issue(userID, sessionID uuid.UUID) (string, time.Time, error) {
	now := time.Now()
	exp := now.Add(t.ttl)
	claims := AccessClaims{
		SessionID: sessionID,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    issuer,
			Subject:   userID.String(),
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(exp),
		},
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(t.secret)
	return signed, exp, err
}

// Parse validates an access token and returns the user and session IDs it carries.
func (t *TokenIssuer) Parse(token string) (userID, sessionID uuid.UUID, err error) {
	var claims AccessClaims
	_, err = jwt.ParseWithClaims(token, &claims, func(*jwt.Token) (any, error) { return t.secret, nil },
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(issuer),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		return uuid.Nil, uuid.Nil, ErrInvalidToken
	}
	userID, err = uuid.Parse(claims.Subject)
	if err != nil || claims.SessionID == uuid.Nil {
		return uuid.Nil, uuid.Nil, ErrInvalidToken
	}
	return userID, claims.SessionID, nil
}

// NewRefreshToken returns an opaque "<sessionID>.<secret>" token and the hash to store.
// Embedding the session ID lets a stale (already rotated) token be detected as reuse.
func NewRefreshToken(sessionID uuid.UUID) (token string, hash []byte, err error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return "", nil, err
	}
	encoded := base64.RawURLEncoding.EncodeToString(secret)
	return sessionID.String() + "." + encoded, HashRefreshSecret(encoded), nil
}

// SplitRefreshToken returns the session ID and secret portion of a refresh token.
func SplitRefreshToken(token string) (uuid.UUID, string, error) {
	id, secret, ok := strings.Cut(token, ".")
	if !ok || secret == "" {
		return uuid.Nil, "", ErrInvalidToken
	}
	sessionID, err := uuid.Parse(id)
	if err != nil {
		return uuid.Nil, "", ErrInvalidToken
	}
	return sessionID, secret, nil
}

func HashRefreshSecret(secret string) []byte {
	sum := sha256.Sum256([]byte(secret))
	return sum[:]
}

func HashesEqual(a, b []byte) bool { return subtle.ConstantTimeCompare(a, b) == 1 }

// RandomString returns a URL-safe random string using the given alphabet.
func RandomString(n int, alphabet string) (string, error) {
	// Bytes at or above limit are discarded to avoid modulo bias.
	limit := 256 - (256 % len(alphabet))
	out := make([]byte, 0, n)
	buf := make([]byte, n)
	for len(out) < n {
		if _, err := rand.Read(buf); err != nil {
			return "", err
		}
		for _, b := range buf {
			if int(b) < limit && len(out) < n {
				out = append(out, alphabet[int(b)%len(alphabet)])
			}
		}
	}
	return string(out), nil
}

const Base62 = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// API token prefixes tell token kinds apart at a glance and help secret scanners find
// leaked tokens.
const (
	PersonalTokenPrefix = "gtp_"
	BotTokenPrefix      = "gtb_"
)

// NewAPIToken returns a random API token with the given prefix, the hash to store, and a
// short hint that identifies the token in listings without revealing it.
func NewAPIToken(prefix string) (token string, hash []byte, hint string, err error) {
	secret, err := RandomString(40, Base62)
	if err != nil {
		return "", nil, "", err
	}
	token = prefix + secret
	return token, HashAPIToken(token), token[:len(prefix)+4], nil
}

// IsAPIToken reports whether token looks like an API token rather than a JWT.
func IsAPIToken(token string) bool {
	return strings.HasPrefix(token, PersonalTokenPrefix) || strings.HasPrefix(token, BotTokenPrefix)
}

// HashAPIToken is the lookup key stored for an API token. Tokens carry 238 bits of
// entropy, so a plain SHA-256 is enough.
func HashAPIToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}
