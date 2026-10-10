package api_test

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/parkerbrown98/gotalk-server/internal/backup"
	"github.com/parkerbrown98/gotalk-server/internal/config"
	"github.com/parkerbrown98/gotalk-server/internal/mail"
	"github.com/parkerbrown98/gotalk-server/internal/service"
	"github.com/parkerbrown98/gotalk-server/internal/storage"
)

// The capture mail driver collects messages per mailbox (Options["box"]) so tests can read
// the emails an instance sends.
var (
	mailboxMu sync.Mutex
	mailboxes = map[string][]mail.Message{}
)

type captureSender struct{ box string }

func (c captureSender) Driver() string                  { return "capture" }
func (c captureSender) Describe() string                { return "capture " + c.box }
func (c captureSender) Check(ctx context.Context) error { return nil }
func (c captureSender) Send(_ context.Context, m mail.Message) error {
	mailboxMu.Lock()
	defer mailboxMu.Unlock()
	if strings.HasPrefix(m.To, "bounce@") {
		return fmt.Errorf("mailbox unavailable")
	}
	mailboxes[c.box] = append(mailboxes[c.box], m)
	return nil
}

func init() {
	mail.Register("capture", func(s mail.Settings, _ mail.Env) (mail.Sender, error) {
		return captureSender{box: s.Options["box"]}, nil
	})
}

func inbox(box string) []mail.Message {
	mailboxMu.Lock()
	defer mailboxMu.Unlock()
	return append([]mail.Message(nil), mailboxes[box]...)
}

// waitForMail waits until box holds n messages and returns the last one.
func waitForMail(t *testing.T, box string, n int) mail.Message {
	t.Helper()
	require.Eventually(t, func() bool { return len(inbox(box)) >= n }, 10*time.Second, 20*time.Millisecond, "expected %d emails", n)
	msgs := inbox(box)
	require.Len(t, msgs, n)
	return msgs[n-1]
}

func withMail(box string) func(*envOpts) {
	return func(o *envOpts) {
		prev := o.mutate
		o.mutate = func(c *config.Config) {
			if prev != nil {
				prev(c)
			}
			c.Mail = mail.Settings{Driver: "capture", From: "Gotalk <noreply@example.com>", Options: map[string]string{"box": box}}
		}
	}
}

var tokenInLink = regexp.MustCompile(`token=([A-Za-z0-9]+)`)

func linkToken(t *testing.T, m mail.Message, path string) string {
	t.Helper()
	require.Contains(t, m.Text, path+"?token=")
	match := tokenInLink.FindStringSubmatch(m.Text)
	require.NotNil(t, match)
	require.Contains(t, m.HTML, match[1], "the HTML part links to the same token")
	return match[1]
}

func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	img.Set(0, 0, color.RGBA{G: 255, A: 255})
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, img))
	return buf.Bytes()
}

func (e *env) raw(method, path, token, contentType string, body []byte) resp {
	e.t.Helper()
	req, err := http.NewRequest(method, e.srv.URL+path, bytes.NewReader(body))
	require.NoError(e.t, err)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := noRedirect.Do(req)
	require.NoError(e.t, err)
	defer func() { _ = res.Body.Close() }()
	data, err := io.ReadAll(res.Body)
	require.NoError(e.t, err)
	return resp{Status: res.StatusCode, Header: res.Header, Raw: data}
}

func (e *env) get(path string, headers map[string]string) resp {
	e.t.Helper()
	req, err := http.NewRequest(http.MethodGet, e.srv.URL+path, nil)
	require.NoError(e.t, err)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := noRedirect.Do(req)
	require.NoError(e.t, err)
	defer func() { _ = res.Body.Close() }()
	data, err := io.ReadAll(res.Body)
	require.NoError(e.t, err)
	return resp{Status: res.StatusCode, Header: res.Header, Raw: data}
}

