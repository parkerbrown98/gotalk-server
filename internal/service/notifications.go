package service

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/parkerbrown98/gotalk-server/internal/apperr"
	"github.com/parkerbrown98/gotalk-server/internal/permissions"
	"github.com/parkerbrown98/gotalk-server/internal/store"
)

// Notification kinds.
const (
	NotifyMention    = "mention"
	NotifyReply      = "reply"
	NotifyTopicReply = "topic_reply"
	NotifyNewTopic   = "new_topic"
	NotifyReaction   = "reaction"
	NotifySolution   = "solution"
	NotifyModeration = "moderation"
)

const (
	maxMentionsPerPost = 20
	excerptLen         = 200
)

type recipient struct {
	userID uuid.UUID
	kind   string
}

// recipients collects at most one notification per user; the first kind added wins, so
// callers add higher-priority kinds (mentions, direct replies) first.
type recipients struct {
	order []recipient
	seen  map[uuid.UUID]bool
	skip  uuid.UUID
}

func newRecipients(actor uuid.UUID) *recipients {
	return &recipients{seen: map[uuid.UUID]bool{}, skip: actor}
}

func (r *recipients) add(kind string, ids ...uuid.UUID) {
	for _, id := range ids {
		if id == uuid.Nil || id == r.skip || r.seen[id] {
			continue
		}
		r.seen[id] = true
		r.order = append(r.order, recipient{userID: id, kind: kind})
	}
}

// forumEvent describes forum activity that may notify users.
type forumEvent struct {
	f       *forumScope
	topic   store.Topic
	post    *store.Post
	actorID uuid.UUID
	extra   map[string]any
}

func excerpt(content string) string {
	content = strings.Join(strings.Fields(content), " ")
	if utf8.RuneCountInString(content) <= excerptLen {
		return content
	}
	return string([]rune(content)[:excerptLen]) + "…"
}

// deliver filters recipients to members who can still see the board and have not muted
// the most specific scope (topic, then board and its ancestors, then place), then stores
// the notifications.
func (s *Service) deliver(ctx context.Context, q *store.Queries, ev forumEvent, rs *recipients) error {
	if len(rs.order) == 0 {
		return nil
	}
	f := ev.f
	ids := make([]uuid.UUID, len(rs.order))
	for i, r := range rs.order {
		ids[i] = r.userID
	}
	members, err := q.ListMembersAmong(ctx, store.ListMembersAmongParams{PlaceID: f.place.ID, UserIds: ids})
	if err != nil {
		return err
	}
	isMember := make(map[uuid.UUID]bool, len(members))
	for _, m := range members {
		isMember[m.UserID] = true
	}
	assigned, err := q.ListMemberRoleIDs(ctx, store.ListMemberRoleIDsParams{PlaceID: f.place.ID, UserIds: ids})
	if err != nil {
		return err
	}
	roleIDs := map[uuid.UUID][]uuid.UUID{}
	for _, a := range assigned {
		roleIDs[a.UserID] = append(roleIDs[a.UserID], a.RoleID)
	}
	roles, err := q.ListRoles(ctx, f.place.ID)
	if err != nil {
		return err
	}
	rolePerms := make(map[uuid.UUID]permissions.Permission, len(roles))
	for _, r := range roles {
		rolePerms[r.ID] = permissions.Permission(r.Permissions)
	}

	// Most specific first.
	targets := []uuid.UUID{ev.topic.ID}
	chain := f.chain(ev.topic.BoardID)
	for i := len(chain) - 1; i >= 0; i-- {
		targets = append(targets, chain[i].ID)
	}
	targets = append(targets, f.place.ID)
	subRows, err := q.ListSubscriptionsFor(ctx, store.ListSubscriptionsForParams{UserIds: ids, TargetIds: targets})
	if err != nil {
		return err
	}
	subs := map[uuid.UUID]map[uuid.UUID]string{}
	for _, sr := range subRows {
		if subs[sr.UserID] == nil {
			subs[sr.UserID] = map[uuid.UUID]string{}
		}
		subs[sr.UserID][sr.TargetID] = sr.Level
	}

	var out []recipient
	for _, r := range rs.order {
		if !isMember[r.userID] {
			continue
		}
		m := permissions.Member{
			IsOwner:       f.place.OwnerID == r.userID,
			Raw:           permissions.Permission(f.defaultRole.Permissions),
			DefaultRoleID: f.defaultRole.ID,
			RoleIDs:       append([]uuid.UUID{f.defaultRole.ID}, roleIDs[r.userID]...),
		}
		for _, id := range roleIDs[r.userID] {
			m.Raw |= rolePerms[id]
		}
		if !f.visibleTo(m, ev.topic.BoardID) {
			continue
		}
		muted := false
		for _, t := range targets {
			if level, ok := subs[r.userID][t]; ok {
				muted = level == "muted"
				break
			}
		}
		if !muted {
			out = append(out, r)
		}
	}

	data := map[string]any{
		"place_name":  f.place.Name,
		"place_slug":  f.place.Slug,
		"topic_title": ev.topic.Title,
	}
	if ev.post != nil {
		data["excerpt"] = excerpt(ev.post.Content)
		data["post_number"] = ev.post.PostNumber
	}
	for k, v := range ev.extra {
		data[k] = v
	}
	var postID *uuid.UUID
	if ev.post != nil {
		postID = &ev.post.ID
	}
	return s.insertNotifications(ctx, q, out, &f.place.ID, &ev.topic.ID, postID, ev.actorID, data)
}

