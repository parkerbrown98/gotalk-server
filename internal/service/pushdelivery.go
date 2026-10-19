package service

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/parkerbrown98/gotalk-server/internal/pushgw"
	"github.com/parkerbrown98/gotalk-server/internal/realtime"
	"github.com/parkerbrown98/gotalk-server/internal/store"
)

// Push delivery timing. Tests shorten them.
var (
	// PushPoll is how often idle replicas look for due pushes.
	PushPoll = 5 * time.Second
	// PushMaxAge is how old a notification may get before its push is dropped: a push
	// about something long past is noise.
	PushMaxAge = 6 * time.Hour
	// PushBackoff is the wait before each retry.
	PushBackoff = []time.Duration{10 * time.Second, 30 * time.Second, 2 * time.Minute, 5 * time.Minute, 15 * time.Minute}
)

const (
	// MaxPushAttempts bounds retries of one notification's push.
	MaxPushAttempts = 6
	pushBatch       = 50
	pushLock        = 2 * time.Minute
	maxPushTitle    = 120
	maxPushBody     = 400
)

// wakePush tells the delivery loop that pushes were queued.
func (s *Service) wakePush() {
	select {
	case s.pushWake <- struct{}{}:
	default:
	}
}

// queuePushes queues new notifications for people with push subscriptions, in the same
// transaction as the notifications.
func (s *Service) queuePushes(ctx context.Context, q *store.Queries, ns []store.Notification) error {
	if !s.PushEnabled() || len(ns) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, len(ns))
	for i, n := range ns {
		ids[i] = n.ID
	}
	n, err := q.EnqueuePushes(ctx, ids)
	if err != nil {
		return err
	}
	if n > 0 {
		s.afterCommit(ctx, q, func(context.Context) { s.wakePush() })
	}
	return nil
}

// RunPushDelivery pushes queued notifications until ctx is cancelled. Every replica runs
// it; row locks keep them from pushing a notification twice.
func (s *Service) RunPushDelivery(ctx context.Context) {
	poll := time.NewTicker(PushPoll)
	defer poll.Stop()
	for {
		for s.deliverPushBatch(ctx) == pushBatch && ctx.Err() == nil {
		}
		select {
		case <-ctx.Done():
			return
		case <-poll.C:
		case <-s.pushWake:
		}
	}
}

// pushJob is one claimed notification and what happened to its pushes.
type pushJob struct {
	row   store.PushOutbox
	retry []uuid.UUID
	// wait is the longest Retry-After the gateway asked for.
	wait time.Duration
	// drop ends the job without retrying (for example, a malformed request).
	drop bool
}

type pushEntry struct {
	job *pushJob
	sub store.PushSubscription
	req pushgw.Push
}

func (s *Service) deliverPushBatch(ctx context.Context) int {
	if !s.PushEnabled() {
		return 0
	}
	gw, err := s.q.GetPushGateway(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Warn("reading the push gateway registration", "error", err)
		}
		return 0
	}
	creds, registered, err := s.pushCredentials(ctx, gw)
	if err != nil || !registered || gw.Status != PushActive {
		// Pushes wait in the queue until the gateway verifies the instance (or they expire).
		return 0
	}
	claimed, err := s.q.ClaimPushOutbox(ctx, store.ClaimPushOutboxParams{LockSeconds: int32(pushLock / time.Second), Lim: pushBatch})
	if err != nil {
		if ctx.Err() == nil {
			s.log.Warn("claiming queued pushes", "error", err)
		}
		return 0
	}
	if len(claimed) == 0 {
		return 0
	}
	jobs := make([]*pushJob, len(claimed))
	users := make([]uuid.UUID, 0, len(claimed))
	for i, row := range claimed {
		jobs[i] = &pushJob{row: row}
		users = append(users, row.UserID)
	}
	dnd := s.inDoNotDisturb(ctx, users)
	entries := s.buildPushEntries(ctx, jobs, gw, creds, dnd)
	s.sendPushEntries(ctx, creds, entries)
	if ctx.Err() != nil {
		// Shutting down: the locks expire and another replica (or the next start) retries.
		return len(claimed)
	}
	for _, job := range jobs {
		s.finishPushJob(ctx, job)
	}
	return len(claimed)
}