func TestSetupWizardConfiguresProviders(t *testing.T) {
	box := t.Name()
	e := newEnv(t)
	ctx := context.Background()
	token, err := e.svc.SetupToken(ctx)
	require.NoError(t, err)

	// Without the token header the current settings stay private.
	status := e.expect(200, e.do("GET", "/api/v1/setup/status", "", nil)).obj(t)
	require.NotContains(t, status, "config")
	names := []string{}
	for _, c := range status["checks"].([]any) {
		names = append(names, c.(map[string]any)["name"].(string))
	}
	require.Equal(t, []string{"database", "redis", "public_url", "storage", "email", "voice"}, names)

	status = e.expect(200, e.get("/api/v1/setup/status", map[string]string{"Gotalk-Setup-Token": token})).obj(t)
	cfg := status["config"].(map[string]any)
	require.Equal(t, "default", cfg["storage"].(map[string]any)["source"])
	require.Equal(t, true, cfg["mail"].(map[string]any)["editable"])
	require.Contains(t, cfg["drivers"].(map[string]any)["mail"], "smtp")
	require.Contains(t, cfg["drivers"].(map[string]any)["storage"], "s3")

	// Live tests need the token and report per-section results.
	dir := t.TempDir()
	storageSettings := map[string]any{"driver": "local", "local_path": filepath.Join(dir, "media")}
	mailSettings := map[string]any{"driver": "capture", "from": "Forum <noreply@example.com>", "options": map[string]any{"box": box}}
	e.expect(403, e.do("POST", "/api/v1/setup/test", "", map[string]any{"setup_token": "nope", "settings": map[string]any{"storage": storageSettings}}))
	checks := e.expect(200, e.do("POST", "/api/v1/setup/test", "", map[string]any{
		"setup_token": token, "test_email_to": "admin@example.com",
		"settings": map[string]any{"storage": storageSettings, "mail": mailSettings},
	})).obj(t)["checks"].([]any)
	require.Len(t, checks, 2)
	for _, c := range checks {
		require.Equal(t, "ok", c.(map[string]any)["status"], c)
	}
	test := waitForMail(t, box, 1)
	require.Equal(t, "admin@example.com", test.To)
	require.Contains(t, test.Subject, "Test email")

	e.expect(422, e.do("POST", "/api/v1/setup/test", "", map[string]any{
		"setup_token": token, "settings": map[string]any{"storage": map[string]any{"driver": "s3"}},
	}))
	// A storage directory that cannot be created fails its check, so setup refuses it
	// unless told to skip checks.
	blocker := filepath.Join(dir, "file")
	require.NoError(t, os.WriteFile(blocker, []byte("x"), 0o600))
	body := map[string]any{
		"setup_token": token,
		"instance":    map[string]any{"name": "Configured"},
		"admin":       map[string]any{"username": "admin1", "email": "admin@example.com", "password": "admin-password-1"},
		"settings":    map[string]any{"storage": map[string]any{"driver": "local", "local_path": filepath.Join(blocker, "sub")}, "mail": mailSettings},
	}
	r := e.expect(422, e.do("POST", "/api/v1/setup", "", body))
	require.Contains(t, string(r.Raw), "storage check failed")
	required, err := e.svc.SetupRequired(ctx)
	require.NoError(t, err)
	require.True(t, required, "nothing was saved")

	body["settings"].(map[string]any)["storage"] = storageSettings
	res := e.expect(201, e.do("POST", "/api/v1/setup", "", body)).obj(t)
	admin := res["tokens"].(map[string]any)["access_token"].(string)
	features := res["instance"].(map[string]any)["features"].(map[string]any)
	require.Equal(t, true, features["email"])
	require.Equal(t, true, features["uploads"])
	require.Equal(t, "healthy", res["instance"].(map[string]any)["status"])

	conf := e.expect(200, e.do("GET", "/api/v1/instance/config", admin, nil)).obj(t)
	require.Equal(t, "settings", conf["storage"].(map[string]any)["source"])
	require.Equal(t, filepath.Join(dir, "media"), conf["storage"].(map[string]any)["settings"].(map[string]any)["local_path"])
	require.Equal(t, "settings", conf["mail"].(map[string]any)["source"])
	require.NotEmpty(t, conf["mail"].(map[string]any)["updated_by"])
	ready := e.expect(200, e.do("GET", "/readyz", "", nil)).obj(t)
	require.Equal(t, "ready", ready["status"], ready)
	require.Equal(t, "ok", ready["checks"].(map[string]any)["email"])

	// The settings page is the same document; status no longer leaks config.
	require.Contains(t, string(e.expect(200, e.do("GET", "/setup", "", nil)).Raw), "Instance settings")
}

