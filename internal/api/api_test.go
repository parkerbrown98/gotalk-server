package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"

	"github.com/parkerbrown98/gotalk-server/internal/api"
	"github.com/parkerbrown98/gotalk-server/internal/config"
	"github.com/parkerbrown98/gotalk-server/internal/database"
	"github.com/parkerbrown98/gotalk-server/internal/ratelimit"
	"github.com/parkerbrown98/gotalk-server/internal/service"
)

var (
	pgOnce      sync.Once
	pgContainer *tcpostgres.PostgresContainer
	pgAdminURL  string
	pgErr       error
	dbCounter   atomic.Int64

	redisOnce      sync.Once
	redisContainer *tcredis.RedisContainer
	redisURL       string
	redisErr       error
)

func TestMain(m *testing.M) {
	code := m.Run()
	if pgContainer != nil {
		_ = testcontainers.TerminateContainer(pgContainer)
	}
	if redisContainer != nil {
		_ = testcontainers.TerminateContainer(redisContainer)
	}
	os.Exit(code)
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// freshDatabase creates an isolated database on the shared Postgres container.
func freshDatabase(t *testing.T) string {
	t.Helper()
	testcontainers.SkipIfProviderIsNotHealthy(t)
	pgOnce.Do(func() {
		ctx := context.Background()
		pgContainer, pgErr = tcpostgres.Run(ctx, "postgres:17-alpine",
			tcpostgres.WithDatabase("gotalk"),
			tcpostgres.WithUsername("gotalk"),
			tcpostgres.WithPassword("gotalk"),
			tcpostgres.BasicWaitStrategies(),
		)
		if pgErr == nil {
			pgAdminURL, pgErr = pgContainer.ConnectionString(ctx, "sslmode=disable")
		}
	})
	require.NoError(t, pgErr)

	ctx := context.Background()
	name := fmt.Sprintf("t_%d_%d", time.Now().UnixNano(), dbCounter.Add(1))
	conn, err := pgx.Connect(ctx, pgAdminURL)
	require.NoError(t, err)
	_, err = conn.Exec(ctx, "CREATE DATABASE "+name)
	require.NoError(t, err)
	require.NoError(t, conn.Close(ctx))

	u, err := url.Parse(pgAdminURL)
	require.NoError(t, err)
	u.Path = "/" + name
	return u.String()
}

func sharedRedis(t *testing.T) string {
	t.Helper()
	testcontainers.SkipIfProviderIsNotHealthy(t)
	redisOnce.Do(func() {
		ctx := context.Background()
		redisContainer, redisErr = tcredis.Run(ctx, "redis:7-alpine")
		if redisErr == nil {
			redisURL, redisErr = redisContainer.ConnectionString(ctx)
		}
	})
	require.NoError(t, redisErr)
	return redisURL
}

type env struct {
	t   *testing.T
	srv *httptest.Server
	svc *service.Service
	cfg *config.Config
}

type envOpts struct {
	dbURL     string
	redisURL  string
	heartbeat time.Duration
	mutate    func(*config.Config)
}

func newEnv(t *testing.T, opts ...func(*envOpts)) *env {
	t.Helper()
	o := envOpts{}
	for _, f := range opts {
		f(&o)
	}
	if o.dbURL == "" {
		o.dbURL = freshDatabase(t)
	}

	cfg, err := config.Load("")
	require.NoError(t, err)
	cfg.Database.URL = o.dbURL
	cfg.Redis.URL = o.redisURL
	cfg.Auth.JWTSecret = ""
	cfg.Setup = config.Setup{InstanceName: "Gotalk", RegistrationMode: "open"}
	// Tests make many auth calls from one IP; TestAuthRateLimit lowers this explicitly.
	cfg.RateLimit.Auth = "1000-M"
	cfg.RateLimit.Content = "1000-M"
	cfg.RateLimit.Chat = "1000-M"
	if o.mutate != nil {
		o.mutate(cfg)
	}

	ctx := context.Background()
	log := quietLogger()
	pool, err := database.Connect(ctx, cfg.Database.URL, 10, 30*time.Second, log)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	require.NoError(t, database.Migrate(ctx, pool, log))

	var rdb *redis.Client
	if cfg.Redis.URL != "" {
		ropts, err := redis.ParseURL(cfg.Redis.URL)
		require.NoError(t, err)
		rdb = redis.NewClient(ropts)
		t.Cleanup(func() { _ = rdb.Close() })
	}
	limiter, err := ratelimit.New(rdb, cfg.RateLimit.Tiers())
	require.NoError(t, err)

	svc, err := service.New(ctx, pool, cfg, log)
	require.NoError(t, err)

	handler, err := api.New(api.Deps{
		Service: svc, Limiter: limiter, Redis: rdb, Config: cfg, Logger: log, Version: "test",
		HeartbeatInterval: o.heartbeat,
	})
	require.NoError(t, err)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	t.Cleanup(handler.Close)
	return &env{t: t, srv: srv, svc: svc, cfg: cfg}
}

type resp struct {
	Status int
	Header http.Header
	Raw    []byte
}

func (r resp) obj(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal(r.Raw, &m), string(r.Raw))
	return m
}