// inDoNotDisturb returns the people whose presence is do-not-disturb.
func (s *Service) inDoNotDisturb(ctx context.Context, users []uuid.UUID) map[uuid.UUID]bool {
	src := s.presence.Load()
	if src == nil {
		return nil
	}
	statuses, err := src.p.Get(ctx, dedupe(users))
	if err != nil {
		s.log.Warn("reading presence for push delivery", "error", err)
		return nil
	}
	out := map[uuid.UUID]bool{}
	for id, st := range statuses {
		if st == realtime.StatusDND {
			out[id] = true
		}
	}
	return out
}

// buildPushEntries works out who gets what: notifications that were read, deleted or are
// too old, people who paused pushes, are in quiet hours or do-not-disturb, and categories
// they turned off produce no pushes.
func (s *Service) buildPushEntries(ctx context.Context, jobs []*pushJob, gw store.PushGateway, creds pushgw.Credentials, dnd map[uuid.UUID]bool) []pushEntry {
	now := time.Now()
	prefs := map[uuid.UUID]*store.PushPreference{}
	var entries []pushEntry
	for _, job := range jobs {
		n, err := s.q.GetNotification(ctx, job.row.NotificationID)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		} else if err != nil {
			s.log.Warn("loading a notification to push", "notification_id", job.row.NotificationID, "error", err)
			job.retryAll()
			continue
		}
		if n.ReadAt != nil || now.Sub(n.CreatedAt) > PushMaxAge || dnd[n.UserID] {
			continue
		}
		pref, ok := prefs[n.UserID]
		if !ok {
			row, err := s.q.GetPushPreferences(ctx, n.UserID)
			if err == nil {
				pref = &row
			}
			prefs[n.UserID] = pref
		}
		if pref != nil && (pushQuiet(*pref, now) || containsString(pref.DisabledCategories, pushCategory(n.Kind))) {
			continue
		}
		subs, err := s.q.ListDeliverableSubscriptions(ctx, store.ListDeliverableSubscriptionsParams{
			UserID: n.UserID, GatewayUrl: deref(gw.GatewayUrl), Only: job.row.Pending,
		})
		if err != nil {
			s.log.Warn("loading push subscriptions", "user_id", n.UserID, "error", err)
			job.retryAll()
			continue
		}
		if len(subs) == 0 {
			continue
		}
		msg, err := s.pushMessage(ctx, n, creds)
		if errors.Is(err, errPushTooLarge) {
			s.log.Warn("dropping a push that does not fit", "notification_id", n.ID)
			continue
		} else if err != nil {
			s.log.Warn("building a push; will retry", "notification_id", n.ID, "error", err)
			job.retryAll()
			continue
		}
		for _, sub := range subs {
			req := pushgw.Push{
				SubscriptionID: sub.SubscriptionID, Mode: sub.Mode, NotificationID: n.ID.String(),
				Urgency: msg.urgency, TTL: &msg.ttl, CollapseKey: msg.collapseKey,
			}
			if sub.Mode == pushgw.ModeEncrypted {
				body, err := pushgw.Encrypt(sub.P256dh, sub.Auth, msg.plaintext)
				if err != nil {
					s.log.Warn("encrypting a push", "subscription", sub.ID, "error", err)
					continue
				}
				req.Payload = base64.RawURLEncoding.EncodeToString(body)
			}
			entries = append(entries, pushEntry{job: job, sub: sub, req: req})
		}
	}
	return entries
}

// retryAll marks a job to be retried for every subscription it was going to push to:
// its pending subset, or uuid.Nil for all of the person's subscriptions.
func (j *pushJob) retryAll() {
	if j.row.Pending != nil {
		j.retry = append(j.retry, j.row.Pending...)
	} else {
		j.retry = append(j.retry, uuid.Nil)
	}
}