func TestInstanceConfigAPI(t *testing.T) {
	e := newEnv(t, func(o *envOpts) {
		o.mutate = func(c *config.Config) { c.MarkSet("storage.driver") }
	})
	admin := e.setup()
	alice, _ := e.register("alice")

	e.expect(403, e.do("GET", "/api/v1/instance/config", alice, nil))
	e.expect(403, e.do("PATCH", "/api/v1/instance/config", alice, map[string]any{"cors": map[string]any{"allowed_origins": []string{"*"}}}))
	pat := e.pat(admin, "read", "write", "admin")
	e.expect(403, e.do("GET", "/api/v1/instance/config", pat, nil))

	conf := e.expect(200, e.do("GET", "/api/v1/instance/config", admin, nil)).obj(t)
	storageSec := conf["storage"].(map[string]any)
	require.Equal(t, "config", storageSec["source"])
	require.Equal(t, false, storageSec["editable"])
	require.Equal(t, "storage.driver", storageSec["config_key"])
	r := e.expect(409, e.do("PATCH", "/api/v1/instance/config", admin, map[string]any{"storage": map[string]any{"driver": "local", "local_path": t.TempDir()}}))
	require.Contains(t, string(r.Raw), "GOTALK_STORAGE_DRIVER")

	// Secrets are write-only: saved, kept when omitted, never returned.
	smtp := map[string]any{"driver": "smtp", "from": "Gotalk <noreply@example.com>", "smtp_host": "127.0.0.1", "smtp_port": 1,
		"smtp_tls": "none", "smtp_username": "u", "smtp_password": "hunter2-secret"}
	r = e.expect(422, e.do("PATCH", "/api/v1/instance/config", admin, map[string]any{"mail": smtp}))
	require.Contains(t, string(r.Raw), "email check failed", "nothing listens on port 1")
	saved := e.expect(200, e.do("PATCH", "/api/v1/instance/config?force=true", admin, map[string]any{"mail": smtp})).obj(t)
	mailSec := saved["config"].(map[string]any)["mail"].(map[string]any)
	require.Equal(t, "settings", mailSec["source"])
	require.Equal(t, []any{"smtp_password"}, mailSec["secrets_set"])
	require.NotContains(t, string(e.expect(200, e.do("GET", "/api/v1/instance/config", admin, nil)).Raw), "hunter2")
	delete(smtp, "smtp_password")
	smtp["smtp_port"] = 2
	saved = e.expect(200, e.do("PATCH", "/api/v1/instance/config?force=true", admin, map[string]any{"mail": smtp})).obj(t)
	require.Equal(t, []any{"smtp_password"}, saved["config"].(map[string]any)["mail"].(map[string]any)["secrets_set"])
	require.Equal(t, "hunter2-secret", e.svc.Providers().MailSettings.SMTPPassword)
	info := e.expect(200, e.do("GET", "/api/v1/instance", "", nil)).obj(t)
	require.Equal(t, true, info["features"].(map[string]any)["password_reset"])

	// Forgetting the section falls back to config/defaults (email off).
	reset := e.expect(200, e.do("DELETE", "/api/v1/instance/config/mail", admin, nil)).obj(t)
	require.Equal(t, "default", reset["mail"].(map[string]any)["source"])
	require.False(t, e.svc.MailEnabled())

	// CORS changes apply to HTTP and the gateway without a restart.
	preflight := func(origin string) string {
		req, err := http.NewRequest(http.MethodOptions, e.srv.URL+"/api/v1/instance", nil)
		require.NoError(t, err)
		req.Header.Set("Origin", origin)
		req.Header.Set("Access-Control-Request-Method", "GET")
		res, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = res.Body.Close()
		return res.Header.Get("Access-Control-Allow-Origin")
	}
	require.Equal(t, "*", preflight("https://anything.example"))
	e.expect(422, e.do("PATCH", "/api/v1/instance/config", admin, map[string]any{"cors": map[string]any{"allowed_origins": []string{"https://app.example.com/"}}}))
	e.expect(200, e.do("PATCH", "/api/v1/instance/config", admin, map[string]any{"cors": map[string]any{"allowed_origins": []string{"https://*.example.com"}}}))
	require.Equal(t, "https://app.example.com", preflight("https://app.example.com"))
	require.Empty(t, preflight("https://evil.test"))
	// The desktop app's webview origin can be allow-listed too.
	e.expect(200, e.do("PATCH", "/api/v1/instance/config", admin, map[string]any{"cors": map[string]any{"allowed_origins": []string{"tauri://localhost"}}}))
	require.Equal(t, "tauri://localhost", preflight("tauri://localhost"))
	require.Empty(t, preflight("https://app.example.com"))
	conn, _, err := dialGateway(e, map[string][]string{"Origin": {"tauri://localhost"}})
	require.NoError(t, err)
	_ = conn.CloseNow()
	e.expect(200, e.do("PATCH", "/api/v1/instance/config", admin, map[string]any{"cors": map[string]any{"allowed_origins": []string{"https://*.example.com"}}}))
	_, status, err := dialGateway(e, map[string][]string{"Origin": {"https://evil.test"}})
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, status)

	checks := e.expect(200, e.do("GET", "/api/v1/instance/checks", admin, nil)).obj(t)["checks"].([]any)
	require.NotEmpty(t, checks)
	e.expect(403, e.do("GET", "/api/v1/instance/checks", alice, nil))
}