func (r resp) list(t *testing.T) []any {
	t.Helper()
	var l []any
	require.NoError(t, json.Unmarshal(r.Raw, &l), string(r.Raw))
	return l
}

var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func (e *env) do(method, path, token string, body any) resp {
	e.t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(e.t, err)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, e.srv.URL+path, rdr)
	require.NoError(e.t, err)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := noRedirect.Do(req)
	require.NoError(e.t, err)
	defer func() { _ = res.Body.Close() }()
	raw, err := io.ReadAll(res.Body)
	require.NoError(e.t, err)
	return resp{Status: res.StatusCode, Header: res.Header, Raw: raw}
}

func (e *env) expect(status int, r resp) resp {
	e.t.Helper()
	require.Equal(e.t, status, r.Status, "body: %s", r.Raw)
	return r
}

// setup completes first-run setup headlessly and returns the admin's access token.
func (e *env) setup() string {
	e.t.Helper()
	_, err := e.svc.CompleteSetupHeadless(context.Background(), service.SetupInput{
		InstanceName: "Test Instance", RegistrationMode: "open",
		AdminUsername: "owner", AdminEmail: "owner@example.com", AdminPassword: "owner-password-1",
	})
	require.NoError(e.t, err)
	return e.login("owner", "owner-password-1")
}

func (e *env) login(login, password string) string {
	e.t.Helper()
	r := e.expect(200, e.do("POST", "/api/v1/auth/login", "", map[string]any{"login": login, "password": password}))
	return r.obj(e.t)["access_token"].(string)
}

func (e *env) register(username string) (token, id string) {
	e.t.Helper()
	r := e.expect(201, e.do("POST", "/api/v1/auth/register", "", map[string]any{
		"username": username, "email": username + "@example.com", "password": username + "-password-1",
	}))
	m := r.obj(e.t)
	return m["access_token"].(string), m["user"].(map[string]any)["id"].(string)
}

func (e *env) createPlace(token, slug, visibility string) map[string]any {
	e.t.Helper()
	return e.expect(201, e.do("POST", "/api/v1/places", token, map[string]any{
		"slug": slug, "name": "Place " + slug, "visibility": visibility,
	})).obj(e.t)
}

