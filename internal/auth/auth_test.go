package auth

import (
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPasswordRoundTrip(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(hash, "$argon2id$v=19$m=19456,t=2,p=1$"))

	ok, err := VerifyPassword("correct horse battery staple", hash)
	require.NoError(t, err)
	assert.True(t, ok)

	ok, err = VerifyPassword("wrong", hash)
	require.NoError(t, err)
	assert.False(t, ok)

	other, err := HashPassword("correct horse battery staple")
	require.NoError(t, err)
	assert.NotEqual(t, hash, other, "salts must differ")
}

func TestVerifyRejectsMalformedHashes(t *testing.T) {
	for _, h := range []string{"", "plain", "$argon2i$v=19$m=1,t=1,p=1$a$b", "$argon2id$v=19$m=x$a$b"} {
		_, err := VerifyPassword("x", h)
		assert.ErrorIs(t, err, ErrInvalidHash, h)
	}
}

func TestAccessTokens(t *testing.T) {
	issuer := NewTokenIssuer([]byte("0123456789abcdef0123456789abcdef"), time.Minute)
	userID, sessionID := uuid.New(), uuid.New()

	token, exp, err := issuer.Issue(userID, sessionID)
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now().Add(time.Minute), exp, 2*time.Second)

	gotUser, gotSession, err := issuer.Parse(token)
	require.NoError(t, err)
	assert.Equal(t, userID, gotUser)
	assert.Equal(t, sessionID, gotSession)

	other := NewTokenIssuer([]byte("another-secret-another-secret-xx"), time.Minute)
	_, _, err = other.Parse(token)
	assert.ErrorIs(t, err, ErrInvalidToken, "wrong key")

	_, _, err = issuer.Parse(token + "x")
	assert.ErrorIs(t, err, ErrInvalidToken, "tampered signature")

	expired := NewTokenIssuer([]byte("0123456789abcdef0123456789abcdef"), -time.Minute)
	old, _, err := expired.Issue(userID, sessionID)
	require.NoError(t, err)
	_, _, err = issuer.Parse(old)
	assert.ErrorIs(t, err, ErrInvalidToken, "expired")
}

func TestAccessTokensRejectAlgNone(t *testing.T) {
	claims := AccessClaims{SessionID: uuid.New(), RegisteredClaims: jwt.RegisteredClaims{
		Issuer: issuer, Subject: uuid.NewString(), ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
	}}
	unsigned, err := jwt.NewWithClaims(jwt.SigningMethodNone, claims).SignedString(jwt.UnsafeAllowNoneSignatureType)
	require.NoError(t, err)

	_, _, err = NewTokenIssuer([]byte("0123456789abcdef0123456789abcdef"), time.Minute).Parse(unsigned)
	assert.ErrorIs(t, err, ErrInvalidToken)
}

func TestRefreshTokens(t *testing.T) {
	sessionID := uuid.New()
	token, hash, err := NewRefreshToken(sessionID)
	require.NoError(t, err)

	gotID, secret, err := SplitRefreshToken(token)
	require.NoError(t, err)
	assert.Equal(t, sessionID, gotID)
	assert.True(t, HashesEqual(hash, HashRefreshSecret(secret)))
	assert.False(t, HashesEqual(hash, HashRefreshSecret(secret+"x")))

	for _, bad := range []string{"", "no-dot", "not-a-uuid.secret", sessionID.String() + "."} {
		_, _, err := SplitRefreshToken(bad)
		assert.ErrorIs(t, err, ErrInvalidToken, bad)
	}
}

func TestRandomString(t *testing.T) {
	s, err := RandomString(64, Base62)
	require.NoError(t, err)
	assert.Len(t, s, 64)
	for _, r := range s {
		assert.Contains(t, Base62, string(r))
	}
	other, err := RandomString(64, Base62)
	require.NoError(t, err)
	assert.NotEqual(t, s, other)
}
