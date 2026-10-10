package service

import (
	"context"
	"sync"
	"time"

	"github.com/parkerbrown98/gotalk-server/internal/database"
	"github.com/parkerbrown98/gotalk-server/internal/livekit"
	"github.com/parkerbrown98/gotalk-server/internal/mail"
	"github.com/parkerbrown98/gotalk-server/internal/storage"
)

// Check statuses.
const (
	CheckOK      = "ok"
	CheckWarning = "warning"
	CheckError   = "error"
	CheckSkipped = "skipped"
)

// Check is the result of one pre-flight or health check. Hint tells the operator what to
// do about a warning or error.
type Check struct {
	Name   string
	Status string
	Detail string
	Hint   string
}

const checkTimeout = 10 * time.Second

func checkStorage(ctx context.Context, b storage.Backend, openErr error) Check {
	c := Check{Name: "storage"}
	switch {
	case openErr != nil:
		c.Status, c.Detail = CheckError, openErr.Error()
		c.Hint = "fix the storage.* settings (GOTALK_STORAGE_*) or choose another storage backend in the setup wizard"
	case b == nil:
		c.Status, c.Detail = CheckError, "no storage backend is configured"
		c.Hint = "set GOTALK_STORAGE_DRIVER to local or s3"
	default:
		ctx, cancel := context.WithTimeout(ctx, checkTimeout)
		defer cancel()
		if err := storage.Check(ctx, b); err != nil {
			c.Status, c.Detail = CheckError, b.Describe()+": "+err.Error()
			if b.Driver() == "local" {
				c.Hint = "make sure the directory exists or can be created and is writable by the server's user (uid 65532 in the container image)"
			} else {
				c.Hint = "check the endpoint, bucket name, region and that the access key may read, write and delete objects"
			}
		} else {
			c.Status, c.Detail = CheckOK, b.Describe()+" is writable"
		}
	}
	return c
}

func checkMail(ctx context.Context, m mail.Sender, openErr error) Check {
	c := Check{Name: "email"}
	switch {
	case openErr != nil:
		c.Status, c.Detail = CheckError, openErr.Error()
		c.Hint = "fix the mail.* settings (GOTALK_MAIL_*) or reconfigure email in the setup wizard"
	case m == nil:
		c.Status, c.Detail = CheckWarning, "email is not configured, so password resets and email verification are unavailable"
		c.Hint = "set GOTALK_MAIL_DRIVER (smtp, sendgrid, mailgun, postmark, resend or ses) or configure email in the setup wizard"
	default:
		ctx, cancel := context.WithTimeout(ctx, checkTimeout)
		defer cancel()
		if err := m.Check(ctx); err != nil {
			c.Status, c.Detail = CheckError, m.Describe()+": "+err.Error()
			c.Hint = "check the host, port, TLS mode and credentials, or the provider API key"
		} else {
			c.Status, c.Detail = CheckOK, m.Describe()+" accepted the connection"
		}
	}
	return c
}

func checkVoice(ctx context.Context, lk *livekit.Client, openErr error) Check {
	c := Check{Name: "voice"}
	switch {
	case openErr != nil:
		c.Status, c.Detail = CheckError, openErr.Error()
		c.Hint = "fix the voice.* settings (GOTALK_VOICE_*) or reconfigure voice in the setup wizard"
	case lk == nil:
		c.Status, c.Detail = CheckSkipped, "not configured; set GOTALK_VOICE_LIVEKIT_URL and API credentials (or use the setup wizard) to enable voice channels"
	default:
		ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		if err := lk.Ping(ctx); err != nil {
			c.Status, c.Detail = CheckError, "LiveKit is unreachable or rejected the API key: "+err.Error()
			c.Hint = "check that LiveKit is running, livekit_api_url is reachable from this server and the API key and secret match LiveKit's keys"
		} else {
			c.Status, c.Detail = CheckOK, "connected to LiveKit at "+lk.URL()
		}
	}
	return c
}

// sendTestEmail sends a test message directly (not through the queue).
func (s *Service) sendTestEmail(ctx context.Context, m mail.Sender, to string) Check {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	name := "Gotalk"
	if settings, err := s.q.GetInstanceSettings(ctx); err == nil && settings.SetupCompletedAt != nil {
		name = settings.Name
	}
	msg := renderTestEmail(name)
	msg.To = to
	if err := m.Send(ctx, msg); err != nil {
		return Check{Name: "email", Status: CheckError, Detail: "sending a test email failed: " + err.Error(),
			Hint: "check the sender address (mail.from) is allowed by your provider"}
	}
	return Check{Name: "email", Status: CheckOK, Detail: "sent a test email to " + to + " via " + m.Describe()}
}

// DatabaseCheck reports whether PostgreSQL is reachable and migrated.
func (s *Service) DatabaseCheck(ctx context.Context) Check {
	st, err := database.Status(ctx, s.pool)
	switch {
	case err != nil:
		return Check{Name: "database", Status: CheckError, Detail: err.Error(), Hint: "check GOTALK_DATABASE_URL and that PostgreSQL is running"}
	case st.Current < st.Latest:
		return Check{Name: "database", Status: CheckError, Detail: "migrations are pending", Hint: "run `gotalk migrate` or enable database.auto_migrate"}
	case st.Current > st.Latest:
		return Check{Name: "database", Status: CheckError, Detail: "the database schema is newer than this version of Gotalk",
			Hint: "upgrade Gotalk to the version that last migrated this database"}
	}
	return Check{Name: "database", Status: CheckOK, Detail: "connected; schema is up to date"}
}