func TestSetupWizardFlow(t *testing.T) {
	e := newEnv(t)

	e.expect(503, e.do("GET", "/api/v1/users/@me", "", nil))
	r := e.expect(303, e.do("GET", "/", "", nil))
	require.Equal(t, "/setup", r.Header.Get("Location"))
	require.Equal(t, "awaiting_setup", e.expect(200, e.do("GET", "/readyz", "", nil)).obj(t)["status"])
	require.Equal(t, true, e.expect(200, e.do("GET", "/api/v1/instance", "", nil)).obj(t)["setup_required"])
	require.Contains(t, string(e.expect(200, e.do("GET", "/setup", "", nil)).Raw), "Welcome to Gotalk")

	status := e.expect(200, e.do("GET", "/api/v1/setup/status", "", nil)).obj(t)
	require.Equal(t, true, status["setup_required"])
	checks := status["checks"].([]any)
	require.Equal(t, "database", checks[0].(map[string]any)["name"])
	require.Equal(t, "ok", checks[0].(map[string]any)["status"])

	body := func(token string) map[string]any {
		return map[string]any{
			"setup_token": token,
			"instance":    map[string]any{"name": "My Forum", "registration_mode": "invite_only"},
			"admin":       map[string]any{"username": "admin1", "email": "admin@example.com", "password": "admin-password-1"},
		}
	}
	e.expect(403, e.do("POST", "/api/v1/setup", "", body("wrong-token")))

	token, err := e.svc.SetupToken(context.Background())
	require.NoError(t, err)
	require.Len(t, token, 32)
	res := e.expect(201, e.do("POST", "/api/v1/setup", "", body(token))).obj(t)
	require.Equal(t, "My Forum", res["instance"].(map[string]any)["name"])
	require.Equal(t, false, res["instance"].(map[string]any)["setup_required"])
	access := res["tokens"].(map[string]any)["access_token"].(string)

	e.expect(409, e.do("POST", "/api/v1/setup", "", body(token)))
	me := e.expect(200, e.do("GET", "/api/v1/users/@me", access, nil)).obj(t)
	require.Equal(t, true, me["is_instance_admin"])

	r = e.expect(303, e.do("GET", "/setup", "", nil))
	require.Equal(t, "/", r.Header.Get("Location"))
	require.Equal(t, "ready", e.expect(200, e.do("GET", "/readyz", "", nil)).obj(t)["status"])

	info := e.expect(200, e.do("GET", "/api/v1/instance", "", nil)).obj(t)
	require.Equal(t, "invite_only", info["registration_mode"])
	require.Equal(t, e.srv.URL+"/api/v1", info["api"].(map[string]any)["base_url"])

	wk := e.expect(200, e.do("GET", "/.well-known/gotalk-instance", "", nil)).obj(t)
	require.Equal(t, e.srv.URL+"/api/v1", wk["api_base_url"])
}

func TestHeadlessSetupIsRaceSafe(t *testing.T) {
	e := newEnv(t)
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = e.svc.CompleteSetupHeadless(context.Background(), service.SetupInput{
				InstanceName: "Race", AdminUsername: fmt.Sprintf("admin%d", i),
				AdminEmail: fmt.Sprintf("admin%d@example.com", i), AdminPassword: "admin-password-1",
			})
		}()
	}
	wg.Wait()
	succeeded := 0
	for _, err := range errs {
		if err == nil {
			succeeded++
		} else {
			require.ErrorIs(t, err, service.ErrSetupAlreadyCompleted)
		}
	}
	require.Equal(t, 1, succeeded)
	stats, err := e.svc.InstanceStats(context.Background())
	require.NoError(t, err)
	require.EqualValues(t, 1, stats.Users)
}

func TestConcurrentMigrationsAreSerialized(t *testing.T) {
	dbURL := freshDatabase(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make([]error, 3)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pool, err := pgxpool.New(ctx, dbURL)
			if err != nil {
				errs[i] = err
				return
			}
			defer pool.Close()
			errs[i] = database.Migrate(ctx, pool, quietLogger())
		}()
	}
	wg.Wait()
	for _, err := range errs {
		require.NoError(t, err)
	}
}

