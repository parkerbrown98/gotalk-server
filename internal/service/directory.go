package service

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/parkerbrown98/gotalk-server/internal/apperr"
	"github.com/parkerbrown98/gotalk-server/internal/config"
	"github.com/parkerbrown98/gotalk-server/internal/indexsig"
	"github.com/parkerbrown98/gotalk-server/internal/store"
)

// SectionDirectory controls listing in the public index (docs/api-index.md).
const SectionDirectory = "directory"

// IndexProtocolVersion is the index protocol version this server speaks.
const IndexProtocolVersion = 1

// DefaultPlatformURL is the public index that pings go to unless configured otherwise.
const DefaultPlatformURL = "https://gotalk.sh"

// DirectorySettings opt the instance in to the public index. Place owners still opt in each
// place, and members can exclude their own posts.
type DirectorySettings struct {
	Listed        bool   `json:"listed" doc:"Advertise the instance in /.well-known/gotalk-instance and serve the index endpoints"`
	ContactEmail  string `json:"contact_email,omitempty" doc:"Shared with the index for abuse reports and crawl problems only"`
	PlatformURL   string `json:"platform_url,omitempty" doc:"Index that receives pings (default https://gotalk.sh)"`
	PingsDisabled bool   `json:"pings_disabled,omitempty" doc:"Stop pinging the index after changes; it then picks them up on its next scheduled read (within minutes)"`
	SigningKey    string `json:"signing_key,omitempty" doc:"Ed25519 seed (32 bytes, unpadded base64url) for signing pings; write-only, leave empty to keep the current value. Without one, the instance generates and stores a key"`
}

func (d DirectorySettings) Redacted() DirectorySettings { d.SigningKey = ""; return d }

func (d DirectorySettings) SecretsSet() []string {
	if d.SigningKey != "" {
		return []string{"signing_key"}
	}
	return []string{}
}

func (d DirectorySettings) KeepSecrets(prev DirectorySettings) DirectorySettings {
	if d.SigningKey == "" {
		d.SigningKey = prev.SigningKey
	}
	return d
}

func directorySettingsOf(c config.Directory) DirectorySettings {
	return DirectorySettings{Listed: c.Listed, ContactEmail: c.ContactEmail, PlatformURL: c.PlatformURL, PingsDisabled: c.PingsDisabled, SigningKey: c.SigningKey}
}

func (d DirectorySettings) config() config.Directory {
	return config.Directory{Listed: d.Listed, ContactEmail: d.ContactEmail, PlatformURL: d.PlatformURL, PingsDisabled: d.PingsDisabled, SigningKey: d.SigningKey}
}

func (d DirectorySettings) platformURL() string {
	if d.PlatformURL == "" {
		return DefaultPlatformURL
	}
	return strings.TrimRight(d.PlatformURL, "/")
}

// directoryListed reports whether the operator turned directory listing on.
func (s *Service) directoryListed() bool { return s.Providers().Directory.Listed }

// Notices shown next to the index settings. They are plain language for clients to display
// as-is, and deliberately claim no license: the clause that grants the index the right to
// show excerpts is pending legal review.
const (
	DirectoryLicensePending = "[Pending legal review: the clause that lets the public index show excerpts of " +
		"public posts is not written yet. Until it is, listing does not grant anyone a license to your members' content.]"
	directoryNoticeOn = "This instance is listed in the public Gotalk directory. Places whose owners allow it have their " +
		"public boards shown in search engines and the directory. Members can exclude their own posts. " +
		"Turning listing off removes everything from the index, usually within an hour. " + DirectoryLicensePending
	directoryNoticeOff = "This instance is not listed in the public Gotalk directory. Listing it lets place owners choose " +
		"to show their public boards in search engines and the directory. Nothing is shown until a place owner opts in. " +
		DirectoryLicensePending
)

// DirectoryNotice explains the instance's listing to administrators.
func DirectoryNotice(listed bool) string {
	if listed {
		return directoryNoticeOn
	}
	return directoryNoticeOff
}

