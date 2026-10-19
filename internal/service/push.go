package service

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/parkerbrown98/gotalk-server/internal/apperr"
	"github.com/parkerbrown98/gotalk-server/internal/config"
	"github.com/parkerbrown98/gotalk-server/internal/pushgw"
	"github.com/parkerbrown98/gotalk-server/internal/realtime"
	"github.com/parkerbrown98/gotalk-server/internal/store"
)

// SectionPush controls push notifications through a Gotalk push gateway (docs/push.md).
const SectionPush = "push"

// Registration states of the instance with its push gateway.
const (
	PushUnregistered = "unregistered"
	PushPending      = "pending"
	PushActive       = "active"
	PushSuspended    = "suspended"
	PushBlocked      = "blocked"
	PushError        = "error"
)

// Gateway sync timing. Tests shorten them.
var (
	// PushGatewayRecheck is how often one replica re-reads the registration status while
	// it is active.
	PushGatewayRecheck = 6 * time.Hour
	// PushGatewayRetry is how often registration is retried while it is not active.
	PushGatewayRetry   = 5 * time.Minute
	pushGatewayTimeout = 30 * time.Second
	// pushDiscoveryTTL is how long the gateway's discovery document (its apps and VAPID
	// keys) is served from memory.
	pushDiscoveryTTL = time.Hour
)

// PushSettings turn on push notifications through a gateway.
type PushSettings struct {
	Enabled      bool   `json:"enabled" doc:"Register with the push gateway and send notifications to phones and browsers"`
	GatewayURL   string `json:"gateway_url,omitempty" doc:"Push gateway base URL (default https://push.gotalk.sh, the official gateway the official apps use)"`
	ContactEmail string `json:"contact_email,omitempty" doc:"Given to the gateway for abuse and key notices; defaults to the oldest administrator's address"`
	SigningKey   string `json:"signing_key,omitempty" doc:"Ed25519 seed (32 bytes, unpadded base64url) for signing gateway requests; write-only, leave empty to keep the current value. Without one, the instance generates and stores a key"`
}

func (p PushSettings) Redacted() PushSettings { p.SigningKey = ""; return p }

func (p PushSettings) SecretsSet() []string {
	if p.SigningKey != "" {
		return []string{"signing_key"}
	}
	return []string{}
}

func (p PushSettings) KeepSecrets(prev PushSettings) PushSettings {
	if p.SigningKey == "" {
		p.SigningKey = prev.SigningKey
	}
	return p
}

func pushSettingsOf(c config.Push) PushSettings {
	return PushSettings{Enabled: c.Enabled, GatewayURL: c.GatewayURL, ContactEmail: c.ContactEmail, SigningKey: c.SigningKey}
}

func (p PushSettings) config() config.Push {
	return config.Push{Enabled: p.Enabled, GatewayURL: p.GatewayURL, ContactEmail: p.ContactEmail, SigningKey: p.SigningKey}
}

// gatewayURL is the effective gateway base URL, without a trailing slash.
func (p PushSettings) gatewayURL() string {
	if p.GatewayURL == "" {
		return config.DefaultPushGatewayURL
	}
	return strings.TrimRight(p.GatewayURL, "/")
}

// NormalizeGatewayURL trims a trailing slash so URLs compare equal.
func NormalizeGatewayURL(u string) string { return strings.TrimRight(u, "/") }

// presenceSource holds the API layer's presence store.
type presenceSource struct{ p realtime.Presence }

// SetPresence lets push delivery hold back pushes for people in do-not-disturb.
func (s *Service) SetPresence(p realtime.Presence) {
	if p == nil {
		s.presence.Store(nil)
		return
	}
	s.presence.Store(&presenceSource{p: p})
}

// pushKeys is the gateway signing key for one settings revision.
type pushKeys struct {
	revision  int64
	signing   ed25519.PrivateKey
	generated bool
}

