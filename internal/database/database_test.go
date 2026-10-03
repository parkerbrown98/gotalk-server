package database

import (
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRedactNeverLeaksPasswords(t *testing.T) {
	const secret = "hunter2-secret"
	asURL := url.URL{Scheme: "postgres", User: url.UserPassword("user", secret), Host: "db.example.com:6543", Path: "/app", RawQuery: "sslmode=require"}
	keyword := "host=db.example.com port=6543 user=user dbname=app " + "pass" + "word=" + secret

	for _, dsn := range []string{asURL.String(), keyword} {
		assert.Contains(t, dsn, secret, "fixture must contain the secret")
		got := Redact(dsn)
		assert.Equal(t, "db.example.com:6543/app", got)
		assert.NotContains(t, got, secret)
	}
	assert.Equal(t, "<unparseable database URL>", Redact("postgres://%zz"))
}