// PlaceIndexNotice explains a place's search indexing to its owners and members.
func PlaceIndexNotice(p store.Place) string {
	switch {
	case p.InIndex:
		return "Public boards in this place are shown in search engines and the public Gotalk directory. " +
			"Members can exclude their own posts in account settings. Removed posts leave the index, usually within an hour."
	case p.Visibility != "public":
		return "Only public places can appear in search engines. This place is " + strings.ReplaceAll(p.Visibility, "_", "-") + "."
	case p.IsNsfw:
		return "Places marked NSFW cannot appear in search engines."
	case p.Indexable:
		return "Search indexing is allowed for this place, but this instance is not listed in the public directory, so nothing is shown yet."
	default:
		return "This place is not shown in search engines. Allowing it shows its public boards in search engines and the public " +
			"Gotalk directory. Members can exclude their own posts."
	}
}

// BoardIndexNotice explains a board's search indexing to people who manage it.
func BoardIndexNotice(b store.Board) string {
	switch {
	case b.InIndex:
		return "Topics in this board are shown in search engines and the public Gotalk directory."
	case b.IsNsfw:
		return "Boards marked NSFW cannot appear in search engines."
	case !b.Indexable:
		return "This board is excluded from search engines, even when its place is indexed."
	default:
		return "This board is not in search engines: its place is not indexed, or signed-out visitors cannot read it."
	}
}

// AccountIndexNotice explains the per-person exclusion setting.
func AccountIndexNotice(exclude bool) string {
	if exclude {
		return "Your posts are excluded from public search indexes. They are removed from the index, usually within an hour, " +
			"and topics you started are removed with them."
	}
	return "Your posts in places that allow search indexing can appear in search engines and the public Gotalk directory. " +
		"Turn this on to remove them, usually within an hour."
}

// DirectoryPolicyText is suggested text for an instance's policy documents once it is
// listed. Operators publish it themselves; the license clause is a placeholder.
func DirectoryPolicyText() map[string]string {
	return map[string]string{
		PolicyTerms: "## Public directory\n\nThis instance is listed in the public Gotalk directory. Place owners can allow " +
			"their place's public boards to appear in search engines and at the directory. Content in those boards may be " +
			"shown there as excerpts with a link back here.\n\n" + DirectoryLicensePending + "\n",
		PolicyPrivacy: "## Search engines and the public directory\n\nIf a place you post in allows search indexing, your " +
			"public posts there, your username and your display name can appear in search engines and the public Gotalk " +
			"directory. Email addresses, IP addresses and avatars are never shared. You can exclude all of your posts in " +
			"your account settings. Excluded or deleted posts are removed from the index, usually within an hour and at " +
			"most within 24 hours.\n",
		PolicyGuidelines: "## Public places\n\nSome places on this instance are shown in search engines. Write in those " +
			"places as you would in public. Places show a notice when they are indexed.\n",
	}
}

// instanceAudit records an instance-wide change in the instance audit log.
func (s *Service) instanceAudit(ctx context.Context, q *store.Queries, actor *Principal, action, targetType string, targetID *uuid.UUID, meta map[string]any) error {
	meta = maps.Clone(meta)
	if meta == nil {
		meta = map[string]any{}
	}
	if actor != nil && actor.Token != nil {
		meta["via"] = "api_token"
		meta["token_id"] = actor.Token.ID
	}
	raw, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return err
	}
	var actorID *uuid.UUID
	if actor != nil {
		actorID = &actor.User.ID
	}
	return q.CreateInstanceAuditEntry(ctx, store.CreateInstanceAuditEntryParams{
		ID: id, ActorID: actorID, Action: action, TargetType: targetType, TargetID: targetID, Metadata: raw,
	})
}

// ListInstanceAuditLog returns instance-wide audit entries, newest first (administrators).
func (s *Service) ListInstanceAuditLog(ctx context.Context, p *Principal, before *uuid.UUID, limit int32) ([]store.InstanceAuditLog, error) {
	if !p.User.IsInstanceAdmin {
		return nil, apperr.Forbidden("only instance administrators can read the instance audit log")
	}
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	return s.q.ListInstanceAuditLog(ctx, store.ListInstanceAuditLogParams{Before: before, Lim: limit})
}

// directoryKeys is the ping signing key plus every public key to publish.
type directoryKeys struct {
	revision int64
	signing  ed25519.PrivateKey
	// published lists public keys (unpadded base64url), the signing key's first.
	published []string
	// generated is true when the key was generated by the instance rather than configured.
	generated bool
}