// pushSigningKey returns the configured key, or the generated one (creating it on first
// use).
func (s *Service) pushSigningKey(ctx context.Context) (*pushKeys, error) {
	p := s.Providers()
	if k := s.pushKeys.Load(); k != nil && k.revision == p.Revision {
		return k, nil
	}
	k := &pushKeys{revision: p.Revision}
	if seed := p.Push.SigningKey; seed != "" {
		var err error
		if k.signing, err = pushgw.KeyFromSeed(seed); err != nil {
			return nil, err
		}
	} else {
		row, err := s.q.GetPushGateway(ctx)
		if err != nil {
			return nil, err
		}
		stored := row.SigningKey
		if len(stored) != ed25519.SeedSize {
			seed := make([]byte, ed25519.SeedSize)
			if _, err := rand.Read(seed); err != nil {
				return nil, err
			}
			if stored, err = s.q.EnsurePushSigningKey(ctx, seed); err != nil {
				return nil, err
			}
		}
		k.signing, k.generated = ed25519.NewKeyFromSeed(stored), true
	}
	s.pushKeys.Store(k)
	return k, nil
}

// PushEnabled reports whether the operator turned push notifications on.
func (s *Service) PushEnabled() bool { return s.Providers().Push.Enabled }

// PushGatewayURL is the gateway this instance pushes through.
func (s *Service) PushGatewayURL() string { return s.Providers().Push.gatewayURL() }

// PushPublicKeys returns the keys to publish in the well-known document's push.keys: the
// signing key while push is on, none otherwise.
func (s *Service) PushPublicKeys(ctx context.Context) ([]PublicKey, error) {
	if !s.PushEnabled() {
		return nil, nil
	}
	k, err := s.pushSigningKey(ctx)
	if err != nil {
		return nil, err
	}
	return []PublicKey{{Alg: "Ed25519", PublicKey: pushgw.PublicKey(k.signing)}}, nil
}

// pushClient returns a client for the configured gateway.
func (s *Service) pushClient() *pushgw.Client {
	return pushgw.New(s.PushGatewayURL(), pushgw.Options{UserAgent: "gotalk-server"})
}

// pushCredentials returns the signing credentials of the current registration, or false
// when the instance is not registered with the configured gateway under its current key.
func (s *Service) pushCredentials(ctx context.Context, row store.PushGateway) (pushgw.Credentials, bool, error) {
	k, err := s.pushSigningKey(ctx)
	if err != nil {
		return pushgw.Credentials{}, false, err
	}
	creds := pushgw.Credentials{Key: k.signing}
	if row.InstanceID == nil || row.KeyID == nil || deref(row.GatewayUrl) != s.PushGatewayURL() ||
		deref(row.PublicKey) != pushgw.PublicKey(k.signing) {
		return creds, false, nil
	}
	creds.KeyID = *row.KeyID
	return creds, true, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// wakePushGateway asks the registration loop to sync now.
func (s *Service) wakePushGateway() {
	select {
	case s.pushGatewayWake <- struct{}{}:
	default:
	}
}

// RunPushGateway keeps the instance registered and verified with its push gateway: right
// away when push is turned on or its settings change, then every PushGatewayRecheck (or
// PushGatewayRetry while not active). One replica per interval does the periodic check.
func (s *Service) RunPushGateway(ctx context.Context) {
	t := time.NewTimer(0)
	defer t.Stop()
	for {
		force := false
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.pushGatewayWake:
			force = true
		}
		next := PushGatewayRecheck
		if s.PushEnabled() {
			if required, err := s.SetupRequired(ctx); err == nil && required {
				// Registration waits for setup, which wakes this loop when it completes.
				next = PushGatewayRetry
			} else if st, err := s.syncPushGateway(ctx, force); err != nil && ctx.Err() == nil {
				s.log.Warn("syncing the push gateway registration", "error", err)
				next = PushGatewayRetry
			} else if st != PushActive {
				next = PushGatewayRetry
			}
		}
		if !t.Stop() {
			select {
			case <-t.C:
			default:
			}
		}
		t.Reset(next)
	}
}