func TestAuthLifecycle(t *testing.T) {
	e := newEnv(t)
	e.setup()

	aliceToken, _ := e.register("alice")
	e.expect(409, e.do("POST", "/api/v1/auth/register", "", map[string]any{
		"username": "ALICE", "email": "other@example.com", "password": "alice-password-1"}))
	e.expect(409, e.do("POST", "/api/v1/auth/register", "", map[string]any{
		"username": "alice2", "email": "Alice@Example.com", "password": "alice-password-1"}))
	e.expect(422, e.do("POST", "/api/v1/auth/register", "", map[string]any{
		"username": "admin", "email": "x@example.com", "password": "long-enough-password"}))
	e.expect(422, e.do("POST", "/api/v1/auth/register", "", map[string]any{
		"username": "shortpw", "email": "s@example.com", "password": "short"}))

	e.expect(401, e.do("POST", "/api/v1/auth/login", "", map[string]any{"login": "alice", "password": "nope-nope-nope"}))
	e.expect(401, e.do("POST", "/api/v1/auth/login", "", map[string]any{"login": "nobody", "password": "nope-nope-nope"}))
	login := e.expect(200, e.do("POST", "/api/v1/auth/login", "", map[string]any{
		"login": "ALICE@example.com", "password": "alice-password-1"})).obj(t)
	refresh1 := login["refresh_token"].(string)

	// Rotation: the old refresh token stops working and reusing it revokes the session.
	rotated := e.expect(200, e.do("POST", "/api/v1/auth/refresh", "", map[string]any{"refresh_token": refresh1})).obj(t)
	access2 := rotated["access_token"].(string)
	e.expect(200, e.do("GET", "/api/v1/users/@me", access2, nil))
	e.expect(401, e.do("POST", "/api/v1/auth/refresh", "", map[string]any{"refresh_token": refresh1}))
	e.expect(401, e.do("GET", "/api/v1/users/@me", access2, nil))
	e.expect(401, e.do("POST", "/api/v1/auth/refresh", "", map[string]any{"refresh_token": rotated["refresh_token"]}))

	sessions := e.expect(200, e.do("GET", "/api/v1/users/@me/sessions", aliceToken, nil)).list(t)
	require.Len(t, sessions, 1, "registration session only; the rotated session was revoked")
	require.Equal(t, true, sessions[0].(map[string]any)["current"])

	// Profile updates, including URL scheme validation.
	me := e.expect(200, e.do("PATCH", "/api/v1/users/@me", aliceToken, map[string]any{
		"display_name": "  Alice A.  ", "pronouns": "she/her", "avatar_url": "https://cdn.example.com/a.png"})).obj(t)
	require.Equal(t, "Alice A.", me["display_name"])
	e.expect(422, e.do("PATCH", "/api/v1/users/@me", aliceToken, map[string]any{"avatar_url": "javascript:alert(1)"}))
	pub := e.expect(200, e.do("GET", "/api/v1/users/alice", "", nil)).obj(t)
	require.NotContains(t, pub, "email", "public profiles never expose email")

	// Password change keeps the current session and signs out the others.
	other := e.login("alice", "alice-password-1")
	e.expect(403, e.do("POST", "/api/v1/users/@me/password", aliceToken, map[string]any{
		"current_password": "wrong-password", "new_password": "alice-password-2"}))
	e.expect(204, e.do("POST", "/api/v1/users/@me/password", aliceToken, map[string]any{
		"current_password": "alice-password-1", "new_password": "alice-password-2"}))
	e.expect(200, e.do("GET", "/api/v1/users/@me", aliceToken, nil))
	e.expect(401, e.do("GET", "/api/v1/users/@me", other, nil))
	e.login("alice", "alice-password-2")

	// Malformed and missing credentials.
	e.expect(401, e.do("GET", "/api/v1/users/@me", "", nil))
	e.expect(401, e.do("GET", "/api/v1/users/@me", "not-a-jwt", nil))

	e.expect(204, e.do("POST", "/api/v1/auth/logout", aliceToken, nil))
	e.expect(401, e.do("GET", "/api/v1/users/@me", aliceToken, nil))
}

func TestAccountDeletion(t *testing.T) {
	e := newEnv(t)
	owner := e.setup()
	e.expect(409, e.do("DELETE", "/api/v1/users/@me", owner, map[string]any{"password": "owner-password-1"}))

	bob, _ := e.register("bob")
	place := e.createPlace(owner, "lounge", "public")
	e.expect(200, e.do("POST", "/api/v1/places/lounge/join", bob, nil))
	e.expect(403, e.do("DELETE", "/api/v1/users/@me", bob, map[string]any{"password": "wrong"}))
	e.expect(204, e.do("DELETE", "/api/v1/users/@me", bob, map[string]any{"password": "bob-password-1"}))

	e.expect(401, e.do("GET", "/api/v1/users/@me", bob, nil))
	e.expect(404, e.do("GET", "/api/v1/users/bob", "", nil))
	got := e.expect(200, e.do("GET", "/api/v1/places/"+place["id"].(string), "", nil)).obj(t)
	require.EqualValues(t, 1, got["member_count"])
	e.expect(409, e.do("POST", "/api/v1/auth/register", "", map[string]any{
		"username": "bob", "email": "bob@example.com", "password": "bob-password-1"}), // username stays reserved
	)
	e.register("bobby") // but the email was released
}