func (s *Service) insertNotifications(ctx context.Context, q *store.Queries, rs []recipient, placeID, topicID, postID *uuid.UUID, actorID uuid.UUID, data map[string]any) error {
	if len(rs) == 0 {
		return nil
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	var actor *uuid.UUID
	if actorID != uuid.Nil {
		actor = &actorID
	}
	params := make([]store.CreateNotificationParams, len(rs))
	for i, r := range rs {
		id, err := uuid.NewV7()
		if err != nil {
			return err
		}
		params[i] = store.CreateNotificationParams{
			ID: id, UserID: r.userID, Kind: r.kind, PlaceID: placeID, TopicID: topicID,
			PostID: postID, ActorID: actor, Data: raw,
		}
	}
	var batchErr error
	q.CreateNotification(ctx, params).Exec(func(_ int, err error) {
		if err != nil && batchErr == nil {
			batchErr = err
		}
	})
	return batchErr
}

// notifyModeration tells a user about a moderation action taken against them. These
// bypass mutes: users should always learn why they were restricted.
func (s *Service) notifyModeration(ctx context.Context, q *store.Queries, place store.Place, userID uuid.UUID, actor *Principal, action, reason string, extra map[string]any) error {
	data := map[string]any{"place_name": place.Name, "place_slug": place.Slug, "action": action, "reason": reason}
	for k, v := range extra {
		data[k] = v
	}
	var topicID, postID *uuid.UUID
	if v, ok := extra["topic_id"].(uuid.UUID); ok {
		topicID = &v
	}
	if v, ok := extra["post_id"].(uuid.UUID); ok {
		postID = &v
	}
	return s.insertNotifications(ctx, q, []recipient{{userID: userID, kind: NotifyModeration}},
		&place.ID, topicID, postID, actor.User.ID, data)
}

var (
	mentionPattern = regexp.MustCompile(`(?:^|[^\w@./-])@([a-zA-Z0-9_.-]{3,32})`)
	codePattern    = regexp.MustCompile("(?s)```.*?```|`[^`\n]*`")
)

// mentions extracts lower-cased @usernames from Markdown, ignoring code spans and blocks.
func mentions(content string) []string {
	content = codePattern.ReplaceAllString(content, " ")
	seen := map[string]bool{}
	var out []string
	for _, m := range mentionPattern.FindAllStringSubmatch(content, -1) {
		name := strings.ToLower(strings.TrimRight(m[1], ".-"))
		if len(name) < 3 || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
		if len(out) == maxMentionsPerPost {
			break
		}
	}
	return out
}

func (s *Service) mentionedUserIDs(ctx context.Context, q *store.Queries, content string, exclude []string) ([]uuid.UUID, error) {
	names := mentions(content)
	if len(exclude) > 0 {
		skip := setOf(exclude...)
		names = slicesFilter(names, func(n string) bool { return !skip[n] })
	}
	if len(names) == 0 {
		return nil, nil
	}
	users, err := q.ListUsersByUsernames(ctx, names)
	if err != nil {
		return nil, err
	}
	ids := make([]uuid.UUID, len(users))
	for i, u := range users {
		ids[i] = u.ID
	}
	return ids, nil
}

func slicesFilter[T any](in []T, keep func(T) bool) []T {
	out := in[:0:0]
	for _, v := range in {
		if keep(v) {
			out = append(out, v)
		}
	}
	return out
}

// NotificationView is a notification plus its actor (nil if the actor's account is gone).
type NotificationView struct {
	Notification store.Notification
	Actor        *store.User
}

func (s *Service) ListNotifications(ctx context.Context, p *Principal, unreadOnly bool, page Pagination) ([]NotificationView, error) {
	page = page.normalized()
	rows, err := s.q.ListNotifications(ctx, store.ListNotificationsParams{
		UserID: p.User.ID, UnreadOnly: unreadOnly, Lim: page.Limit, Off: page.Offset,
	})
	if err != nil {
		return nil, err
	}
	var actorIDs []uuid.UUID
	for _, n := range rows {
		if n.ActorID != nil {
			actorIDs = append(actorIDs, *n.ActorID)
		}
	}
	users, err := s.usersByID(ctx, s.q, actorIDs)
	if err != nil {
		return nil, err
	}
	out := make([]NotificationView, len(rows))
	for i, n := range rows {
		out[i] = NotificationView{Notification: n}
		if n.ActorID != nil {
			if u, ok := users[*n.ActorID]; ok {
				out[i].Actor = &u
			}
		}
	}
	return out, nil
}

func (s *Service) CountUnreadNotifications(ctx context.Context, p *Principal) (int64, error) {
	return s.q.CountUnreadNotifications(ctx, p.User.ID)
}

func (s *Service) MarkNotificationRead(ctx context.Context, p *Principal, id uuid.UUID) error {
	n, err := s.q.MarkNotificationRead(ctx, store.MarkNotificationReadParams{ID: id, UserID: p.User.ID})
	if err == nil && n == 0 {
		return apperr.NotFound("notification not found")
	}
	return err
}

func (s *Service) MarkAllNotificationsRead(ctx context.Context, p *Principal) error {
	return s.q.MarkAllNotificationsRead(ctx, p.User.ID)
}

func (s *Service) DeleteNotification(ctx context.Context, p *Principal, id uuid.UUID) error {
	n, err := s.q.DeleteNotification(ctx, store.DeleteNotificationParams{ID: id, UserID: p.User.ID})
	if err == nil && n == 0 {
		return apperr.NotFound("notification not found")
	}
	return err
}

// usersByID loads live users keyed by ID; deleted accounts are omitted.
func (s *Service) usersByID(ctx context.Context, q *store.Queries, ids []uuid.UUID) (map[uuid.UUID]store.User, error) {
	out := map[uuid.UUID]store.User{}
	if len(ids) == 0 {
		return out, nil
	}
	users, err := q.ListUsersByIDs(ctx, dedupe(ids))
	if err != nil {
		return nil, err
	}
	for _, u := range users {
		out[u.ID] = u
	}
	return out, nil
}

func dedupe(ids []uuid.UUID) []uuid.UUID {
	seen := make(map[uuid.UUID]bool, len(ids))
	out := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

func validateSubscriptionLevel(level string) error {
	switch level {
	case "watching", "normal", "muted":
		return nil
	}
	return apperr.Invalid("level must be one of watching, normal, muted")
}

func (s *Service) setSubscription(ctx context.Context, q *store.Queries, p *Principal, placeID uuid.UUID, targetType string, targetID uuid.UUID, level string) error {
	if level == "normal" {
		return q.DeleteSubscription(ctx, store.DeleteSubscriptionParams{UserID: p.User.ID, TargetType: targetType, TargetID: targetID})
	}
	return q.UpsertSubscription(ctx, store.UpsertSubscriptionParams{
		UserID: p.User.ID, TargetType: targetType, TargetID: targetID, PlaceID: placeID, Level: level,
	})
}

// SetPlaceSubscription sets the caller's notification level for a whole place: watching
// notifies about every new topic; muted silences the place except for more specific
// watches.
func (s *Service) SetPlaceSubscription(ctx context.Context, p *Principal, ref, level string) error {
	if err := validateSubscriptionLevel(level); err != nil {
		return err
	}
	place, _, err := s.requireMember(ctx, s.q, p, ref)
	if err != nil {
		return err
	}
	return s.setSubscription(ctx, s.q, p, place.ID, "place", place.ID, level)
}

// SetBoardSubscription: watching notifies about new topics in the board and its children.
func (s *Service) SetBoardSubscription(ctx context.Context, p *Principal, boardID uuid.UUID, level string) error {
	if err := validateSubscriptionLevel(level); err != nil {
		return err
	}
	f, b, err := s.boardScope(ctx, s.q, p, boardID)
	if err != nil {
		return err
	}
	if !f.acc.IsMember {
		return apperr.Forbidden("you are not a member of this place")
	}
	return s.setSubscription(ctx, s.q, p, f.place.ID, "board", b.ID, level)
}

// SetTopicSubscription: watching notifies about every reply.
func (s *Service) SetTopicSubscription(ctx context.Context, p *Principal, topicID uuid.UUID, level string) error {
	if err := validateSubscriptionLevel(level); err != nil {
		return err
	}
	f, topic, err := s.topicScope(ctx, s.q, p, topicID)
	if err != nil {
		return err
	}
	if !f.acc.IsMember {
		return apperr.Forbidden("you are not a member of this place")
	}
	return s.setSubscription(ctx, s.q, p, f.place.ID, "topic", topic.ID, level)
}