// syncPushGateway registers, verifies or re-checks the instance. Unless force is set, it
// only runs when no replica did so within the interval. It returns the resulting status.
func (s *Service) syncPushGateway(ctx context.Context, force bool) (string, error) {
	s.pushSyncMu.Lock()
	defer s.pushSyncMu.Unlock()
	row, err := s.q.GetPushGateway(ctx)
	if err != nil {
		return "", err
	}
	if !force {
		interval := PushGatewayRecheck
		if row.Status != PushActive {
			interval = PushGatewayRetry
		}
		// Leave a margin so replicas with slightly different timers still take turns.
		if _, err := s.q.ClaimPushGatewayCheck(ctx, (interval - interval/10).Seconds()); errors.Is(err, pgx.ErrNoRows) {
			return row.Status, nil
		} else if err != nil {
			return row.Status, err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, pushGatewayTimeout)
	defer cancel()
	client := s.pushClient()
	s.refreshPushDiscovery(ctx, client)

	origin := s.cfg.Server.PublicURL
	if origin == "" {
		return s.setPushStatus(ctx, nil, PushError,
			"server.public_url is not set; the gateway identifies the instance by its public https:// address")
	}
	creds, registered, err := s.pushCredentials(ctx, row)
	if err != nil {
		return row.Status, err
	}
	if registered {
		inst, err := client.GetInstance(ctx, creds, *row.InstanceID)
		if err == nil {
			if inst.Status == pushgw.StatusActive && inst.KeyStatus(creds.KeyID) == "active" {
				return s.setPushStatus(ctx, nil, PushActive, "")
			}
			if inst.Status == pushgw.StatusBlocked {
				return s.setPushStatus(ctx, nil, PushBlocked, blockReason(inst.StatusReason))
			}
			return s.verifyPushGateway(ctx, client, creds, *row.InstanceID)
		}
		e, ok := pushgw.AsError(err)
		switch {
		case ok && (e.Code == pushgw.CodeUnknownKey || e.Code == pushgw.CodeInstanceNotFound):
			// The registration expired or the gateway deleted it: register again (recovery
			// keeps the instance and its subscriptions when the origin is still verified).
			s.log.Info("the push gateway no longer knows this instance's key; registering again", "code", e.Code)
		case ok && e.Code == pushgw.CodeInstanceNotVerified:
			// A pending (rotation or recovery) key may only call verify.
			return s.verifyPushGateway(ctx, client, creds, *row.InstanceID)
		case ok && e.Code == pushgw.CodeInstanceBlocked:
			return s.setPushStatus(ctx, nil, PushBlocked, e.Detail)
		case temporary(err):
			// A blip (timeout, 5xx, throttling) says nothing about the registration: keep the
			// status, so delivery goes on, and try again on the next round.
			return row.Status, fmt.Errorf("checking the push gateway registration: %w", err)
		default:
			return s.setPushStatus(ctx, nil, PushError, "checking the registration failed: "+err.Error())
		}
	}

	contact, err := s.pushContactEmail(ctx)
	if err != nil {
		return s.setPushStatus(ctx, nil, PushError, err.Error())
	}
	pub := pushgw.PublicKey(creds.Key)
	reg, err := client.Register(ctx, pushgw.RegisterInput{
		Origin: origin, PublicKey: pub, ContactEmail: contact, Software: "gotalk", SoftwareVersion: s.version(),
	})
	if err != nil {
		if e, ok := pushgw.AsError(err); ok && e.Code == pushgw.CodeInstanceBlocked {
			return s.setPushStatus(ctx, nil, PushBlocked, e.Detail)
		}
		return s.setPushStatus(ctx, nil, PushError, "registering with the gateway failed: "+err.Error())
	}
	gw := s.PushGatewayURL()
	if err := s.q.SavePushRegistration(ctx, store.SavePushRegistrationParams{
		GatewayUrl: &gw, PublicKey: &pub, InstanceID: &reg.InstanceID, KeyID: &reg.KeyID, Status: PushPending, Detail: "",
	}); err != nil {
		return row.Status, err
	}
	s.log.Info("registered with the push gateway", "gateway", gw, "instance_id", reg.InstanceID, "key_id", reg.KeyID)
	creds.KeyID = reg.KeyID
	return s.verifyPushGateway(ctx, client, creds, reg.InstanceID)
}

// verifyPushGateway asks the gateway to fetch the well-known document, which lists the key.
func (s *Service) verifyPushGateway(ctx context.Context, client *pushgw.Client, creds pushgw.Credentials, instanceID string) (string, error) {
	inst, err := client.Verify(ctx, creds, instanceID)
	if err != nil {
		e, ok := pushgw.AsError(err)
		switch {
		case ok && e.Code == pushgw.CodeInstanceBlocked:
			return s.setPushStatus(ctx, nil, PushBlocked, e.Detail)
		case ok && e.Code == pushgw.CodeInstanceNotVerified:
			return s.setPushStatus(ctx, nil, PushSuspended, "the gateway reports the instance not verified: "+e.Detail)
		case ok && e.Code == pushgw.CodeVerificationFailed:
			return s.setPushStatus(ctx, nil, PushPending, "the gateway could not verify the instance: "+e.Detail+
				" (it fetches "+s.cfg.Server.PublicURL+"/.well-known/gotalk-instance, which must list the push key)")
		case temporary(err):
			row, rerr := s.q.GetPushGateway(ctx)
			if rerr != nil {
				return "", rerr
			}
			return row.Status, fmt.Errorf("asking the push gateway to verify the instance: %w", err)
		default:
			return s.setPushStatus(ctx, nil, PushError, "verification failed: "+err.Error())
		}
	}
	switch inst.Status {
	case pushgw.StatusActive:
		return s.setPushStatus(ctx, nil, PushActive, "")
	case pushgw.StatusBlocked:
		return s.setPushStatus(ctx, nil, PushBlocked, blockReason(inst.StatusReason))
	case pushgw.StatusSuspended:
		return s.setPushStatus(ctx, nil, PushSuspended, blockReason(inst.StatusReason))
	default:
		return s.setPushStatus(ctx, nil, PushPending, "waiting for the gateway to verify the instance")
	}
}

func blockReason(r *string) string {
	if r == nil {
		return ""
	}
	return *r
}

// temporary reports whether a gateway call may succeed as is later: network failures,
// timeouts, 5xx and throttling.
func temporary(err error) bool {
	if e, ok := pushgw.AsError(err); ok {
		return e.Temporary()
	}
	return err != nil
}

// setPushStatus records the registration status and, when it changed, logs it and adds an
// entry to the instance audit log so administrators see gateway problems.
func (s *Service) setPushStatus(ctx context.Context, actor *Principal, status, detail string) (string, error) {
	detail = truncate(detail, 1000)
	err := s.tx(ctx, func(q *store.Queries) error {
		prev, err := q.GetPushGateway(ctx)
		if err != nil {
			return err
		}
		if err := q.SetPushGatewayStatus(ctx, store.SetPushGatewayStatusParams{Status: status, Detail: detail}); err != nil {
			return err
		}
		if prev.Status == status {
			return nil
		}
		return s.instanceAudit(ctx, q, actor, "push.status", "", nil, map[string]any{
			"from": prev.Status, "to": status, "detail": detail, "gateway_url": s.PushGatewayURL(),
		})
	})
	if err != nil {
		return status, err
	}
	switch status {
	case PushActive:
		s.wakePush()
	case PushError, PushBlocked, PushSuspended:
		s.log.Warn("push notifications are not working", "status", status, "detail", detail)
	}
	return status, nil
}

// pushContactEmail is the configured contact, or the oldest administrator's address.
func (s *Service) pushContactEmail(ctx context.Context) (string, error) {
	if c := s.Providers().Push.ContactEmail; c != "" {
		return c, nil
	}
	admin, err := s.q.GetOldestInstanceAdmin(ctx)
	if err == nil && admin.Email != "" {
		return admin.Email, nil
	}
	if c := s.Providers().Directory.ContactEmail; c != "" {
		return c, nil
	}
	return "", errors.New("no contact email for the push gateway: set push.contact_email")
}

// version is the server version reported to the gateway.
func (s *Service) version() string {
	if v := s.appVersion.Load(); v != nil {
		return *v
	}
	return "dev"
}

// SetVersion records the server version, which registration reports to the push gateway.
func (s *Service) SetVersion(v string) { s.appVersion.Store(&v) }

// pushDiscovery is the gateway's cached discovery document.
type pushDiscovery struct {
	gatewayURL string
	doc        *pushgw.Discovery
	at         time.Time
}

func (s *Service) refreshPushDiscovery(ctx context.Context, client *pushgw.Client) {
	d, err := client.Discover(ctx)
	if err != nil {
		s.log.Warn("fetching the push gateway's discovery document", "gateway", client.BaseURL(), "error", err)
		return
	}
	s.pushDiscovery.Store(&pushDiscovery{gatewayURL: client.BaseURL(), doc: d, at: time.Now()})
}

// PushWebApp is an app the gateway serves to browsers, with the key browsers subscribe with.
type PushWebApp struct {
	AppID          string
	VAPIDPublicKey string
}

// PushWebApps returns the gateway's web apps from the cached discovery document. A stale
// cache is served while a refresh runs in the background.
func (s *Service) PushWebApps() []PushWebApp {
	d := s.pushDiscovery.Load()
	if d == nil || d.gatewayURL != s.PushGatewayURL() || time.Since(d.at) > pushDiscoveryTTL {
		if s.PushEnabled() && s.pushDiscoveryBusy.CompareAndSwap(false, true) {
			go func() {
				defer s.pushDiscoveryBusy.Store(false)
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				s.refreshPushDiscovery(ctx, s.pushClient())
			}()
		}
		if d == nil || d.gatewayURL != s.PushGatewayURL() {
			return []PushWebApp{}
		}
	}
	out := []PushWebApp{}
	for _, a := range d.doc.Apps {
		if a.Platforms.Web != nil {
			out = append(out, PushWebApp{AppID: a.ID, VAPIDPublicKey: a.Platforms.Web.VAPIDPublicKey})
		}
	}
	return out
}

// PushAvailable reports whether clients can register for push: it is on and the gateway
// has verified the instance.
func (s *Service) PushAvailable(ctx context.Context) bool {
	if !s.PushEnabled() {
		return false
	}
	row, err := s.q.GetPushGateway(ctx)
	return err == nil && row.Status == PushActive && deref(row.GatewayUrl) == s.PushGatewayURL()
}

// PushStatus is the administrators' view of push notifications.
type PushStatus struct {
	Enabled       bool
	Source        string
	Editable      bool
	GatewayURL    string
	Origin        string
	Status        string
	Detail        string
	InstanceID    *string
	KeyID         *string
	KeySource     string
	PublicKey     string
	VerifiedAt    *time.Time
	CheckedAt     *time.Time
	Subscriptions int64
	WebApps       []PushWebApp
	GatewayName   string
}

// PushStatus returns the gateway registration for administrators.
func (s *Service) PushStatus(ctx context.Context, p *Principal) (PushStatus, error) {
	if !p.User.IsInstanceAdmin {
		return PushStatus{}, apperr.Forbidden("only instance administrators can read instance settings")
	}
	prov := s.Providers()
	row, err := s.q.GetPushGateway(ctx)
	if err != nil {
		return PushStatus{}, err
	}
	st := PushStatus{
		Enabled: prov.Push.Enabled, Source: prov.Sources[SectionPush], GatewayURL: prov.Push.gatewayURL(),
		Origin: s.cfg.Server.PublicURL, Status: row.Status, Detail: row.Detail, KeySource: "none",
		VerifiedAt: row.VerifiedAt, CheckedAt: row.CheckedAt, WebApps: s.PushWebApps(),
	}
	st.Editable = st.Source != SourceConfig
	if deref(row.GatewayUrl) == st.GatewayURL {
		st.InstanceID, st.KeyID = row.InstanceID, row.KeyID
	} else if row.InstanceID != nil {
		st.Status, st.Detail = PushUnregistered, "registered with a different gateway; the instance registers with "+st.GatewayURL+" next"
	}
	if prov.Push.Enabled {
		k, err := s.pushSigningKey(ctx)
		if err != nil {
			return st, err
		}
		st.PublicKey, st.KeySource = pushgw.PublicKey(k.signing), "configured"
		if k.generated {
			st.KeySource = "generated"
		}
	}
	if d := s.pushDiscovery.Load(); d != nil && d.gatewayURL == st.GatewayURL {
		st.GatewayName = d.doc.Name
	}
	st.Subscriptions, err = s.q.CountAllPushSubscriptions(ctx)
	return st, err
}

// VerifyPushGateway registers or re-verifies the instance now (administrators).
func (s *Service) VerifyPushGateway(ctx context.Context, p *Principal) error {
	if !p.User.IsInstanceAdmin {
		return apperr.Forbidden("only instance administrators can change instance settings")
	}
	if !s.PushEnabled() {
		return apperr.Conflict("push notifications are off; turn them on first")
	}
	if err := s.instanceAudit(ctx, s.q, p, "push.verify", "", nil, map[string]any{"gateway_url": s.PushGatewayURL()}); err != nil {
		return err
	}
	if _, err := s.syncPushGateway(ctx, true); err != nil {
		if temporary(err) {
			return apperr.Unavailable("the push gateway did not answer; try again shortly (%s)", err.Error())
		}
		return err
	}
	return nil
}

// RotatePushKey replaces the generated signing key. The instance then registers the new key
// with the gateway, which (once the well-known document lists it) revokes the old one.
func (s *Service) RotatePushKey(ctx context.Context, p *Principal) error {
	if !p.User.IsInstanceAdmin {
		return apperr.Forbidden("only instance administrators can change instance settings")
	}
	if s.Providers().Push.SigningKey != "" {
		return apperr.Conflict("the signing key is set in push.signing_key; replace it there to rotate it")
	}
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return err
	}
	err := s.tx(ctx, func(q *store.Queries) error {
		if err := q.ReplacePushSigningKey(ctx, seed); err != nil {
			return err
		}
		if _, err := q.BumpConfigRevision(ctx); err != nil {
			return err
		}
		return s.instanceAudit(ctx, q, p, "push.key_rotate", "", nil,
			map[string]any{"public_key": pushgw.PublicKey(ed25519.NewKeyFromSeed(seed))})
	})
	if err != nil {
		return err
	}
	if err := s.ReloadProviders(ctx); err != nil {
		return err
	}
	s.wakePushGateway()
	return nil
}