// sendPushEntries sends entries in batches and records each outcome on its job.
func (s *Service) sendPushEntries(ctx context.Context, creds pushgw.Credentials, entries []pushEntry) {
	client := s.pushClient()
	var (
		gone  = map[string]bool{}
		sent  []uuid.UUID
		reGW  bool
		block *pushgw.Error
	)
	for start := 0; start < len(entries); start += pushgw.MaxBatch {
		chunk := entries[start:min(start+pushgw.MaxBatch, len(entries))]
		reqs := make([]pushgw.Push, len(chunk))
		for i, e := range chunk {
			reqs[i] = e.req
		}
		results, err := client.PushBatch(ctx, creds, reqs)
		if err != nil {
			e, isGW := pushgw.AsError(err)
			switch {
			case ctx.Err() != nil:
				return
			case isGW && (e.Code == pushgw.CodeUnknownKey || e.Code == pushgw.CodeInvalidSignature || e.Code == pushgw.CodeInstanceNotFound):
				reGW = true
			case isGW && (e.Code == pushgw.CodeInstanceNotVerified || e.Code == pushgw.CodeInstanceBlocked):
				block = e
			case isGW && (e.Status == http.StatusBadRequest || e.Status == http.StatusUnprocessableEntity || e.Status == http.StatusRequestEntityTooLarge):
				s.log.Error("the push gateway refused a batch", "error", err)
				for _, en := range chunk {
					en.job.drop = true
				}
				continue
			}
			s.log.Warn("pushing to the gateway failed; will retry", "error", err)
			for _, en := range chunk {
				en.job.retry = append(en.job.retry, en.sub.ID)
				if isGW {
					en.job.wait = max(en.job.wait, e.RetryAfter)
				}
			}
			continue
		}
		for i, r := range results {
			en := chunk[i]
			if r.Status == "queued" {
				sent = append(sent, en.sub.ID)
				continue
			}
			code := ""
			if r.Error != nil {
				code = r.Error.Code
			}
			switch code {
			case pushgw.CodeSubscriptionGone, pushgw.CodeSubscriptionNotFound:
				gone[en.sub.SubscriptionID] = true
			case pushgw.CodeRateLimited, pushgw.CodeQuotaExceeded, pushgw.CodeProviderUnavailable, pushgw.CodeInternalError:
				en.job.retry = append(en.job.retry, en.sub.ID)
			default:
				s.log.Warn("the push gateway rejected a push", "subscription", en.sub.ID, "code", code)
			}
		}
	}
	if len(sent) > 0 {
		if err := s.q.TouchPushSubscriptions(ctx, sent); err != nil {
			s.log.Warn("recording pushes", "error", err)
		}
	}
	gw := s.PushGatewayURL()
	for sub := range gone {
		if _, err := s.q.DeletePushSubscriptionsByGatewayID(ctx, store.DeletePushSubscriptionsByGatewayIDParams{
			GatewayUrl: gw, SubscriptionID: sub,
		}); err != nil {
			s.log.Warn("deleting a dead push subscription", "subscription_id", sub, "error", err)
		}
	}
	if len(gone) > 0 {
		s.log.Info("removed push subscriptions the gateway reported dead", "count", len(gone))
	}
	if block != nil {
		status := PushSuspended
		if block.Code == pushgw.CodeInstanceBlocked {
			status = PushBlocked
		}
		if _, err := s.setPushStatus(ctx, nil, status, block.Detail); err != nil {
			s.log.Warn("recording the push gateway status", "error", err)
		}
		s.wakePushGateway()
	}
	if reGW {
		if _, err := s.setPushStatus(ctx, nil, PushError, "the gateway no longer accepts this instance's key; registering again"); err != nil {
			s.log.Warn("recording the push gateway status", "error", err)
		}
		s.wakePushGateway()
	}
}