func TestSettingsPropagateToOtherReplicas(t *testing.T) {
	dbURL := freshDatabase(t)
	a := newEnv(t, func(o *envOpts) { o.dbURL = dbURL })
	b := newEnv(t, func(o *envOpts) { o.dbURL = dbURL })
	admin := a.setup()
	a.expect(200, a.do("PATCH", "/api/v1/instance/config", admin, map[string]any{"cors": map[string]any{"allowed_origins": []string{"https://only.example"}}}))
	require.Equal(t, []string{"*"}, b.svc.Providers().CORS.AllowedOrigins, "replica b has not polled yet")
	require.NoError(t, b.svc.ReloadProviders(context.Background()))
	require.Equal(t, []string{"https://only.example"}, b.svc.Providers().CORS.AllowedOrigins)
	require.Equal(t, a.svc.Providers().Revision, b.svc.Providers().Revision)
}

func TestPasswordResetByEmail(t *testing.T) {
	box := t.Name()
	e := newEnv(t, withMail(box))
	e.setup()
	alice, _ := e.register("alice")
	verify := waitForMail(t, box, 1)
	require.Contains(t, verify.Subject, "Confirm your email")

	// Unknown addresses look the same as known ones.
	e.expect(202, e.do("POST", "/api/v1/auth/password-reset", "", map[string]any{"email": "nobody@example.com"}))
	e.expect(202, e.do("POST", "/api/v1/auth/password-reset", "", map[string]any{"email": "ALICE@example.com"}))
	m := waitForMail(t, box, 2)
	require.Equal(t, "alice@example.com", m.To)
	require.Contains(t, m.Subject, "Reset your Test Instance password")
	token := linkToken(t, m, e.srv.URL+"/reset-password")

	// A second request within the cooldown sends nothing more.
	e.expect(202, e.do("POST", "/api/v1/auth/password-reset", "", map[string]any{"email": "alice@example.com"}))

	e.expect(422, e.do("POST", "/api/v1/auth/password-reset/confirm", "", map[string]any{"token": "wrong", "new_password": "brand-new-password"}))
	e.expect(422, e.do("POST", "/api/v1/auth/password-reset/confirm", "", map[string]any{"token": token, "new_password": "short"}))
	e.expect(204, e.do("POST", "/api/v1/auth/password-reset/confirm", "", map[string]any{"token": token, "new_password": "brand-new-password"}))
	e.expect(401, e.do("GET", "/api/v1/users/@me", alice, nil))
	e.expect(422, e.do("POST", "/api/v1/auth/password-reset/confirm", "", map[string]any{"token": token, "new_password": "another-password-1"}))
	e.expect(401, e.do("POST", "/api/v1/auth/login", "", map[string]any{"login": "alice", "password": "alice-password-1"}))
	fresh := e.login("alice", "brand-new-password")
	me := e.expect(200, e.do("GET", "/api/v1/users/@me", fresh, nil)).obj(t)
	require.Equal(t, true, me["email_verified"], "resetting by email proves the address")
	time.Sleep(100 * time.Millisecond)
	require.Len(t, inbox(box), 2)

	page := e.expect(200, e.do("GET", "/reset-password", "", nil))
	require.Contains(t, string(page.Raw), "Choose a new password")
}