func TestPlacesRolesAndModeration(t *testing.T) {
	e := newEnv(t)
	owner := e.setup()
	bob, bobID := e.register("bob")
	carol, carolID := e.register("carol")

	place := e.createPlace(owner, "gophers", "public")
	require.EqualValues(t, 1, place["member_count"])
	require.NotNil(t, place["my_permissions"])
	e.expect(409, e.do("POST", "/api/v1/places", owner, map[string]any{"slug": "gophers", "name": "dup"}))
	e.expect(422, e.do("POST", "/api/v1/places", owner, map[string]any{"slug": "setup", "name": "reserved"}))

	anon := e.expect(200, e.do("GET", "/api/v1/places/gophers", "", nil)).obj(t)
	require.NotContains(t, anon, "my_permissions")

	joined := e.expect(200, e.do("POST", "/api/v1/places/gophers/join", bob, nil)).obj(t)
	require.EqualValues(t, 2, joined["member_count"])
	e.expect(200, e.do("POST", "/api/v1/places/gophers/join", bob, nil)) // idempotent
	e.expect(200, e.do("POST", "/api/v1/places/gophers/join", carol, nil))
	e.expect(403, e.do("PATCH", "/api/v1/places/gophers", bob, map[string]any{"name": "hijacked"}))

	// Owner creates a moderator role and assigns it to bob.
	mod := e.expect(201, e.do("POST", "/api/v1/places/gophers/roles", owner, map[string]any{
		"name": "Moderator", "permissions": 4 | 32 | 64})).obj(t) // MANAGE_ROLES | KICK | BAN
	require.EqualValues(t, 1, mod["position"])
	modID := mod["id"].(string)
	member := e.expect(200, e.do("PUT", "/api/v1/places/gophers/members/"+bobID+"/roles/"+modID, owner, nil)).obj(t)
	require.Equal(t, []any{modID}, member["role_ids"])

	perms := e.expect(200, e.do("GET", "/api/v1/places/gophers/permissions/@me", bob, nil)).obj(t)
	require.Contains(t, perms["permission_names"], "KICK_MEMBERS")
	require.EqualValues(t, 1, perms["top_position"])

	// Hierarchy: bob cannot grant what he lacks, edit his own top role, or act on the owner.
	e.expect(403, e.do("POST", "/api/v1/places/gophers/roles", bob, map[string]any{"name": "Evil", "permissions": 1}))
	helper := e.expect(201, e.do("POST", "/api/v1/places/gophers/roles", bob, map[string]any{"name": "Helper", "permissions": 32})).obj(t)
	require.EqualValues(t, 1, helper["position"])
	roles := e.expect(200, e.do("GET", "/api/v1/places/gophers/roles", bob, nil)).list(t)
	require.Equal(t, "Moderator", roles[0].(map[string]any)["name"], "highest first")
	require.EqualValues(t, 2, roles[0].(map[string]any)["position"], "existing roles shift up")
	e.expect(403, e.do("PATCH", "/api/v1/places/gophers/roles/"+modID, bob, map[string]any{"name": "Mod+"}))
	e.expect(403, e.do("DELETE", "/api/v1/places/gophers/members/"+e.userID(owner), bob, nil))

	// Reordering: owner moves Helper above Moderator; now bob cannot touch Helper.
	moved := e.expect(200, e.do("PATCH", "/api/v1/places/gophers/roles/"+helper["id"].(string), owner, map[string]any{"position": 2})).obj(t)
	require.EqualValues(t, 2, moved["position"])
	e.expect(403, e.do("DELETE", "/api/v1/places/gophers/roles/"+helper["id"].(string), bob, nil))
	e.expect(204, e.do("DELETE", "/api/v1/places/gophers/roles/"+helper["id"].(string), owner, nil))
	roles = e.expect(200, e.do("GET", "/api/v1/places/gophers/roles", owner, nil)).list(t)
	require.Len(t, roles, 2)
	require.EqualValues(t, 1, roles[0].(map[string]any)["position"], "positions close the gap")

	// Nicknames.
	nick := e.expect(200, e.do("PATCH", "/api/v1/places/gophers/members/"+carolID, carol, map[string]any{"nickname": "Caz"})).obj(t)
	require.Equal(t, "Caz", nick["nickname"])
	e.expect(403, e.do("PATCH", "/api/v1/places/gophers/members/"+bobID, carol, map[string]any{"nickname": "x"}))

	// Kick, ban, unban.
	e.expect(204, e.do("DELETE", "/api/v1/places/gophers/members/"+carolID, bob, nil))
	e.expect(404, e.do("GET", "/api/v1/places/gophers/members/"+carolID, owner, nil))
	e.expect(200, e.do("POST", "/api/v1/places/gophers/join", carol, nil))
	e.expect(204, e.do("PUT", "/api/v1/places/gophers/bans/"+carolID, bob, map[string]any{"reason": "spam"}))
	e.expect(403, e.do("POST", "/api/v1/places/gophers/join", carol, nil))
	bans := e.expect(200, e.do("GET", "/api/v1/places/gophers/bans", owner, nil)).obj(t)["items"].([]any)
	require.Equal(t, "spam", bans[0].(map[string]any)["reason"])
	e.expect(403, e.do("PUT", "/api/v1/places/gophers/bans/"+e.userID(owner), bob, nil))
	e.expect(204, e.do("DELETE", "/api/v1/places/gophers/bans/"+carolID, owner, nil))
	e.expect(200, e.do("POST", "/api/v1/places/gophers/join", carol, nil))

	members := e.expect(200, e.do("GET", "/api/v1/places/gophers/members?limit=2", owner, nil)).obj(t)
	require.Len(t, members["items"], 2)
	require.EqualValues(t, 2, members["next_offset"])

	// Leaving and ownership.
	e.expect(409, e.do("POST", "/api/v1/places/gophers/leave", owner, nil))
	e.expect(403, e.do("POST", "/api/v1/places/gophers/transfer", bob, map[string]any{"user_id": bobID}))
	transferred := e.expect(200, e.do("POST", "/api/v1/places/gophers/transfer", owner, map[string]any{"user_id": bobID})).obj(t)
	require.Equal(t, bobID, transferred["owner_id"])
	e.expect(204, e.do("POST", "/api/v1/places/gophers/leave", owner, nil))
	e.expect(403, e.do("GET", "/api/v1/places/gophers/members", owner, nil))

	myPlaces := e.expect(200, e.do("GET", "/api/v1/users/@me/places", bob, nil)).list(t)
	require.Len(t, myPlaces, 1)

	e.expect(204, e.do("DELETE", "/api/v1/places/gophers", bob, nil))
	e.expect(404, e.do("GET", "/api/v1/places/gophers", bob, nil))
}