// finishPushJob deletes a handled job, or schedules the subscriptions that failed
// transiently for another attempt.
func (s *Service) finishPushJob(ctx context.Context, job *pushJob) {
	row := job.row
	giveUp := job.drop || len(job.retry) == 0 || row.Attempts >= MaxPushAttempts || time.Since(row.CreatedAt) > PushMaxAge
	if giveUp {
		if len(job.retry) > 0 && !job.drop {
			s.log.Warn("giving up on a push", "notification_id", row.NotificationID, "attempts", row.Attempts)
		}
		if err := s.q.FinishPush(ctx, row.NotificationID); err != nil {
			s.log.Warn("finishing a push", "notification_id", row.NotificationID, "error", err)
		}
		return
	}
	var pending []uuid.UUID
	if !containsUUID(job.retry, uuid.Nil) {
		pending = dedupe(job.retry)
	}
	wait := max(job.wait, PushBackoff[min(int(row.Attempts), len(PushBackoff))-1])
	if err := s.q.RetryPush(ctx, store.RetryPushParams{
		NotificationID: row.NotificationID, NextAttemptAt: time.Now().Add(wait), Pending: pending,
	}); err != nil {
		s.log.Warn("scheduling a push retry", "notification_id", row.NotificationID, "error", err)
	}
}