// Preflight runs every check live: database, Redis (when ping is not nil), public URL,
// storage, email and voice. It backs the setup wizard, `gotalk check` and the boot log.
func (s *Service) Preflight(ctx context.Context, redisPing func(context.Context) error) []Check {
	checks := []Check{s.DatabaseCheck(ctx)}
	if redisPing == nil {
		checks = append(checks, Check{Name: "redis", Status: CheckSkipped,
			Detail: "not configured; rate limits, presence and real-time events stay within this process (fine for a single server)",
			Hint:   "set GOTALK_REDIS_URL before running more than one replica"})
	} else if err := redisPing(ctx); err != nil {
		checks = append(checks, Check{Name: "redis", Status: CheckError, Detail: err.Error(), Hint: "check GOTALK_REDIS_URL and that Redis is running"})
	} else {
		checks = append(checks, Check{Name: "redis", Status: CheckOK, Detail: "connected"})
	}
	if s.cfg.Server.PublicURL == "" {
		checks = append(checks, Check{Name: "public_url", Status: CheckWarning,
			Detail: "GOTALK_SERVER_PUBLIC_URL is not set; links in emails and uploaded file URLs will be derived from each request's host",
			Hint:   "set GOTALK_SERVER_PUBLIC_URL to the address people use, e.g. https://forum.example.com"})
	} else {
		checks = append(checks, Check{Name: "public_url", Status: CheckOK, Detail: s.cfg.Server.PublicURL})
	}
	p := s.Providers()
	var wg sync.WaitGroup
	results := make([]Check, 3)
	wg.Go(func() { results[0] = checkStorage(ctx, p.Storage, p.StorageErr) })
	wg.Go(func() { results[1] = checkMail(ctx, p.Mail, p.MailErr) })
	wg.Go(func() { results[2] = checkVoice(ctx, p.Voice, p.VoiceErr) })
	wg.Wait()
	s.health.store(results[0], results[1])
	return append(checks, results...)
}

// healthCache keeps the latest storage and email checks so readiness probes and instance
// metadata do not hit providers on every request.
type healthCache struct {
	mu       sync.Mutex
	checks   map[string]Check
	at       map[string]time.Time
	inFlight map[string]bool
	// refreshes tracks background refreshes so shutdown can wait for them; closed stops
	// new ones.
	refreshes sync.WaitGroup
	closed    bool
}

var healthTTL = map[string]time.Duration{"storage": time.Minute, "email": 5 * time.Minute}

func (h *healthCache) reset() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.checks, h.at = nil, nil
}

func (h *healthCache) store(checks ...Check) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.checks == nil {
		h.checks, h.at = map[string]Check{}, map[string]time.Time{}
	}
	for _, c := range checks {
		h.checks[c.Name], h.at[c.Name] = c, time.Now()
	}
}

// ProviderHealth returns the storage and email checks. Missing results are computed
// right away when wait is set; stale ones are refreshed in the background.
func (s *Service) ProviderHealth(ctx context.Context, wait bool) map[string]Check {
	p := s.Providers()
	run := map[string]func(context.Context) Check{
		"storage": func(ctx context.Context) Check { return checkStorage(ctx, p.Storage, p.StorageErr) },
		"email":   func(ctx context.Context) Check { return checkMail(ctx, p.Mail, p.MailErr) },
	}
	out := map[string]Check{}
	for name, fn := range run {
		if name == "email" && p.Mail == nil {
			// Nothing to contact; the answer is known.
			out[name] = fn(ctx)
			continue
		}
		h := &s.health
		h.mu.Lock()
		c, ok := h.checks[name]
		stale := !ok || time.Since(h.at[name]) > healthTTL[name]
		start := stale && !h.inFlight[name] && !h.closed && (ok || !wait)
		if start {
			if h.inFlight == nil {
				h.inFlight = map[string]bool{}
			}
			h.inFlight[name] = true
			// Registered under the lock, so it cannot race with WaitHealthChecks.
			h.refreshes.Add(1)
		}
		h.mu.Unlock()
		switch {
		case ok:
			out[name] = c
		case wait:
			c = fn(ctx)
			h.store(c)
			out[name] = c
		default:
			out[name] = Check{Name: name, Status: CheckSkipped, Detail: "not checked yet"}
		}
		if start {
			go func() {
				defer h.refreshes.Done()
				ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), checkTimeout)
				defer cancel()
				h.store(fn(ctx))
				h.mu.Lock()
				delete(h.inFlight, name)
				h.mu.Unlock()
			}()
		}
	}
	return out
}

// WaitHealthChecks stops background health refreshes and waits for running ones to
// finish (at most the check timeout); call it when shutting down.
func (s *Service) WaitHealthChecks() {
	s.health.mu.Lock()
	s.health.closed = true
	s.health.mu.Unlock()
	s.health.refreshes.Wait()
}

// InstanceState summarizes health for clients and orchestrators: awaiting_setup,
// healthy or degraded, with the features that are degraded.
func (s *Service) InstanceState(ctx context.Context) (string, []string, error) {
	required, err := s.SetupRequired(ctx)
	if err != nil {
		return "", nil, err
	}
	if required {
		return "awaiting_setup", []string{}, nil
	}
	degraded := []string{}
	health := s.ProviderHealth(ctx, false)
	for _, name := range []string{"storage", "email"} {
		if c := health[name]; c.Status == CheckError || c.Status == CheckWarning {
			degraded = append(degraded, name)
		}
	}
	if p := s.Providers(); p.VoiceErr != nil {
		degraded = append(degraded, "voice")
	}
	if len(degraded) > 0 {
		return "degraded", degraded, nil
	}
	return "healthy", degraded, nil
}
