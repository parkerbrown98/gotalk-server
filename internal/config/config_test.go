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
	assert.False(t, cfg.Voice.Enabled(), "voice stays off until a LiveKit URL is set")
	assert.Equal(t, "local", cfg.Storage.Driver)
	assert.False(t, cfg.Mail.Enabled())
	assert.False(t, cfg.IsSet("mail.driver"), "empty values in the file do not count as configured")
}

func TestStorageMailAndUploadsConfig(t *testing.T) {
	cfg, err := load("", environ())
	require.NoError(t, err)
	assert.Equal(t, "local", cfg.Storage.Driver)
	assert.Equal(t, "data", cfg.Storage.LocalPath)
	assert.EqualValues(t, 8<<20, cfg.Uploads.MaxSize)
	assert.False(t, cfg.Mail.Enabled())
	assert.False(t, cfg.IsSet("storage.driver"), "defaults are not explicit")

	cfg, err = load("", environ(
		"GOTALK_STORAGE_DRIVER=s3",
		"GOTALK_STORAGE_S3_BUCKET=media",
		"GOTALK_STORAGE_S3_ENDPOINT=http://minio:9000",
		"GOTALK_STORAGE_S3_ACCESS_KEY_ID=key",
		"GOTALK_STORAGE_S3_SECRET_ACCESS_KEY=secret",
		"GOTALK_STORAGE_S3_FORCE_PATH_STYLE=true",
		"GOTALK_MAIL_DRIVER=smtp",
		"GOTALK_MAIL_FROM=Gotalk <noreply@example.com>",
		"GOTALK_MAIL_SMTP_HOST=mail.example.com",
		"GOTALK_MAIL_SMTP_PORT=2525",
		"GOTALK_UPLOADS_MAX_SIZE=1048576",
		// Compose passes optional variables through empty; they must not clobber defaults.
		"GOTALK_STORAGE_LOCAL_PATH=",
		"GOTALK_LOG_LEVEL=",
	))
	require.NoError(t, err)
	assert.Equal(t, "s3", cfg.Storage.Driver)
	assert.True(t, cfg.Storage.S3ForcePathStyle)
	assert.Equal(t, "data", cfg.Storage.LocalPath)
	assert.Equal(t, "info", cfg.Log.Level)
	assert.Equal(t, 2525, cfg.Mail.SMTPPort)
	assert.EqualValues(t, 1<<20, cfg.Uploads.MaxSize)
	assert.True(t, cfg.IsSet("storage.driver"))
	assert.True(t, cfg.IsSet("mail.driver"))
	assert.False(t, cfg.IsSet("storage.local_path"))

	_, err = load("", environ(
		"GOTALK_STORAGE_DRIVER=floppy",
		"GOTALK_MAIL_DRIVER=smtp",
		"GOTALK_UPLOADS_MAX_SIZE=10",
		"GOTALK_SERVER_CORS_ALLOWED_ORIGINS=https://app.example.com/,https://*.*.example.com",
	))
	require.Error(t, err)
	for _, want := range []string{"floppy", "mail.smtp_host", "uploads.max_size", "https://app.example.com/", "at most one wildcard"} {
		assert.ErrorContains(t, err, want)
	}
	_, err = load("", environ("GOTALK_SERVER_CORS_ALLOWED_ORIGINS=https://*.example.com,http://localhost:3000"))
	require.NoError(t, err)

	// The desktop app's webview origins can be allow-listed.
	_, err = load("", environ("GOTALK_SERVER_CORS_ALLOWED_ORIGINS=tauri://localhost,http://tauri.localhost,https://tauri.localhost"))
	require.NoError(t, err)
	_, err = load("", environ("GOTALK_SERVER_CORS_ALLOWED_ORIGINS=localhost:3000,tauri://"))
	require.Error(t, err)
}

func TestVoiceConfig(t *testing.T) {
	cfg, err := load("", environ())
	require.NoError(t, err)
	assert.False(t, cfg.Voice.Enabled())
	assert.Equal(t, 10*time.Minute, cfg.Voice.TokenTTL)

	// Credentials alone do not enable voice; the URL does.
	_, err = load("", environ("GOTALK_VOICE_LIVEKIT_API_KEY=key"))
	require.NoError(t, err)

	cfg, err = load("", environ(
		"GOTALK_VOICE_LIVEKIT_URL=wss://voice.example.com",
		"GOTALK_VOICE_LIVEKIT_API_URL=http://livekit:7880",
		"GOTALK_VOICE_LIVEKIT_API_KEY=key",
		"GOTALK_VOICE_LIVEKIT_API_SECRET=0123456789abcdef0123456789abcdef",
		"GOTALK_VOICE_TOKEN_TTL=2m",
	))
	require.NoError(t, err)
	assert.True(t, cfg.Voice.Enabled())
	assert.Equal(t, "http://livekit:7880", cfg.Voice.LiveKitAPIURL)
	assert.Equal(t, 2*time.Minute, cfg.Voice.TokenTTL)

	_, err = load("", environ(
		"GOTALK_VOICE_LIVEKIT_URL=ftp://voice.example.com",
		"GOTALK_VOICE_LIVEKIT_API_SECRET=short",
		"GOTALK_VOICE_JOIN_TIMEOUT=1s",
	))
	require.Error(t, err)
	for _, want := range []string{"voice.livekit_url", "voice.livekit_api_key", "voice.livekit_api_secret", "voice.join_timeout"} {
		assert.ErrorContains(t, err, want)
	}
}