func containsString(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

func containsUUID(list []uuid.UUID, v uuid.UUID) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// pushPayload is the plaintext a device decrypts (gateway docs/payload.md).
type pushPayload struct {
	V              int               `json:"v"`
	Instance       string            `json:"instance"`
	UserID         string            `json:"user_id"`
	NotificationID string            `json:"notification_id"`
	Type           string            `json:"type"`
	Title          string            `json:"title"`
	Body           string            `json:"body,omitempty"`
	ThreadID       string            `json:"thread_id,omitempty"`
	Category       string            `json:"category,omitempty"`
	Badge          *int64            `json:"badge,omitempty"`
	CreatedAt      time.Time         `json:"created_at"`
	Target         map[string]string `json:"target,omitempty"`
}

// pushMessage is what every subscription of one notification gets.
type pushMessage struct {
	plaintext   []byte
	urgency     string
	ttl         int
	collapseKey string
}

func (s *Service) pushMessage(ctx context.Context, n store.Notification, creds pushgw.Credentials) (pushMessage, error) {
	var actor *store.User
	if n.ActorID != nil {
		if u, err := s.q.GetUserByID(ctx, *n.ActorID); err == nil {
			actor = &u
		}
	}
	unread, err := s.q.CountUnreadNotifications(ctx, n.UserID)
	if err != nil {
		return pushMessage{}, err
	}
	p := buildPushPayload(n, actor, s.cfg.Server.PublicURL)
	p.Badge = &unread
	plaintext, err := fitPushPayload(p)
	if err != nil {
		return pushMessage{}, err
	}
	msg := pushMessage{plaintext: plaintext, urgency: pushUrgency(n.Kind), ttl: pushTTL(n.Kind)}
	if p.ThreadID != "" {
		msg.collapseKey = collapseKey(creds.Key.Seed(), p.ThreadID)
	}
	return msg, nil
}

// buildPushPayload renders a notification for a lock screen. Clients prefix the instance
// name, and only show what they render from these fields.
func buildPushPayload(n store.Notification, actor *store.User, instance string) pushPayload {
	var data map[string]any
	_ = json.Unmarshal(n.Data, &data)
	str := func(k string) string { v, _ := data[k].(string); return v }
	who := "Someone"
	if actor != nil {
		who = cmpOr(actor.DisplayName, actor.Username)
	}
	where := str("topic_title")
	if n.ChannelID != nil && str("channel_name") != "" && n.PlaceID != nil {
		where = "#" + str("channel_name")
	}
	in := ""
	if where != "" {
		in = " in " + where
	}
	p := pushPayload{
		V: 1, Instance: instance, UserID: n.UserID.String(), NotificationID: n.ID.String(), Type: n.Kind,
		Category: pushCategory(n.Kind), CreatedAt: n.CreatedAt.UTC(), Body: str("excerpt"), Target: map[string]string{},
	}
	switch n.Kind {
	case NotifyMention:
		p.Title = who + " mentioned you" + in
	case NotifyReply:
		p.Title = who + " replied to you" + in
	case NotifyTopicReply:
		p.Title = who + " replied" + in
	case NotifyNewTopic:
		p.Title = "New topic: " + str("topic_title")
		if pl := str("place_name"); pl != "" {
			p.Title = who + " started a topic in " + pl
			p.Body = strings.TrimSpace(str("topic_title") + "\n" + str("excerpt"))
		}
	case NotifyReaction:
		target := "your post"
		if n.MessageID != nil {
			target = "your message"
		}
		p.Title = strings.TrimSpace(who+" reacted "+str("emoji")) + " to " + target
	case NotifySolution:
		p.Title = who + " marked your answer as the solution"
		p.Body = str("topic_title")
	case NotifyModeration:
		p.Title = "Moderation notice"
		if pl := str("place_name"); pl != "" {
			p.Title += " from " + pl
		}
		p.Body = strings.TrimSpace(strings.ReplaceAll(str("action"), ".", " ") + ": " + str("reason"))
		p.Body = strings.TrimSuffix(p.Body, ":")
	case NotifyDirectMessage:
		p.Title = who
		if name := str("channel_name"); name != "" {
			p.Title = who + " in " + name
		}
	default:
		p.Title = "New notification"
	}
	set := func(k string, id *uuid.UUID) {
		if id != nil {
			p.Target[k] = id.String()
		}
	}
	set("place_id", n.PlaceID)
	set("topic_id", n.TopicID)
	set("post_id", n.PostID)
	set("channel_id", n.ChannelID)
	set("message_id", n.MessageID)
	if n.ChannelID != nil && n.PlaceID == nil {
		p.Target["conversation_id"] = n.ChannelID.String()
	}
	switch {
	case n.ChannelID != nil:
		p.ThreadID = n.ChannelID.String()
	case n.TopicID != nil:
		p.ThreadID = n.TopicID.String()
	}
	p.Title = clip(p.Title, maxPushTitle)
	p.Body = clip(p.Body, maxPushBody)
	return p
}

// fitPushPayload encodes p, shortening the body (then the title) until it fits the
// plaintext limit with room for padding.
func fitPushPayload(p pushPayload) ([]byte, error) {
	for {
		raw, err := json.Marshal(p)
		if err != nil {
			return nil, err
		}
		if len(raw) <= pushgw.MaxPlaintext {
			return raw, nil
		}
		over := len(raw) - pushgw.MaxPlaintext
		switch {
		case p.Body != "":
			p.Body = clipBytes(p.Body, len(p.Body)-over-8)
		case p.Title != "":
			p.Title = clipBytes(p.Title, len(p.Title)-over-8)
		default:
			return nil, errPushTooLarge
		}
	}
}

// errPushTooLarge means a payload cannot be shortened to fit; retrying will not help.
var errPushTooLarge = errors.New("the push payload does not fit")

// clip shortens s to at most n runes, ending with an ellipsis when cut.
func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}

// clipBytes shortens s to at most n bytes on a rune boundary, with an ellipsis.
func clipBytes(s string, n int) string {
	if n <= 3 {
		return ""
	}
	cut := n - 3
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// pushUrgency: high only for what a person expects right away.
func pushUrgency(kind string) string {
	switch kind {
	case NotifyMention, NotifyDirectMessage:
		return pushgw.UrgencyHigh
	case NotifyReaction, NotifyNewTopic, NotifySolution:
		return pushgw.UrgencyLow
	}
	return pushgw.UrgencyNormal
}

// pushTTL is how long the gateway keeps a push for an offline device.
func pushTTL(kind string) int {
	if pushUrgency(kind) == pushgw.UrgencyLow {
		return int((6 * time.Hour).Seconds())
	}
	return int((24 * time.Hour).Seconds())
}

// collapseKey groups pushes per conversation or topic, so a newer one replaces an
// undelivered older one. It is keyed with the instance's secret so providers cannot link it
// to public IDs.
func collapseKey(secret []byte, thread string) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte("gotalk-push-collapse:" + thread))
	return "t" + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))[:22]
}