// checkPushSettings validates proposed push settings: the instance has a public https
// address and the gateway answers its discovery document.
func (s *Service) checkPushSettings(ctx context.Context, ps PushSettings) Check {
	c := Check{Name: "push"}
	if !ps.Enabled {
		c.Status, c.Detail = CheckOK, "push notifications are off"
		return c
	}
	if s.cfg.Server.PublicURL == "" {
		c.Status, c.Detail = CheckError, "server.public_url is not set"
		c.Hint = "Set server.public_url (GOTALK_SERVER_PUBLIC_URL) to the instance's public https:// address; the gateway identifies the instance by it."
		return c
	}
	if u, err := url.Parse(s.cfg.Server.PublicURL); err == nil && u.Scheme != "https" && !isLocalHost(u.Hostname()) {
		c.Status, c.Detail = CheckError, "push gateways only accept instances served over HTTPS"
		c.Hint = "Serve the instance over HTTPS and set server.public_url to its https:// address."
		return c
	}
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	d, err := pushgw.New(ps.gatewayURL(), pushgw.Options{UserAgent: "gotalk-server"}).Discover(ctx)
	if err != nil {
		c.Status, c.Detail = CheckError, "the push gateway is not reachable: "+err.Error()
		c.Hint = "Check gateway_url and that this server can reach it over the network."
		return c
	}
	apps := make([]string, len(d.Apps))
	for i, a := range d.Apps {
		apps[i] = a.ID
	}
	c.Status = CheckOK
	c.Detail = fmt.Sprintf("%s (%s %s) delivers for %s", ps.gatewayURL(), cmpOr(d.Name, "gateway"), d.Version, strings.Join(apps, ", "))
	if len(apps) == 0 {
		c.Status, c.Detail = CheckWarning, ps.gatewayURL()+" serves no apps, so no client can register"
	}
	return c
}