func (e *env) userID(token string) string {
	e.t.Helper()
	return e.expect(200, e.do("GET", "/api/v1/users/@me", token, nil)).obj(e.t)["id"].(string)
}

func TestVisibilityAndDiscovery(t *testing.T) {
	e := newEnv(t)
	owner := e.setup()
	dave, _ := e.register("dave")

	e.createPlace(owner, "open-club", "public")
	e.createPlace(owner, "by-invite", "invite_only")
	e.createPlace(owner, "secret", "private")

	found := e.expect(200, e.do("GET", "/api/v1/places", "", nil)).obj(t)["items"].([]any)
	slugs := []string{}
	for _, p := range found {
		slugs = append(slugs, p.(map[string]any)["slug"].(string))
	}
	require.ElementsMatch(t, []string{"open-club", "by-invite"}, slugs)

	search := e.expect(200, e.do("GET", "/api/v1/places?q=OPEN", "", nil)).obj(t)["items"].([]any)
	require.Len(t, search, 1)
	wildcard := e.expect(200, e.do("GET", "/api/v1/places?q=%25", "", nil)).obj(t)["items"].([]any)
	require.Empty(t, wildcard, "LIKE wildcards are matched literally")

	e.expect(404, e.do("GET", "/api/v1/places/secret", dave, nil))
	e.expect(404, e.do("GET", "/api/v1/places/secret", "", nil))
	e.expect(200, e.do("GET", "/api/v1/places/secret", owner, nil))
	e.expect(403, e.do("POST", "/api/v1/places/by-invite/join", dave, nil))
}

