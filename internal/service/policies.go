package service

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/parkerbrown98/gotalk-server/internal/apperr"
	"github.com/parkerbrown98/gotalk-server/internal/database"
	"github.com/parkerbrown98/gotalk-server/internal/store"
)

// Policy kinds. Their names double as consent purposes.
const (
	PolicyTerms      = "terms"
	PolicyPrivacy    = "privacy"
	PolicyGuidelines = "guidelines"

	maxPolicyTitleLen   = 200
	maxPolicyContentLen = 200_000
	maxPolicySummaryLen = 2000
	maxPolicyLeadTime   = 366 * 24 * time.Hour
)

// PolicyKinds lists the policy documents an instance can publish.
var PolicyKinds = []string{PolicyTerms, PolicyPrivacy, PolicyGuidelines}

var defaultPolicyTitles = map[string]string{
	PolicyTerms:      "Terms of Service",
	PolicyPrivacy:    "Privacy Policy",
	PolicyGuidelines: "Community Guidelines",
}

var consentPurposePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{1,31}$`)

func validatePolicyKind(kind string) error {
	if !slices.Contains(PolicyKinds, kind) {
		return apperr.NotFound("unknown policy %q; policies are %s", kind, strings.Join(PolicyKinds, ", "))
	}
	return nil
}

type PolicyInput struct {
	Title   string
	Content string
	// Summary describes what changed since the previous version.
	Summary         string
	RequiresConsent bool
	// EffectiveAt schedules the version; nil means now.
	EffectiveAt *time.Time
}

// PublishPolicy adds a new version of a policy document (instance administrators only).
// Versions are immutable, so the list of versions doubles as the policy's changelog.
func (s *Service) PublishPolicy(ctx context.Context, p *Principal, kind string, in PolicyInput) (store.PolicyDocument, error) {
	if !p.User.IsInstanceAdmin {
		return store.PolicyDocument{}, apperr.Forbidden("only instance administrators can publish policies")
	}
	if err := validatePolicyKind(kind); err != nil {
		return store.PolicyDocument{}, err
	}
	title := strings.TrimSpace(in.Title)
	if title == "" {
		title = defaultPolicyTitles[kind]
	}
	if utf8.RuneCountInString(title) > maxPolicyTitleLen {
		return store.PolicyDocument{}, apperr.Invalid("title must be at most %d characters", maxPolicyTitleLen)
	}
	if strings.TrimSpace(in.Content) == "" || utf8.RuneCountInString(in.Content) > maxPolicyContentLen {
		return store.PolicyDocument{}, apperr.Invalid("content must be 1-%d characters", maxPolicyContentLen)
	}
	if utf8.RuneCountInString(in.Summary) > maxPolicySummaryLen {
		return store.PolicyDocument{}, apperr.Invalid("summary must be at most %d characters", maxPolicySummaryLen)
	}
	now := time.Now()
	effective := now
	if in.EffectiveAt != nil {
		switch {
		case in.EffectiveAt.Before(now.Add(-time.Minute)):
			return store.PolicyDocument{}, apperr.Invalid("effective_at cannot be in the past")
		case in.EffectiveAt.After(now.Add(maxPolicyLeadTime)):
			return store.PolicyDocument{}, apperr.Invalid("effective_at can be at most a year ahead")
		case in.EffectiveAt.After(now):
			effective = *in.EffectiveAt
		}
	}
	id, err := uuid.NewV7()
	if err != nil {
		return store.PolicyDocument{}, err
	}
	doc, err := s.q.CreatePolicyDocument(ctx, store.CreatePolicyDocumentParams{
		ID: id, Kind: kind, Title: title, Content: in.Content, Summary: strings.TrimSpace(in.Summary),
		RequiresConsent: in.RequiresConsent, EffectiveAt: effective, PublishedBy: &p.User.ID,
	})
	if database.IsUniqueViolation(err, "policy_documents_kind_version_key") {
		return store.PolicyDocument{}, apperr.Conflict("another version was published at the same time; try again")
	}
	if err == nil {
		s.log.Info("policy published", "kind", kind, "version", doc.Version, "effective_at", doc.EffectiveAt)
	}
	return doc, err
}

// CurrentPolicies returns the version of each policy that is in effect now.
func (s *Service) CurrentPolicies(ctx context.Context) ([]store.PolicyDocument, error) {
	return s.q.ListCurrentPolicies(ctx)
}

// GetPolicy returns the version of a policy in effect now.
func (s *Service) GetPolicy(ctx context.Context, kind string) (store.PolicyDocument, error) {
	if err := validatePolicyKind(kind); err != nil {
		return store.PolicyDocument{}, err
	}
	doc, err := s.q.GetCurrentPolicy(ctx, kind)
	return doc, notFound(err, "this instance has not published a %s policy", kind)
}

// ListPolicyVersions returns every version of a policy, newest first, including versions
// scheduled for the future.
func (s *Service) ListPolicyVersions(ctx context.Context, kind string) ([]store.PolicyDocument, error) {
	if err := validatePolicyKind(kind); err != nil {
		return nil, err
	}
	return s.q.ListPolicyVersions(ctx, kind)
}

func (s *Service) GetPolicyVersion(ctx context.Context, kind string, version int32) (store.PolicyDocument, error) {
	if err := validatePolicyKind(kind); err != nil {
		return store.PolicyDocument{}, err
	}
	doc, err := s.q.GetPolicyVersion(ctx, store.GetPolicyVersionParams{Kind: kind, Version: version})
	return doc, notFound(err, "policy version not found")
}

type ConsentInput struct {
	// Purpose is a policy kind or another processing purpose, such as "analytics".
	Purpose string
	Granted bool
	// PolicyVersion is the version being accepted for policy purposes; nil means the
	// current version.
	PolicyVersion *int32
}

// RecordConsent appends to the caller's consent log. Records are never edited: giving or
// withdrawing consent again adds a new record.
func (s *Service) RecordConsent(ctx context.Context, p *Principal, in ConsentInput, client ClientInfo) (store.ConsentRecord, error) {
	return s.recordConsent(ctx, s.q, p.User.ID, in, client)
}

func (s *Service) recordConsent(ctx context.Context, q *store.Queries, userID uuid.UUID, in ConsentInput, client ClientInfo) (store.ConsentRecord, error) {
	if !consentPurposePattern.MatchString(in.Purpose) {
		return store.ConsentRecord{}, apperr.Invalid("purpose must be 2-32 lowercase letters, numbers or '_', starting with a letter")
	}
	if slices.Contains(PolicyKinds, in.Purpose) {
		var (
			doc store.PolicyDocument
			err error
		)
		if in.PolicyVersion == nil {
			doc, err = q.GetCurrentPolicy(ctx, in.Purpose)
		} else {
			doc, err = q.GetPolicyVersion(ctx, store.GetPolicyVersionParams{Kind: in.Purpose, Version: *in.PolicyVersion})
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return store.ConsentRecord{}, apperr.NotFound("policy version not found")
		}
		if err != nil {
			return store.ConsentRecord{}, err
		}
		in.PolicyVersion = &doc.Version
	} else if in.PolicyVersion != nil {
		return store.ConsentRecord{}, apperr.Invalid("policy_version only applies to policy purposes (%s)", strings.Join(PolicyKinds, ", "))
	}
	id, err := uuid.NewV7()
	if err != nil {
		return store.ConsentRecord{}, err
	}
	return q.CreateConsentRecord(ctx, store.CreateConsentRecordParams{
		ID: id, UserID: userID, Purpose: in.Purpose, PolicyVersion: in.PolicyVersion, Granted: in.Granted,
		IpAddress: client.IP, UserAgent: truncate(client.UserAgent, 512),
	})
}

// acceptCurrentPolicies records consent to every policy in effect that requires it.
func (s *Service) acceptCurrentPolicies(ctx context.Context, q *store.Queries, userID uuid.UUID, client ClientInfo) error {
	docs, err := q.ListCurrentPolicies(ctx)
	if err != nil {
		return err
	}
	for _, d := range docs {
		if !d.RequiresConsent {
			continue
		}
		if _, err := s.recordConsent(ctx, q, userID, ConsentInput{Purpose: d.Kind, Granted: true, PolicyVersion: &d.Version}, client); err != nil {
			return err
		}
	}
	return nil
}

// ConsentStatus is a user's latest decision per purpose, plus the policies in effect that
// require consent the user has not given to their current version.
type ConsentStatus struct {
	Consents    []store.ConsentRecord
	Outstanding []store.PolicyDocument
}

func (s *Service) GetConsentStatus(ctx context.Context, p *Principal) (ConsentStatus, error) {
	latest, err := s.q.ListLatestConsents(ctx, p.User.ID)
	if err != nil {
		return ConsentStatus{}, err
	}
	docs, err := s.q.ListCurrentPolicies(ctx)
	if err != nil {
		return ConsentStatus{}, err
	}
	out := ConsentStatus{Consents: latest, Outstanding: []store.PolicyDocument{}}
	for _, d := range docs {
		if !d.RequiresConsent {
			continue
		}
		i := slices.IndexFunc(latest, func(c store.ConsentRecord) bool { return c.Purpose == d.Kind })
		if i < 0 || !latest[i].Granted || latest[i].PolicyVersion == nil || *latest[i].PolicyVersion < d.Version {
			out.Outstanding = append(out.Outstanding, d)
		}
	}
	return out, nil
}

func (s *Service) ListConsentHistory(ctx context.Context, p *Principal, page Pagination) ([]store.ConsentRecord, error) {
	page = page.normalized()
	return s.q.ListConsentHistory(ctx, store.ListConsentHistoryParams{UserID: p.User.ID, Lim: page.Limit, Off: page.Offset})
}

// Moderation actions counted by transparency reports.
var transparencyActions = []string{
	"member.warn", "member.timeout", "member.kick", "member.ban", "member.unban", "member.voice_disconnect",
	"message.delete", "post.delete", "topic.delete",
}

const maxTransparencyWindow = 366 * 24 * time.Hour

// TransparencyReport aggregates moderation activity over a period, for the whole instance
// (PlaceID nil) or one place. It holds counts only, never content or identities.
type TransparencyReport struct {
	PlaceID        *uuid.UUID
	Since, Until   time.Time
	ReportsTotal   int64
	ReportsReason  map[string]int64
	ReportsStatus  map[string]int64
	Actions        map[string]int64
	ContentRemoved int64
}

func transparencyWindow(since, until *time.Time) (time.Time, time.Time, error) {
	end := time.Now().UTC()
	if until != nil {
		end = until.UTC()
	}
	start := end.Add(-30 * 24 * time.Hour)
	if since != nil {
		start = since.UTC()
	}
	if !start.Before(end) {
		return start, end, apperr.Invalid("since must be before until")
	}
	if end.Sub(start) > maxTransparencyWindow {
		return start, end, apperr.Invalid("the period can be at most 366 days")
	}
	return start, end, nil
}

// InstanceTransparency reports moderation statistics across every place.
func (s *Service) InstanceTransparency(ctx context.Context, since, until *time.Time) (TransparencyReport, error) {
	return s.transparency(ctx, nil, since, until)
}

// PlaceTransparency reports moderation statistics for a place anyone who can see the
// place may read.
func (s *Service) PlaceTransparency(ctx context.Context, p *Principal, ref string, since, until *time.Time) (TransparencyReport, error) {
	place, _, err := s.placeFor(ctx, s.q, p, ref)
	if err != nil {
		return TransparencyReport{}, err
	}
	return s.transparency(ctx, &place.ID, since, until)
}

func (s *Service) transparency(ctx context.Context, placeID *uuid.UUID, since, until *time.Time) (TransparencyReport, error) {
	start, end, err := transparencyWindow(since, until)
	if err != nil {
		return TransparencyReport{}, err
	}
	r := TransparencyReport{
		PlaceID: placeID, Since: start, Until: end,
		ReportsReason: map[string]int64{}, ReportsStatus: map[string]int64{}, Actions: map[string]int64{},
	}
	reasons, err := s.q.ReportCountsByReason(ctx, store.ReportCountsByReasonParams{PlaceID: placeID, Since: start, Until: end})
	if err != nil {
		return r, err
	}
	for _, row := range reasons {
		r.ReportsReason[row.Reason] = row.Total
		r.ReportsTotal += row.Total
	}
	statuses, err := s.q.ReportCountsByStatus(ctx, store.ReportCountsByStatusParams{PlaceID: placeID, Since: start, Until: end})
	if err != nil {
		return r, err
	}
	for _, row := range statuses {
		r.ReportsStatus[row.Status] = row.Total
	}
	actions, err := s.q.AuditActionCounts(ctx, store.AuditActionCountsParams{
		PlaceID: placeID, Actions: transparencyActions, Since: start, Until: end,
	})
	if err != nil {
		return r, err
	}
	for _, row := range actions {
		r.Actions[row.Action] = row.Total
		switch row.Action {
		case "message.delete", "post.delete", "topic.delete":
			r.ContentRemoved += row.Total
		}
	}
	return r, nil
}
