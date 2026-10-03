package service

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/parkerbrown98/gotalk-server/internal/apperr"
	"github.com/parkerbrown98/gotalk-server/internal/database"
	"github.com/parkerbrown98/gotalk-server/internal/permissions"
	"github.com/parkerbrown98/gotalk-server/internal/store"
)

const (
	maxReasonLen     = 512
	maxReportDetails = 2000
	minTimeout       = time.Minute
	maxTimeout       = 28 * 24 * time.Hour
	maxBanDuration   = 365 * 24 * time.Hour
)

var auditActionPattern = regexp.MustCompile(`^[a-z_]+(\.[a-z_]+)?$`)

// audit records a moderation or administrative action in the place's audit log.
func (s *Service) audit(ctx context.Context, q *store.Queries, placeID uuid.UUID, actor *Principal, action, targetType string, targetID *uuid.UUID, reason string, meta map[string]any) error {
	if meta == nil {
		meta = map[string]any{}
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
	return q.CreateAuditEntry(ctx, store.CreateAuditEntryParams{
		ID: id, PlaceID: placeID, ActorID: actorID, Action: action, TargetType: targetType,
		TargetID: targetID, Reason: reason, Metadata: raw,
	})
}

type AuditFilter struct {
	// Action matches exactly ("member.ban") or by category ("member").
	Action   string
	ActorID  *uuid.UUID
	TargetID *uuid.UUID
}

type AuditView struct {
	Entry store.AuditLog
	Actor *store.User
}

func (s *Service) ListAuditLog(ctx context.Context, p *Principal, ref string, filter AuditFilter, page Pagination) ([]AuditView, error) {
	var action *string
	if filter.Action != "" {
		if !auditActionPattern.MatchString(filter.Action) {
			return nil, apperr.Invalid("action filter is invalid")
		}
		action = &filter.Action
	}
	place, _, err := s.requirePermission(ctx, s.q, p, ref, permissions.ViewAuditLog)
	if err != nil {
		return nil, err
	}
	page = page.normalized()
	rows, err := s.q.ListAuditLog(ctx, store.ListAuditLogParams{
		PlaceID: place.ID, Action: action, ActorID: filter.ActorID, TargetID: filter.TargetID,
		Lim: page.Limit, Off: page.Offset,
	})
	if err != nil {
		return nil, err
	}
	var ids []uuid.UUID
	for _, r := range rows {
		if r.ActorID != nil {
			ids = append(ids, *r.ActorID)
		}
	}
	users, err := s.usersByID(ctx, s.q, ids)
	if err != nil {
		return nil, err
	}
	out := make([]AuditView, len(rows))
	for i, r := range rows {
		out[i] = AuditView{Entry: r, Actor: ptrUser(users, r.ActorID)}
	}
	return out, nil
}

type ReportInput struct {
	PostID    *uuid.UUID
	MessageID *uuid.UUID
	UserID    *uuid.UUID
	Reason    string
	Details   string
}

func validateReportReason(r string) error {
	switch r {
	case "spam", "harassment", "inappropriate", "off_topic", "other":
		return nil
	}
	return apperr.Invalid("reason must be one of spam, harassment, inappropriate, off_topic, other")
}

// CreateReport flags a post, chat message or member for moderators. Anyone who can see a
// post may report it; reporting a message or a user requires membership.
func (s *Service) CreateReport(ctx context.Context, p *Principal, ref string, in ReportInput) (store.Report, error) {
	targets := 0
	for _, t := range []*uuid.UUID{in.PostID, in.MessageID, in.UserID} {
		if t != nil {
			targets++
		}
	}
	if targets != 1 {
		return store.Report{}, apperr.Invalid("report exactly one of post_id, message_id or user_id")
	}
	if err := validateReportReason(in.Reason); err != nil {
		return store.Report{}, err
	}
	in.Details = strings.TrimSpace(in.Details)
	if len([]rune(in.Details)) > maxReportDetails {
		return store.Report{}, apperr.Invalid("details must be at most %d characters", maxReportDetails)
	}

	f, err := s.forum(ctx, s.q, p, ref)
	if err != nil {
		return store.Report{}, err
	}
	params := store.CreateReportParams{
		PlaceID: f.place.ID, ReporterID: &p.User.ID, Reason: in.Reason, Details: in.Details,
	}
	if in.PostID != nil {
		post, err := s.q.GetPost(ctx, *in.PostID)
		if err != nil || post.PlaceID != f.place.ID || post.DeletedAt != nil || !f.canView(post.BoardID) {
			return store.Report{}, apperr.NotFound("post not found")
		}
		topic, err := s.q.GetTopic(ctx, post.TopicID)
		if err != nil || topic.DeletedAt != nil {
			return store.Report{}, apperr.NotFound("post not found")
		}
		if isAuthor(post.AuthorID, p) {
			return store.Report{}, apperr.Invalid("you cannot report your own post")
		}
		params.TargetType, params.PostID, params.TargetUserID = "post", &post.ID, post.AuthorID
		params.ContentSnapshot = post.Content
		if post.PostNumber == 1 {
			params.ContentSnapshot = topic.Title + "\n\n" + post.Content
		}
	} else if in.MessageID != nil {
		cc, m, err := s.messageFor(ctx, s.q, p, *in.MessageID)
		if err != nil || cc.isDM() || *cc.placeID() != f.place.ID {
			return store.Report{}, apperr.NotFound("message not found")
		}
		if isAuthor(m.AuthorID, p) {
			return store.Report{}, apperr.Invalid("you cannot report your own message")
		}
		params.TargetType, params.MessageID, params.ChannelID, params.TargetUserID = "message", &m.ID, &m.ChannelID, m.AuthorID
		params.ContentSnapshot = m.Content
	} else {
		if !f.acc.IsMember {
			return store.Report{}, apperr.Forbidden("you are not a member of this place")
		}
		if *in.UserID == p.User.ID {
			return store.Report{}, apperr.Invalid("you cannot report yourself")
		}
		isMember, err := s.q.IsMember(ctx, store.IsMemberParams{PlaceID: f.place.ID, UserID: *in.UserID})
		if err != nil {
			return store.Report{}, err
		}
		if !isMember {
			return store.Report{}, apperr.NotFound("member not found")
		}
		params.TargetType, params.TargetUserID = "user", in.UserID
	}
	if params.ID, err = uuid.NewV7(); err != nil {
		return store.Report{}, err
	}
	report, err := s.q.CreateReport(ctx, params)
	if database.IsUniqueViolation(err, "reports_open_dedupe") {
		return store.Report{}, apperr.Conflict("you already have an open report about this")
	}
	return report, err
}

type ReportView struct {
	Report     store.Report
	Reporter   *store.User
	TargetUser *store.User
	TopicID    *uuid.UUID
}

func (s *Service) reportViews(ctx context.Context, q *store.Queries, reports []store.Report) ([]ReportView, error) {
	var userIDs, postIDs []uuid.UUID
	for _, r := range reports {
		for _, id := range []*uuid.UUID{r.ReporterID, r.TargetUserID} {
			if id != nil {
				userIDs = append(userIDs, *id)
			}
		}
		if r.PostID != nil {
			postIDs = append(postIDs, *r.PostID)
		}
	}
	users, err := s.usersByID(ctx, q, userIDs)
	if err != nil {
		return nil, err
	}
	topicOf := map[uuid.UUID]uuid.UUID{}
	for _, id := range dedupe(postIDs) {
		post, err := q.GetPost(ctx, id)
		if err != nil {
			return nil, err
		}
		topicOf[id] = post.TopicID
	}
	out := make([]ReportView, len(reports))
	for i, r := range reports {
		v := ReportView{Report: r, Reporter: ptrUser(users, r.ReporterID), TargetUser: ptrUser(users, r.TargetUserID)}
		if r.PostID != nil {
			t := topicOf[*r.PostID]
			v.TopicID = &t
		}
		out[i] = v
	}
	return out, nil
}

// ListReports is the moderation queue (MANAGE_REPORTS), newest first.
func (s *Service) ListReports(ctx context.Context, p *Principal, ref, status string, page Pagination) ([]ReportView, error) {
	var st *string
	if status != "" {
		if status != "open" && status != "resolved" && status != "dismissed" {
			return nil, apperr.Invalid("status must be one of open, resolved, dismissed")
		}
		st = &status
	}
	place, _, err := s.requirePermission(ctx, s.q, p, ref, permissions.ManageReports)
	if err != nil {
		return nil, err
	}
	page = page.normalized()
	reports, err := s.q.ListReports(ctx, store.ListReportsParams{PlaceID: place.ID, Status: st, Lim: page.Limit, Off: page.Offset})
	if err != nil {
		return nil, err
	}
	return s.reportViews(ctx, s.q, reports)
}

// ResolveReport closes a report as resolved (action taken) or dismissed (no action).
func (s *Service) ResolveReport(ctx context.Context, p *Principal, ref string, reportID uuid.UUID, status, note string) (ReportView, error) {
	if status != "resolved" && status != "dismissed" {
		return ReportView{}, apperr.Invalid("status must be resolved or dismissed")
	}
	if len([]rune(note)) > maxReasonLen {
		return ReportView{}, apperr.Invalid("resolution_note must be at most %d characters", maxReasonLen)
	}
	var view ReportView
	err := s.tx(ctx, func(q *store.Queries) error {
		place, _, err := s.requirePermission(ctx, q, p, ref, permissions.ManageReports)
		if err != nil {
			return err
		}
		if _, err := q.GetReport(ctx, store.GetReportParams{ID: reportID, PlaceID: place.ID}); err != nil {
			return notFound(err, "report not found")
		}
		report, err := q.ResolveReport(ctx, store.ResolveReportParams{
			ID: reportID, PlaceID: place.ID, Status: status, ResolvedBy: &p.User.ID, ResolutionNote: note,
		})
		if err != nil {
			return err
		}
		action := map[string]string{"resolved": "report.resolve", "dismissed": "report.dismiss"}[status]
		if err := s.audit(ctx, q, place.ID, p, action, "report", &report.ID, note,
			map[string]any{"target_type": report.TargetType, "post_id": report.PostID, "user_id": report.TargetUserID}); err != nil {
			return err
		}
		views, err := s.reportViews(ctx, q, []store.Report{report})
		if err != nil {
			return err
		}
		view = views[0]
		return nil
	})
	return view, err
}

// moderationTarget checks the caller holds perm and outranks the target member.
func (s *Service) moderationTarget(ctx context.Context, q *store.Queries, p *Principal, ref string, userID uuid.UUID, perm permissions.Permission, verb string) (store.Place, error) {
	if userID == p.User.ID {
		return store.Place{}, apperr.Invalid("you cannot %s yourself", verb)
	}
	place, acc, err := s.requirePermission(ctx, q, p, ref, perm)
	if err != nil {
		return place, err
	}
	target, err := s.targetStanding(ctx, q, place, userID)
	if err != nil {
		return place, err
	}
	if !acc.Member.Outranks(target) {
		return place, apperr.Forbidden("you can only %s members ranked below you", verb)
	}
	return place, nil
}

// TimeoutMember stops a member from posting, reacting and other participation until the
// timeout ends, without removing them (MODERATE_MEMBERS).
func (s *Service) TimeoutMember(ctx context.Context, p *Principal, ref string, userID uuid.UUID, d time.Duration, reason string) (MemberView, error) {
	if d < minTimeout || d > maxTimeout {
		return MemberView{}, apperr.Invalid("duration must be between %d and %d seconds", int(minTimeout.Seconds()), int(maxTimeout.Seconds()))
	}
	if len([]rune(reason)) > maxReasonLen {
		return MemberView{}, apperr.Invalid("reason must be at most %d characters", maxReasonLen)
	}
	var view MemberView
	err := s.tx(ctx, func(q *store.Queries) error {
		place, err := s.moderationTarget(ctx, q, p, ref, userID, permissions.ModerateMembers, "time out")
		if err != nil {
			return err
		}
		until := time.Now().Add(d).UTC().Truncate(time.Second)
		if err := q.SetMemberTimeout(ctx, store.SetMemberTimeoutParams{PlaceID: place.ID, UserID: userID, TimeoutUntil: &until}); err != nil {
			return err
		}
		s.queueVoiceSync(ctx, q, place.ID, userID)
		if err := s.audit(ctx, q, place.ID, p, "member.timeout", "user", &userID, reason,
			map[string]any{"until": until, "duration_seconds": int64(d.Seconds())}); err != nil {
			return err
		}
		if err := s.notifyModeration(ctx, q, place, userID, p, "member.timeout", reason, map[string]any{"until": until}); err != nil {
			return err
		}
		view, err = s.getMember(ctx, q, place.ID, userID)
		return err
	})
	return view, err
}

func (s *Service) ClearTimeout(ctx context.Context, p *Principal, ref string, userID uuid.UUID) (MemberView, error) {
	var view MemberView
	err := s.tx(ctx, func(q *store.Queries) error {
		place, err := s.moderationTarget(ctx, q, p, ref, userID, permissions.ModerateMembers, "time out")
		if err != nil {
			return err
		}
		if err := q.SetMemberTimeout(ctx, store.SetMemberTimeoutParams{PlaceID: place.ID, UserID: userID}); err != nil {
			return err
		}
		s.queueVoiceSync(ctx, q, place.ID, userID)
		if err := s.audit(ctx, q, place.ID, p, "member.timeout_clear", "user", &userID, "", nil); err != nil {
			return err
		}
		view, err = s.getMember(ctx, q, place.ID, userID)
		return err
	})
	return view, err
}

// WarnMember sends a formal warning that is recorded in the audit log (MODERATE_MEMBERS).
func (s *Service) WarnMember(ctx context.Context, p *Principal, ref string, userID uuid.UUID, reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" || len([]rune(reason)) > maxReasonLen {
		return apperr.Invalid("reason must be 1-%d characters", maxReasonLen)
	}
	return s.tx(ctx, func(q *store.Queries) error {
		place, err := s.moderationTarget(ctx, q, p, ref, userID, permissions.ModerateMembers, "warn")
		if err != nil {
			return err
		}
		if err := s.audit(ctx, q, place.ID, p, "member.warn", "user", &userID, reason, nil); err != nil {
			return err
		}
		return s.notifyModeration(ctx, q, place, userID, p, "member.warn", reason, nil)
	})
}