// pushCheck reports the registration for the pre-flight checks without contacting the
// gateway.
func (s *Service) pushCheck(ctx context.Context) Check {
	c := Check{Name: "push"}
	if !s.PushEnabled() {
		c.Status, c.Detail = CheckSkipped, "push notifications are off; turn them on in the settings to notify phones and browsers"
		return c
	}
	row, err := s.q.GetPushGateway(ctx)
	if err != nil {
		c.Status, c.Detail = CheckError, err.Error()
		return c
	}
	gw := s.PushGatewayURL()
	if required, _ := s.SetupRequired(ctx); required {
		c.Status, c.Detail = CheckWarning, "the instance registers with "+gw+" once setup is complete"
		return c
	}
	switch {
	case row.Status == PushActive && deref(row.GatewayUrl) == gw:
		c.Status, c.Detail = CheckOK, "verified by "+gw+" as "+deref(row.InstanceID)
	case row.Status == PushPending || row.Status == PushUnregistered || deref(row.GatewayUrl) != gw:
		c.Status, c.Detail = CheckWarning, "registering with "+gw+cmpSuffix(row.Detail)
		c.Hint = "The gateway must reach " + cmpOr(s.cfg.Server.PublicURL, "server.public_url") + "/.well-known/gotalk-instance over HTTPS."
	default:
		c.Status, c.Detail = CheckError, gw+" reports the instance "+row.Status+cmpSuffix(row.Detail)
		c.Hint = "See the push section of the instance settings; the instance audit log records each change."
	}
	return c
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func cmpSuffix(detail string) string {
	if detail == "" {
		return ""
	}
	return ": " + detail
}

var gatewaySubscriptionID = regexp.MustCompile(`^sub_[A-Za-z0-9_-]{8,64}$`)

// validGatewaySubscriptionID checks the shape of gateway subscription IDs (sub_…).
func validGatewaySubscriptionID(id string) bool { return gatewaySubscriptionID.MatchString(id) }

// gatewayDeleteDevice deletes a gateway subscription nothing on this instance uses any more.
// Failures are logged: the gateway expires unrefreshed subscriptions on its own.
func (s *Service) gatewayDeleteDevice(ctx context.Context, gatewayURL, subscriptionID string) {
	if NormalizeGatewayURL(gatewayURL) != s.PushGatewayURL() {
		return
	}
	n, err := s.q.CountPushSubscriptionRefs(ctx, store.CountPushSubscriptionRefsParams{GatewayUrl: gatewayURL, SubscriptionID: subscriptionID})
	if err != nil || n > 0 {
		return
	}
	row, err := s.q.GetPushGateway(ctx)
	if err != nil {
		return
	}
	creds, ok, err := s.pushCredentials(ctx, row)
	if err != nil || !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := s.pushClient().DeleteDevice(ctx, creds, subscriptionID); err != nil {
		s.log.Warn("deleting a subscription at the push gateway", "subscription_id", subscriptionID, "error", err)
	}
}