// directoryKeys returns the signing key, generating and storing one on first use when none
// is configured. Callers must only use it while listing is on.
func (s *Service) directoryKeys(ctx context.Context) (*directoryKeys, error) {
	p := s.Providers()
	if k := s.dirKeys.Load(); k != nil && k.revision == p.Revision {
		return k, nil
	}
	row, err := s.q.GetDirectoryKeys(ctx)
	if err != nil {
		return nil, err
	}
	k := &directoryKeys{revision: p.Revision}
	if seed := p.Directory.SigningKey; seed != "" {
		if k.signing, err = indexsig.KeyFromSeed(seed); err != nil {
			return nil, err
		}
	} else {
		stored := row.DirectorySigningKey
		if len(stored) != ed25519.SeedSize {
			seed := make([]byte, ed25519.SeedSize)
			if _, err := rand.Read(seed); err != nil {
				return nil, err
			}
			if stored, err = s.q.EnsureDirectorySigningKey(ctx, seed); err != nil {
				return nil, err
			}
		}
		k.signing, k.generated = ed25519.NewKeyFromSeed(stored), true
	}
	k.published = []string{indexsig.KeyID(k.signing)}
	if prev := row.DirectoryPreviousKey; prev != nil && *prev != k.published[0] {
		k.published = append(k.published, *prev)
	}
	s.dirKeys.Store(k)
	return k, nil
}

// RotateDirectoryKey replaces the generated ping signing key. The old public key stays
// published until RevokePreviousDirectoryKey, so pings signed just before the switch still
// verify.
func (s *Service) RotateDirectoryKey(ctx context.Context, p *Principal) error {
	if !p.User.IsInstanceAdmin {
		return apperr.Forbidden("only instance administrators can change instance settings")
	}
	if s.Providers().Directory.SigningKey != "" {
		return apperr.Conflict("the signing key is set in directory.signing_key; replace it there to rotate it")
	}
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return err
	}
	err := s.tx(ctx, func(q *store.Queries) error {
		row, err := q.GetDirectoryKeys(ctx)
		if err != nil {
			return err
		}
		var previous *string
		if len(row.DirectorySigningKey) == ed25519.SeedSize {
			pub := indexsig.KeyID(ed25519.NewKeyFromSeed(row.DirectorySigningKey))
			previous = &pub
		}
		if err := q.RotateDirectorySigningKey(ctx, store.RotateDirectorySigningKeyParams{Previous: previous, Key: seed}); err != nil {
			return err
		}
		if _, err := q.BumpConfigRevision(ctx); err != nil {
			return err
		}
		return s.instanceAudit(ctx, q, p, "directory.key_rotate", "", nil,
			map[string]any{"public_key": indexsig.KeyID(ed25519.NewKeyFromSeed(seed))})
	})
	if err != nil {
		return err
	}
	return s.ReloadProviders(ctx)
}

// RevokePreviousDirectoryKey stops publishing the key retired by the last rotation.
func (s *Service) RevokePreviousDirectoryKey(ctx context.Context, p *Principal) error {
	if !p.User.IsInstanceAdmin {
		return apperr.Forbidden("only instance administrators can change instance settings")
	}
	err := s.tx(ctx, func(q *store.Queries) error {
		n, err := q.RevokeDirectoryPreviousKey(ctx)
		if err != nil {
			return err
		}
		if n == 0 {
			return apperr.NotFound("there is no previous key to revoke")
		}
		if _, err := q.BumpConfigRevision(ctx); err != nil {
			return err
		}
		return s.instanceAudit(ctx, q, p, "directory.key_revoke", "", nil, nil)
	})
	if err != nil {
		return err
	}
	return s.ReloadProviders(ctx)
}

// PublicKey is a published ping verification key.
type PublicKey struct {
	Alg       string
	PublicKey string
}

// DirectoryInfo is what the well-known document and GET /instance say about listing.
type DirectoryInfo struct {
	Listed        bool
	ContactEmail  string
	Keys          []PublicKey
	PlacesIndexed int64
}

