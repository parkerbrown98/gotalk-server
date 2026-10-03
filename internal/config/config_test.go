package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func environ(vars ...string) func() []string {
	return func() []string { return vars }
}

func TestDefaultsAreValid(t *testing.T) {
	cfg, err := load("", environ())
	require.NoError(t, err)
	assert.Equal(t, ":8080", cfg.Server.Addr)
	assert.Equal(t, []string{"*"}, cfg.Server.CORSAllowedOrigins)
	assert.Equal(t, 15*time.Minute, cfg.Auth.AccessTokenTTL)
	assert.True(t, cfg.Database.AutoMigrate)
	assert.Contains(t, cfg.Database.URL, "localhost:5432/gotalk")
	assert.False(t, cfg.Setup.HasHeadlessAdmin())
}

func TestEnvironmentMapping(t *testing.T) {
	cfg, err := load("", environ(
		"GOTALK_DATABASE_URL=postgres://db/x",
		"GOTALK_AUTH_JWT_SECRET=0123456789abcdef0123456789abcdef",
		"GOTALK_AUTH_ACCESS_TOKEN_TTL=5m",
		"GOTALK_SERVER_CORS_ALLOWED_ORIGINS=https://a.example, https://b.example",
		"GOTALK_SERVER_TRUST_PROXY=true",
		"GOTALK_DATABASE_MAX_CONNS=7",
		"GOTALK_SETUP_ADMIN_USERNAME=root2",
		"GOTALK_SETUP_ADMIN_EMAIL=a@example.com",
		"GOTALK_SETUP_ADMIN_PASSWORD=supersecret!",
		"UNRELATED=1",
	))
	require.NoError(t, err)
	assert.Equal(t, "postgres://db/x", cfg.Database.URL)
	assert.Equal(t, "0123456789abcdef0123456789abcdef", cfg.Auth.JWTSecret)
	assert.Equal(t, 5*time.Minute, cfg.Auth.AccessTokenTTL)
	assert.Equal(t, []string{"https://a.example", "https://b.example"}, cfg.Server.CORSAllowedOrigins)
	assert.True(t, cfg.Server.TrustProxy)
	assert.EqualValues(t, 7, cfg.Database.MaxConns)
	assert.True(t, cfg.Setup.HasHeadlessAdmin())
}

func TestPlatformVariablesAreFallbacks(t *testing.T) {
	cfg, err := load("", environ("DATABASE_URL=postgres://platform/db", "REDIS_URL=redis://r:6379", "PORT=9000"))
	require.NoError(t, err)
	assert.Equal(t, "postgres://platform/db", cfg.Database.URL)
	assert.Equal(t, "redis://r:6379", cfg.Redis.URL)
	assert.Equal(t, ":9000", cfg.Server.Addr)

	cfg, err = load("", environ("DATABASE_URL=postgres://platform/db", "GOTALK_DATABASE_URL=postgres://explicit/db"))
	require.NoError(t, err)
	assert.Equal(t, "postgres://explicit/db", cfg.Database.URL, "GOTALK_ variables win over platform ones")
}

func TestFileSuffixReadsSecrets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pw")
	require.NoError(t, os.WriteFile(path, []byte("from-a-file-secret\n"), 0o600))

	cfg, err := load("", environ("GOTALK_SETUP_ADMIN_PASSWORD_FILE="+path))
	require.NoError(t, err)
	assert.Equal(t, "from-a-file-secret", cfg.Setup.AdminPassword)

	_, err = load("", environ("GOTALK_SETUP_ADMIN_PASSWORD_FILE="+filepath.Join(t.TempDir(), "missing")))
	assert.ErrorContains(t, err, "GOTALK_SETUP_ADMIN_PASSWORD_FILE")
}

func TestYAMLFileThenEnvOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gotalk.yaml")
	require.NoError(t, os.WriteFile(path, []byte(strings.Join([]string{
		"server:",
		"  addr: \":7000\"",
		"  public_url: https://forum.example.com/",
		"setup:",
		"  instance_name: From File",
	}, "\n")), 0o600))

	cfg, err := load(path, environ("GOTALK_SERVER_ADDR=:7001"))
	require.NoError(t, err)
	assert.Equal(t, ":7001", cfg.Server.Addr)
	assert.Equal(t, "https://forum.example.com", cfg.Server.PublicURL, "trailing slash is trimmed")
	assert.Equal(t, "From File", cfg.Setup.InstanceName)
}

func TestValidationReportsAllProblems(t *testing.T) {
	_, err := load("", environ(
		"GOTALK_SERVER_PUBLIC_URL=ftp://nope",
		"GOTALK_AUTH_JWT_SECRET=short",
		"GOTALK_SETUP_REGISTRATION_MODE=sometimes",
		"GOTALK_LOG_FORMAT=xml",
		"GOTALK_SERVER_CORS_ALLOW_CREDENTIALS=true",
	))
	require.Error(t, err)
	for _, want := range []string{"public_url", "jwt_secret", "registration_mode", "log.format", "cors_allow_credentials"} {
		assert.ErrorContains(t, err, want)
	}
}

func TestExampleConfigIsValid(t *testing.T) {
	cfg, err := load(filepath.Join("..", "..", "config.example.yaml"), environ())
	require.NoError(t, err)
	assert.Len(t, cfg.Server.TrustedProxyPrefixes(), 6)
	assert.Equal(t, defaults()["database.url"], cfg.Database.URL, "empty url in the example keeps the default")
}