func TestEmailVerification(t *testing.T) {
	box := t.Name()
	e := newEnv(t, withMail(box))
	e.setup()
	bob, _ := e.register("bob")
	m := waitForMail(t, box, 1)
	require.Equal(t, "bob@example.com", m.To)
	token := linkToken(t, m, e.srv.URL+"/verify-email")

	e.expect(409, e.do("POST", "/api/v1/users/@me/email/verification", bob, nil))
	u := e.expect(200, e.do("POST", "/api/v1/auth/verify-email", "", map[string]any{"token": token})).obj(t)
	require.Equal(t, "bob", u["username"])
	me := e.expect(200, e.do("GET", "/api/v1/users/@me", bob, nil)).obj(t)
	require.Equal(t, true, me["email_verified"])
	e.expect(422, e.do("POST", "/api/v1/auth/verify-email", "", map[string]any{"token": token}))
	e.expect(409, e.do("POST", "/api/v1/users/@me/email/verification", bob, nil))
	require.Contains(t, string(e.expect(200, e.do("GET", "/verify-email", "", nil)).Raw), "Confirm your email")

	// Personal access tokens cannot request verification emails.
	carol, _ := e.register("carol")
	waitForMail(t, box, 2)
	e.expect(403, e.do("POST", "/api/v1/users/@me/email/verification", e.pat(carol, "read", "write"), nil))
}