// Directory describes the instance's listing. Keys are only loaded (and generated) while
// listing is on.
func (s *Service) Directory(ctx context.Context) (DirectoryInfo, error) {
	d := s.Providers().Directory
	info := DirectoryInfo{Listed: d.Listed, ContactEmail: d.ContactEmail, Keys: []PublicKey{}}
	if !d.Listed {
		return info, nil
	}
	keys, err := s.directoryKeys(ctx)
	if err != nil {
		return info, err
	}
	for _, k := range keys.published {
		info.Keys = append(info.Keys, PublicKey{Alg: "Ed25519", PublicKey: k})
	}
	info.PlacesIndexed, err = s.q.CountPlacesInIndex(ctx)
	return info, err
}

// DirectoryStatus is the administrators' view of directory listing.
type DirectoryStatus struct {
	DirectoryInfo
	Source         string
	Editable       bool
	PlatformURL    string
	Pings          bool
	KeySource      string
	PreviousKey    *string
	LastPingAt     *time.Time
	Notice         string
	LicensePending bool
	PolicyText     map[string]string
}

// DirectoryStatus returns listing details for administrators.
func (s *Service) DirectoryStatus(ctx context.Context, p *Principal) (DirectoryStatus, error) {
	if !p.User.IsInstanceAdmin {
		return DirectoryStatus{}, apperr.Forbidden("only instance administrators can read instance settings")
	}
	prov := s.Providers()
	info, err := s.Directory(ctx)
	if err != nil {
		return DirectoryStatus{}, err
	}
	st := DirectoryStatus{
		DirectoryInfo: info, Source: prov.Sources[SectionDirectory], PlatformURL: prov.Directory.platformURL(),
		Pings: !prov.Directory.PingsDisabled, Notice: DirectoryNotice(info.Listed), LicensePending: true,
		PolicyText: DirectoryPolicyText(), KeySource: "none",
	}
	st.Editable = st.Source != SourceConfig
	if info.Listed {
		keys, err := s.directoryKeys(ctx)
		if err != nil {
			return st, err
		}
		st.KeySource = "configured"
		if keys.generated {
			st.KeySource = "generated"
		}
		if len(keys.published) > 1 {
			st.PreviousKey = &keys.published[1]
		}
	}
	if st.LastPingAt, err = s.q.GetLastIndexPing(ctx); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return st, err
	}
	if !info.Listed {
		st.PlacesIndexed, err = s.q.CountPlacesInIndex(ctx)
	}
	return st, err
}

// checkDirectory validates that the instance's public URL can be listed: HTTPS (HTTP only
// for local development) and reachable. It never contacts the index.
func (s *Service) checkDirectory(ctx context.Context, d DirectorySettings) Check {
	c := Check{Name: "directory"}
	if !d.Listed {
		c.Status, c.Detail = CheckOK, "directory listing is off"
		return c
	}
	base := s.cfg.Server.PublicURL
	if base == "" {
		c.Status, c.Detail = CheckWarning, "server.public_url is not set, so the advertised index URL depends on how each request arrives"
		c.Hint = "Set server.public_url (GOTALK_SERVER_PUBLIC_URL) to the instance's public https:// address."
		return c
	}
	u, err := url.Parse(base)
	if err != nil {
		c.Status, c.Detail = CheckError, "server.public_url is invalid"
		return c
	}
	local := isLocalHost(u.Hostname())
	if u.Scheme != "https" && !local {
		c.Status, c.Detail = CheckError, "the public index only lists instances served over HTTPS"
		c.Hint = "Serve the instance over HTTPS and set server.public_url to its https:// address."
		return c
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, base+"/.well-known/gotalk-instance", nil)
	if err != nil {
		c.Status, c.Detail = CheckError, err.Error()
		return c
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		c.Status, c.Detail = CheckError, fmt.Sprintf("%s is not reachable from this server: %v", base, err)
		c.Hint = "The index fetches /.well-known/gotalk-instance from the public URL; make sure it resolves and answers."
		return c
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK {
		c.Status, c.Detail = CheckError, fmt.Sprintf("%s/.well-known/gotalk-instance answered %d", base, res.StatusCode)
		return c
	}
	if local {
		c.Status, c.Detail = CheckWarning, base+" is a local address; the public index cannot reach it (fine for development)"
		return c
	}
	c.Status, c.Detail = CheckOK, base+" is reachable over HTTPS"
	return c
}

func isLocalHost(host string) bool {
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast())
}

// validateDirectory checks proposed directory settings.
func validateDirectory(d DirectorySettings) error {
	return config.ValidateDirectory(d.config())
}