func TestInvitesAndRegistrationModes(t *testing.T) {
	e := newEnv(t)
	owner := e.setup()
	bob, _ := e.register("bob")
	carol, _ := e.register("carol")

	e.createPlace(owner, "inner-circle", "private")
	invite := e.expect(201, e.do("POST", "/api/v1/places/inner-circle/invites", owner, map[string]any{"max_uses": 1})).obj(t)
	code := invite["code"].(string)
	require.Len(t, code, 10)
	require.NotNil(t, invite["expires_at"], "defaults to a 7 day expiry")

	preview := e.expect(200, e.do("GET", "/api/v1/invites/"+code, "", nil)).obj(t)
	require.Equal(t, "inner-circle", preview["place"].(map[string]any)["slug"])

	joined := e.expect(200, e.do("POST", "/api/v1/invites/"+code, bob, nil)).obj(t)
	require.EqualValues(t, 2, joined["member_count"])
	e.expect(200, e.do("POST", "/api/v1/invites/"+code, bob, nil)) // already a member: no use consumed
	e.expect(404, e.do("POST", "/api/v1/invites/"+code, carol, nil))
	e.expect(404, e.do("GET", "/api/v1/invites/"+code, "", nil))

	// Default members can create invites but not list or revoke others' invites.
	bobInvite := e.expect(201, e.do("POST", "/api/v1/places/inner-circle/invites", bob, map[string]any{"max_age": 0})).obj(t)
	require.Nil(t, bobInvite["expires_at"])
	e.expect(403, e.do("GET", "/api/v1/places/inner-circle/invites", bob, nil))
	require.Len(t, e.expect(200, e.do("GET", "/api/v1/places/inner-circle/invites", owner, nil)).list(t), 2)
	e.expect(403, e.do("DELETE", "/api/v1/places/inner-circle/invites/"+code, bob, nil))
	e.expect(204, e.do("DELETE", "/api/v1/places/inner-circle/invites/"+bobInvite["code"].(string), bob, nil))

	// Non-admins cannot change instance settings.
	e.expect(403, e.do("PATCH", "/api/v1/instance", bob, map[string]any{"registration_mode": "closed"}))

	// invite_only registration requires a valid invite and joins its place.
	e.expect(200, e.do("PATCH", "/api/v1/instance", owner, map[string]any{"registration_mode": "invite_only"}))
	e.expect(403, e.do("POST", "/api/v1/auth/register", "", map[string]any{
		"username": "erin", "email": "erin@example.com", "password": "erin-password-1"}))
	fresh := e.expect(201, e.do("POST", "/api/v1/places/inner-circle/invites", owner, map[string]any{})).obj(t)["code"].(string)
	reg := e.expect(201, e.do("POST", "/api/v1/auth/register", "", map[string]any{
		"username": "erin", "email": "erin@example.com", "password": "erin-password-1", "invite_code": fresh})).obj(t)
	places := e.expect(200, e.do("GET", "/api/v1/users/@me/places", reg["access_token"].(string), nil)).list(t)
	require.Equal(t, "inner-circle", places[0].(map[string]any)["slug"])
	e.expect(404, e.do("POST", "/api/v1/auth/register", "", map[string]any{
		"username": "frank", "email": "frank@example.com", "password": "frank-password-1", "invite_code": "bogus"}))
	e.expect(404, e.do("GET", "/api/v1/users/frank", "", nil)) // the failed registration rolled back

	e.expect(200, e.do("PATCH", "/api/v1/instance", owner, map[string]any{"registration_mode": "closed"}))
	e.expect(403, e.do("POST", "/api/v1/auth/register", "", map[string]any{
		"username": "gina", "email": "gina@example.com", "password": "gina-password-1", "invite_code": fresh}))
}