func TestFailedEmailIsRetriedThenDropped(t *testing.T) {
	box := t.Name()
	e := newEnv(t, withMail(box))
	e.setup()
	e.expect(201, e.do("POST", "/api/v1/auth/register", "", map[string]any{
		"username": "bouncer", "email": "bounce@example.com", "password": "bounce-password-1"}))
	pool := e.svc.Pool()
	require.Eventually(t, func() bool {
		var attempts int
		var lastErr string
		err := pool.QueryRow(context.Background(), `SELECT attempts, last_error FROM mail_outbox WHERE to_address = 'bounce@example.com'`).Scan(&attempts, &lastErr)
		return err == nil && attempts == 1 && strings.Contains(lastErr, "mailbox unavailable")
	}, 10*time.Second, 50*time.Millisecond)
	// Expired messages are dropped and their links scrubbed.
	_, err := pool.Exec(context.Background(), `UPDATE mail_outbox SET expires_at = now() - interval '1 second'`)
	require.NoError(t, err)
	require.NoError(t, e.svc.Maintain(context.Background()))
	var status, text string
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT status, text_body FROM mail_outbox`).Scan(&status, &text))
	require.Equal(t, "failed", status)
	require.Empty(t, text)
}

func TestEmailUnavailableWithoutMail(t *testing.T) {
	e := newEnv(t)
	e.setup()
	alice, _ := e.register("alice")
	e.expect(503, e.do("POST", "/api/v1/auth/password-reset", "", map[string]any{"email": "alice@example.com"}))
	e.expect(503, e.do("POST", "/api/v1/users/@me/email/verification", alice, nil))
	info := e.expect(200, e.do("GET", "/api/v1/instance", "", nil)).obj(t)
	require.Equal(t, false, info["features"].(map[string]any)["email"])
	require.Equal(t, "degraded", info["status"])
	require.Equal(t, []any{"email"}, info["degraded_features"])
	health := e.expect(200, e.do("GET", "/healthz", "", nil)).obj(t)
	require.Equal(t, "degraded", health["state"])
}

func TestUploads(t *testing.T) {
	e := newEnv(t)
	admin := e.setup()
	alice, _ := e.register("alice")
	bob, _ := e.register("bob")
	img := pngBytes(t, 32, 32)

	me := e.expect(200, e.raw("PUT", "/api/v1/users/@me/avatar", alice, "image/png", img)).obj(t)
	avatar := me["avatar_url"].(string)
	require.True(t, strings.HasPrefix(avatar, e.srv.URL+"/media/avatars/"), avatar)
	path := strings.TrimPrefix(avatar, e.srv.URL)
	got := e.expect(200, e.do("GET", path, "", nil))
	require.Equal(t, "image/png", got.Header.Get("Content-Type"))
	require.Contains(t, got.Header.Get("Cache-Control"), "immutable")
	require.Equal(t, "default-src 'none'; sandbox", got.Header.Get("Content-Security-Policy"))
	decoded, err := png.Decode(bytes.NewReader(got.Raw))
	require.NoError(t, err)
	require.Equal(t, 32, decoded.Bounds().Dx())
	e.expect(304, e.get(path, map[string]string{"If-None-Match": got.Header.Get("ETag")}))

	// Not images, oversized files, and unknown files.
	e.expect(422, e.raw("PUT", "/api/v1/users/@me/avatar", alice, "image/png", []byte("<svg onload=alert(1)>")))
	big := make([]byte, e.cfg.Uploads.MaxSize+1)
	copy(big, img)
	require.Equal(t, http.StatusRequestEntityTooLarge, e.raw("PUT", "/api/v1/users/@me/avatar", alice, "image/png", big).Status)
	e.expect(404, e.do("GET", "/media/avatars/missing.png", "", nil))
	e.expect(404, e.do("GET", "/media/.gotalk-probe/x", "", nil))
	e.expect(401, e.raw("PUT", "/api/v1/users/@me/avatar", "", "image/png", img))

	// Replacing an avatar deletes the old file; removing it clears the URL.
	me = e.expect(200, e.raw("PUT", "/api/v1/users/@me/avatar", alice, "image/png", pngBytes(t, 8, 8))).obj(t)
	require.NotEqual(t, avatar, me["avatar_url"])
	e.expect(404, e.do("GET", path, "", nil))
	me = e.expect(200, e.do("DELETE", "/api/v1/users/@me/avatar", alice, nil)).obj(t)
	require.Nil(t, me["avatar_url"])

	// Place images need MANAGE_PLACE.
	e.createPlace(alice, "gallery", "public")
	e.expect(403, e.raw("PUT", "/api/v1/places/gallery/icon", bob, "image/png", img))
	place := e.expect(200, e.raw("PUT", "/api/v1/places/gallery/banner", alice, "image/png", img)).obj(t)
	require.Contains(t, place["banner_url"], "/media/place-banners/")
	e.expect(200, e.do("GET", strings.TrimPrefix(place["banner_url"].(string), e.srv.URL), "", nil))
	place = e.expect(200, e.raw("PUT", "/api/v1/places/gallery/icon", alice, "image/png", img)).obj(t)
	require.Contains(t, place["icon_url"], "/media/place-icons/")
	place = e.expect(200, e.do("DELETE", "/api/v1/places/gallery/icon", alice, nil)).obj(t)
	require.Nil(t, place["icon_url"])

	// The instance icon is for administrators.
	e.expect(403, e.raw("PUT", "/api/v1/instance/icon", alice, "image/png", img))
	info := e.expect(200, e.raw("PUT", "/api/v1/instance/icon", admin, "image/png", img)).obj(t)
	require.Contains(t, info["icon_url"], "/media/instance/")
	limits := info["limits"].(map[string]any)
	require.EqualValues(t, e.cfg.Uploads.MaxSize, limits["upload_size"])
	require.Contains(t, limits["upload_types"], "image/webp")

	// With a public URL (CDN or public bucket), uploads link there instead.
	settings := e.cfg.Storage
	settings.PublicURL = "https://cdn.example.com/files/"
	e.expect(200, e.do("PATCH", "/api/v1/instance/config", admin, map[string]any{"storage": settings}))
	me = e.expect(200, e.raw("PUT", "/api/v1/users/@me/avatar", bob, "image/png", img)).obj(t)
	require.True(t, strings.HasPrefix(me["avatar_url"].(string), "https://cdn.example.com/files/avatars/"), me["avatar_url"])
}

func TestUnreferencedUploadsArePruned(t *testing.T) {
	e := newEnv(t)
	e.setup()
	alice, aliceID := e.register("alice")
	me := e.expect(200, e.raw("PUT", "/api/v1/users/@me/avatar", alice, "image/png", pngBytes(t, 4, 4))).obj(t)
	path := strings.TrimPrefix(me["avatar_url"].(string), e.srv.URL)

	// Deleting the account clears the avatar; the sweep removes the file once old enough.
	e.expect(204, e.do("DELETE", "/api/v1/users/@me", alice, map[string]any{"password": "alice-password-1"}))
	require.NoError(t, e.svc.Maintain(context.Background()))
	e.expect(200, e.do("GET", path, "", nil))
	_, err := e.svc.Pool().Exec(context.Background(), `UPDATE uploads SET created_at = now() - interval '2 hours' WHERE uploader_id = $1`, aliceID)
	require.NoError(t, err)
	require.NoError(t, e.svc.Maintain(context.Background()))
	e.expect(404, e.do("GET", path, "", nil))
	b, err := storage.Open(e.cfg.Storage)
	require.NoError(t, err)
	n := 0
	require.NoError(t, b.List(context.Background(), "avatars/", func(storage.Object) error { n++; return nil }))
	require.Zero(t, n, "the avatar file was deleted from storage")
}

// newEnvPool connects to a database without migrating it, like `gotalk restore` does.
func newEnvPool(t *testing.T, dbURL string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), dbURL)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

func TestReapplySetupRecoversAdmin(t *testing.T) {
	e := newEnv(t)
	admin := e.setup()
	alice, _ := e.register("alice")
	ctx := context.Background()

	name, mode := "Renamed", "closed"
	res, err := e.svc.ReapplySetup(ctx, service.ReapplyInput{InstanceName: &name, RegistrationMode: &mode,
		AdminUsername: "owner", AdminPassword: "recovered-password-1"})
	require.NoError(t, err)
	require.True(t, res.AdminReset)
	require.Equal(t, "Renamed", res.Settings.Name)
	e.expect(401, e.do("GET", "/api/v1/users/@me", admin, nil))
	e.login("owner", "recovered-password-1")

	res, err = e.svc.ReapplySetup(ctx, service.ReapplyInput{AdminUsername: "alice", AdminPassword: "alice-admin-pass-1"})
	require.NoError(t, err)
	require.True(t, res.AdminReset)
	e.expect(401, e.do("GET", "/api/v1/users/@me", alice, nil))
	me := e.expect(200, e.do("GET", "/api/v1/users/@me", e.login("alice", "alice-admin-pass-1"), nil)).obj(t)
	require.Equal(t, true, me["is_instance_admin"])

	res, err = e.svc.ReapplySetup(ctx, service.ReapplyInput{AdminUsername: "rescue", AdminEmail: "rescue@example.com", AdminPassword: "rescue-password-1"})
	require.NoError(t, err)
	require.True(t, res.AdminCreated)
	info := e.expect(200, e.do("GET", "/api/v1/instance", "", nil)).obj(t)
	require.Equal(t, "Renamed", info["name"])
	require.Equal(t, "closed", info["registration_mode"])
}

func TestBackupAndRestore(t *testing.T) {
	e := newEnv(t)
	e.setup()
	alice, _ := e.register("alice")
	me := e.expect(200, e.raw("PUT", "/api/v1/users/@me/avatar", alice, "image/png", pngBytes(t, 6, 6))).obj(t)
	e.createPlace(alice, "archive", "public")
	board := e.board(alice, "archive", map[string]any{"name": "General", "slug": "general"})
	topicID, _ := e.topic(alice, board, "Keep this", "Content that must survive a restore")
	e.reply(alice, topicID, "A reply too", "")
	ctx := context.Background()

	src, err := storage.Open(e.cfg.Storage)
	require.NoError(t, err)
	var archive bytes.Buffer
	m, counts, err := backup.Write(ctx, e.svc.Pool(), &archive, backup.Options{
		Version: "test", Storage: src, MediaPrefixes: service.MediaPrefixes(),
	})
	require.NoError(t, err)
	require.EqualValues(t, 8, m.SchemaVersion)
	require.EqualValues(t, 1, counts.MediaFiles)
	require.EqualValues(t, 2, counts.Rows["users"])
	require.EqualValues(t, 2, counts.Rows["posts"])
	require.NotContains(t, counts.Rows, "goose_db_version")

	// Restore into a fresh database and a different storage directory.
	dbURL := freshDatabase(t)
	dest := t.TempDir()
	target := newEnvPool(t, dbURL)
	destStore, err := storage.Open(storage.Settings{Driver: "local", LocalPath: dest})
	require.NoError(t, err)
	_, restored, err := backup.Restore(ctx, target, bytes.NewReader(archive.Bytes()), backup.RestoreOptions{
		MediaTarget: func(context.Context) (storage.Backend, error) { return destStore, nil },
	})
	require.NoError(t, err)
	require.Equal(t, counts.Rows, restored.Rows)
	require.EqualValues(t, 1, restored.MediaFiles)

	r := newEnv(t, func(o *envOpts) {
		o.dbURL = dbURL
		o.mutate = func(c *config.Config) { c.Storage = storage.Settings{Driver: "local", LocalPath: dest} }
	})
	login := r.login("alice", "alice-password-1")
	topic := r.expect(200, r.do("GET", "/api/v1/topics/"+topicID, login, nil)).obj(t)
	require.Equal(t, "Keep this", topic["title"])
	avatar := strings.TrimPrefix(me["avatar_url"].(string), e.srv.URL)
	r.expect(200, r.do("GET", avatar, "", nil))
	required, err := r.svc.SetupRequired(ctx)
	require.NoError(t, err)
	require.False(t, required)

	// A configured database is not overwritten without force.
	_, _, err = backup.Restore(ctx, target, bytes.NewReader(archive.Bytes()), backup.RestoreOptions{})
	require.ErrorIs(t, err, backup.ErrNotEmpty)
	_, _, err = backup.Restore(ctx, target, bytes.NewReader(archive.Bytes()), backup.RestoreOptions{Force: true})
	require.NoError(t, err)

	// Garbage and truncated archives are refused before anything is written, so the data
	// from the last good restore stays. (Sessions created after the backup was taken end
	// with a restore, hence the new login.)
	_, _, err = backup.Restore(ctx, target, strings.NewReader("not a backup"), backup.RestoreOptions{Force: true})
	require.ErrorContains(t, err, "not a gotalk backup")
	_, _, err = backup.Restore(ctx, target, bytes.NewReader(archive.Bytes()[:archive.Len()/3]), backup.RestoreOptions{Force: true})
	require.Error(t, err)
	r.expect(401, r.do("GET", "/api/v1/topics/"+topicID, login, nil))
	r.expect(200, r.do("GET", "/api/v1/topics/"+topicID, r.login("alice", "alice-password-1"), nil))
}