func TestAuthRateLimit(t *testing.T) {
	e := newEnv(t, func(o *envOpts) {
		o.mutate = func(c *config.Config) { c.RateLimit.Auth = "3-M" }
	})
	e.setup()
	for range 2 { // e.setup() already logged in once from this IP
		r := e.expect(401, e.do("POST", "/api/v1/auth/login", "", map[string]any{"login": "owner", "password": "wrong-password"}))
		require.NotEmpty(t, r.Header.Get("X-RateLimit-Remaining"))
	}
	r := e.expect(429, e.do("POST", "/api/v1/auth/login", "", map[string]any{"login": "owner", "password": "owner-password-1"}))
	require.NotEmpty(t, r.Header.Get("Retry-After"))
	require.Equal(t, "0", r.Header.Get("X-RateLimit-Remaining"))
	require.Equal(t, "application/problem+json", r.Header.Get("Content-Type"))

	// Other tiers are unaffected.
	e.expect(200, e.do("GET", "/api/v1/instance", "", nil))
}

func TestRedisRateLimitIsSharedAcrossReplicas(t *testing.T) {
	rURL := sharedRedis(t)
	dbURL := freshDatabase(t)
	limit := func(o *envOpts) {
		o.dbURL, o.redisURL = dbURL, rURL
		o.mutate = func(c *config.Config) { c.RateLimit.Auth = "2-M" }
	}
	// Two replicas sharing one database and one Redis.
	a, b := newEnv(t, limit), newEnv(t, limit)
	require.NoError(t, redis.NewClient(mustParseRedis(t, rURL)).FlushAll(context.Background()).Err())

	body := map[string]any{"login": "nobody", "password": "whatever-pass"}
	a.expect(503, a.do("POST", "/api/v1/auth/login", "", body)) // gated: setup pending
	a.setup()
	b.expect(401, b.do("POST", "/api/v1/auth/login", "", body))
	b.expect(429, b.do("POST", "/api/v1/auth/login", "", body))
	a.expect(429, a.do("POST", "/api/v1/auth/login", "", body))

	ready := a.expect(200, a.do("GET", "/readyz", "", nil)).obj(t)
	require.Equal(t, "ok", ready["checks"].(map[string]any)["redis"])
}

func mustParseRedis(t *testing.T, u string) *redis.Options {
	opts, err := redis.ParseURL(u)
	require.NoError(t, err)
	return opts
}

func TestOpenAPIAndDocs(t *testing.T) {
	e := newEnv(t)
	spec := e.expect(200, e.do("GET", "/api/v1/openapi.json", "", nil)).obj(t)
	paths := spec["paths"].(map[string]any)
	for _, p := range []string{"/auth/login", "/places/{place}", "/places/{place}/members/{userID}/roles/{roleID}", "/setup",
		"/places/{place}/boards", "/boards/{boardID}/topics", "/topics/{topicID}/posts", "/posts/{postID}/reactions/{emoji}",
		"/search", "/users/@me/notifications", "/places/{place}/reports", "/places/{place}/audit-log",
		"/users/@me/tokens", "/applications/{applicationID}/commands", "/channels/{channelID}/interactions",
		"/places/{place}/webhooks", "/webhooks/{webhookID}/deliveries", "/policies/{kind}/versions", "/transparency",
		"/rate-limits"} {
		require.Contains(t, paths, p)
	}
	servers := spec["servers"].([]any)
	require.Equal(t, "/api/v1", servers[0].(map[string]any)["url"])
	docs := e.expect(200, e.do("GET", "/api/v1/docs", "", nil))
	require.True(t, strings.Contains(string(docs.Raw), "openapi"), "docs page references the spec")
	require.Len(t, e.expect(200, e.do("GET", "/api/v1/permissions", "", nil)).list(t), 29)

	e.setup()
	features := e.expect(200, e.do("GET", "/api/v1/instance", "", nil)).obj(t)["features"].(map[string]any)
	require.Equal(t, true, features["forums"])
	require.Equal(t, "postgres", features["search"])
}
