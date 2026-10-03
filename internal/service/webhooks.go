package service

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/parkerbrown98/gotalk-server/internal/apperr"
	"github.com/parkerbrown98/gotalk-server/internal/auth"
	"github.com/parkerbrown98/gotalk-server/internal/config"
	"github.com/parkerbrown98/gotalk-server/internal/permissions"
	"github.com/parkerbrown98/gotalk-server/internal/store"
)

// Webhook event types.
const (
	WebhookPing          = "ping"
	WebhookMemberJoin    = "member.join"
	WebhookMemberLeave   = "member.leave"
	WebhookTopicCreate   = "topic.create"
	WebhookPostCreate    = "post.create"
	WebhookMessageCreate = "message.create"
	WebhookMessageUpdate = "message.update"
	WebhookMessageDelete = "message.delete"
	WebhookModeration    = "moderation.action"
	WebhookReportCreate  = "report.create"
)

// webhookEvents maps each subscribable event to the extra permission needed to subscribe
// to it, beyond MANAGE_WEBHOOKS.
var webhookEvents = map[string]permissions.Permission{
	WebhookMemberJoin:    0,
	WebhookMemberLeave:   0,
	WebhookTopicCreate:   0,
	WebhookPostCreate:    0,
	WebhookMessageCreate: 0,
	WebhookMessageUpdate: 0,
	WebhookMessageDelete: 0,
	WebhookModeration:    permissions.ViewAuditLog,
	WebhookReportCreate:  permissions.ManageReports,
}

// WebhookEvents lists the subscribable events, sorted.
func WebhookEvents() []string {
	out := make([]string, 0, len(webhookEvents))
	for e := range webhookEvents {
		out = append(out, e)
	}
	slices.Sort(out)
	return out
}

// Webhook delivery headers.
const (
	HeaderWebhookID       = "Gotalk-Webhook-Id"
	HeaderDeliveryID      = "Gotalk-Delivery-Id"
	HeaderWebhookEvent    = "Gotalk-Event"
	HeaderDeliveryAttempt = "Gotalk-Delivery-Attempt"
	HeaderSignature       = "Gotalk-Signature"
)

const (
	MaxWebhooksPerPlace = 10
	maxWebhookNameLen   = 100
	maxWebhookURLLen    = 2048
	// MaxWebhookAttempts bounds retries of one delivery.
	MaxWebhookAttempts = 6
	// webhookDisableAfter consecutive failed deliveries disable a webhook.
	webhookDisableAfter = 10
	// webhookBatch deliveries are claimed and sent at once. Claiming no more than are sent
	// in parallel keeps every claimed row's lock alive until its attempt finishes.
	webhookBatch       = 8
	webhookPoll        = 2 * time.Second
	maxWebhookErrorLen = 500
)

// webhookBackoff is the wait before each retry.
var webhookBackoff = []time.Duration{30 * time.Second, 2 * time.Minute, 10 * time.Minute, time.Hour, 6 * time.Hour}

// Encoder converts service views to their public JSON form. The API layer installs it so
// webhooks carry the same shapes as the REST API.
type Encoder func(any) (json.RawMessage, error)

// SetEncoder installs the encoder for webhook payloads. Without one, no webhooks are
// queued, since internal structs must never leave the server.
func (s *Service) SetEncoder(e Encoder) {
	if e == nil {
		s.encoder.Store(nil)
		return
	}
	s.encoder.Store(&e)
}

func (s *Service) encode(v any) (json.RawMessage, bool, error) {
	e := s.encoder.Load()
	if e == nil {
		return nil, false, nil
	}
	raw, err := (*e)(v)
	return raw, true, err
}

// MemberEvent is the payload of member.join and member.leave.
type MemberEvent struct {
	PlaceID uuid.UUID
	User    store.User
}

// TopicEvent is the payload of topic.create and post.create.
type TopicEvent struct {
	Topic TopicView
	Post  PostView
}

// queueWebhook writes deliveries for the place's webhooks subscribed to event, in the
// same transaction as the change, so a delivery exists exactly when the change committed.
// data is only built when someone is subscribed.
func (s *Service) queueWebhook(ctx context.Context, q *store.Queries, placeID uuid.UUID, event string, data func() (any, error)) error {
	if s.encoder.Load() == nil {
		return nil
	}
	hooks, err := q.ListWebhooksForEvent(ctx, store.ListWebhooksForEventParams{PlaceID: placeID, Event: event})
	if err != nil || len(hooks) == 0 {
		return err
	}
	v, err := data()
	if err != nil {
		return err
	}
	raw, _, err := s.encode(v)
	if err != nil {
		s.log.Error("encoding webhook payload", "event", event, "error", err)
		return nil
	}
	for _, h := range hooks {
		if _, err := s.enqueueDelivery(ctx, q, h.ID, event, raw); err != nil {
			return err
		}
	}
	s.afterCommit(ctx, q, func(context.Context) { s.wakeWebhooks() })
	return nil
}

func (s *Service) enqueueDelivery(ctx context.Context, q *store.Queries, webhookID uuid.UUID, event string, payload []byte) (store.WebhookDelivery, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return store.WebhookDelivery{}, err
	}
	return q.CreateWebhookDelivery(ctx, store.CreateWebhookDeliveryParams{ID: id, WebhookID: webhookID, Event: event, Payload: payload})
}

func (s *Service) wakeWebhooks() {
	select {
	case s.webhookWake <- struct{}{}:
	default:
	}
}

// queueMemberWebhook announces a member joining or leaving.
func (s *Service) queueMemberWebhook(ctx context.Context, q *store.Queries, placeID, userID uuid.UUID, event string) error {
	return s.queueWebhook(ctx, q, placeID, event, func() (any, error) {
		u, err := q.GetUserByID(ctx, userID)
		return MemberEvent{PlaceID: placeID, User: u}, err
	})
}

// queueMessageWebhook announces a message event in a channel @everyone can see. Messages
// in staff-only channels and direct messages never reach webhooks.
func (s *Service) queueMessageWebhook(ctx context.Context, q *store.Queries, cc *channelCtx, event string, data any) error {
	if cc.isDM() || !cc.scope.visibleTo(cc.scope.everyone(), cc.permID) {
		return nil
	}
	if mv, ok := data.(MessageView); ok {
		mv.Nonce = ""
		data = mv
	}
	return s.queueWebhook(ctx, q, cc.scope.place.ID, event, func() (any, error) { return data, nil })
}

// queueForumWebhook announces a new topic or reply on a board @everyone can see.
func (s *Service) queueForumWebhook(ctx context.Context, q *store.Queries, f *forumScope, event string, topic store.Topic, post store.Post) error {
	if !f.visibleTo(f.everyone(), topic.BoardID) {
		return nil
	}
	return s.queueWebhook(ctx, q, f.place.ID, event, func() (any, error) {
		tv, err := s.topicView(ctx, q, nil, topic)
		if err != nil {
			return nil, err
		}
		pv, err := s.postView(ctx, q, nil, f, post)
		return TopicEvent{Topic: tv, Post: pv}, err
	})
}

type WebhookInput struct {
	Name   string
	URL    string
	Events []string
	Active bool
}

type WebhookUpdate struct {
	Name   *string
	URL    *string
	Events *[]string
	Active *bool
}

func normalizeWebhookName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > maxWebhookNameLen {
		return "", apperr.Invalid("name must be 1-%d characters", maxWebhookNameLen)
	}
	return name, nil
}

// validateWebhookURL accepts absolute http(s) URLs without credentials. Unless private
// networks are allowed, literal private addresses and localhost are refused up front; the
// delivery dialer re-checks every resolved address, which also defeats DNS tricks.
func (s *Service) validateWebhookURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || len(raw) > maxWebhookURLLen {
		return "", apperr.Invalid("url must be an absolute http(s) URL without credentials, at most %d characters", maxWebhookURLLen)
	}
	if !s.cfg.Webhooks.AllowPrivateNetworks {
		host := strings.ToLower(u.Hostname())
		if host == "localhost" || strings.HasSuffix(host, ".localhost") {
			return "", apperr.Invalid("url must not point to this server's network")
		}
		if ip, err := netip.ParseAddr(host); err == nil && !publicAddress(ip) {
			return "", apperr.Invalid("url must not point to a private, loopback or link-local address")
		}
	}
	u.Fragment = ""
	return u.String(), nil
}

// normalizeWebhookEvents validates events and checks the caller may subscribe to them.
func normalizeWebhookEvents(events []string, m permissions.Member) ([]string, error) {
	if len(events) == 0 {
		return nil, apperr.Invalid("subscribe to at least one event")
	}
	out := []string{}
	for _, e := range events {
		perm, ok := webhookEvents[e]
		if !ok {
			return nil, apperr.Invalid("unknown event %q; events are %s", e, strings.Join(WebhookEvents(), ", "))
		}
		if perm != 0 && !m.Has(perm) {
			return nil, apperr.Forbidden("subscribing to %s requires %s", e, strings.Join(permissions.Names(perm), ", "))
		}
		if !slices.Contains(out, e) {
			out = append(out, e)
		}
	}
	slices.Sort(out)
	return out, nil
}

func newWebhookSecret() (string, error) {
	secret, err := auth.RandomString(40, auth.Base62)
	return "whsec_" + secret, err
}

// CreateWebhook adds an outgoing webhook to a place (MANAGE_WEBHOOKS). The response is the
// only time the secret is shown, apart from rotating it.
func (s *Service) CreateWebhook(ctx context.Context, p *Principal, ref string, in WebhookInput) (store.Webhook, error) {
	name, err := normalizeWebhookName(in.Name)
	if err != nil {
		return store.Webhook{}, err
	}
	target, err := s.validateWebhookURL(in.URL)
	if err != nil {
		return store.Webhook{}, err
	}
	var hook store.Webhook
	err = s.tx(ctx, func(q *store.Queries) error {
		place, acc, err := s.requirePermission(ctx, q, p, ref, permissions.ManageWebhooks)
		if err != nil {
			return err
		}
		events, err := normalizeWebhookEvents(in.Events, acc.Member)
		if err != nil {
			return err
		}
		if _, err := q.LockPlace(ctx, place.ID); err != nil {
			return err
		}
		n, err := q.CountWebhooks(ctx, place.ID)
		if err != nil {
			return err
		}
		if n >= MaxWebhooksPerPlace {
			return apperr.Conflict("a place can have at most %d webhooks", MaxWebhooksPerPlace)
		}
		secret, err := newWebhookSecret()
		if err != nil {
			return err
		}
		id, err := uuid.NewV7()
		if err != nil {
			return err
		}
		hook, err = q.CreateWebhook(ctx, store.CreateWebhookParams{
			ID: id, PlaceID: place.ID, Name: name, Url: target, Secret: secret, Events: events,
			IsActive: in.Active, CreatedBy: &p.User.ID,
		})
		if err != nil {
			return err
		}
		return s.audit(ctx, q, place.ID, p, "webhook.create", "webhook", &hook.ID, "",
			map[string]any{"name": name, "url": target, "events": events})
	})
	return hook, err
}

// ListWebhooks lists the place's webhooks the caller may manage (see webhookFor).
func (s *Service) ListWebhooks(ctx context.Context, p *Principal, ref string) ([]store.Webhook, error) {
	place, acc, err := s.requirePermission(ctx, s.q, p, ref, permissions.ManageWebhooks)
	if err != nil {
		return nil, err
	}
	hooks, err := s.q.ListWebhooks(ctx, place.ID)
	if err != nil {
		return nil, err
	}
	return slicesFilter(hooks, func(h store.Webhook) bool { return acc.Member.Has(webhookPerms(h.Events)) }), nil
}

// webhookPerms is what a webhook's events require beyond MANAGE_WEBHOOKS.
func webhookPerms(events []string) permissions.Permission {
	var need permissions.Permission
	for _, e := range events {
		need |= webhookEvents[e]
	}
	return need
}

// webhookFor loads a webhook the caller may manage: MANAGE_WEBHOOKS plus every extra
// permission its events need, so the payloads and URL of a webhook that receives reports or
// audit entries stay with those who could subscribe to them.
func (s *Service) webhookFor(ctx context.Context, q *store.Queries, p *Principal, id uuid.UUID) (store.Webhook, Access, error) {
	hook, err := q.GetWebhook(ctx, id)
	if err != nil {
		return hook, Access{}, notFound(err, "webhook not found")
	}
	_, acc, err := s.requirePermission(ctx, q, p, hook.PlaceID.String(), permissions.ManageWebhooks)
	if err != nil {
		// Only members may learn that a webhook exists.
		if !acc.IsMember {
			return hook, acc, apperr.NotFound("webhook not found")
		}
		return hook, acc, err
	}
	if need := webhookPerms(hook.Events); !acc.Member.Has(need) {
		return hook, acc, apperr.Forbidden("managing this webhook requires %s because of the events it receives",
			strings.Join(permissions.Names(need), ", "))
	}
	return hook, acc, nil
}

func (s *Service) GetWebhook(ctx context.Context, p *Principal, id uuid.UUID) (store.Webhook, error) {
	hook, _, err := s.webhookFor(ctx, s.q, p, id)
	return hook, err
}

func (s *Service) UpdateWebhook(ctx context.Context, p *Principal, id uuid.UUID, in WebhookUpdate) (store.Webhook, error) {
	params := store.UpdateWebhookParams{ID: id, IsActive: in.Active}
	meta := map[string]any{}
	if in.Name != nil {
		name, err := normalizeWebhookName(*in.Name)
		if err != nil {
			return store.Webhook{}, err
		}
		params.Name = &name
		meta["name"] = name
	}
	if in.URL != nil {
		target, err := s.validateWebhookURL(*in.URL)
		if err != nil {
			return store.Webhook{}, err
		}
		params.Url = &target
		meta["url"] = target
	}
	setIf(meta, "active", in.Active)
	var hook store.Webhook
	err := s.tx(ctx, func(q *store.Queries) error {
		_, acc, err := s.webhookFor(ctx, q, p, id)
		if err != nil {
			return err
		}
		if in.Events != nil {
			events, err := normalizeWebhookEvents(*in.Events, acc.Member)
			if err != nil {
				return err
			}
			params.Events = events
			meta["events"] = events
		}
		if hook, err = q.UpdateWebhook(ctx, params); err != nil {
			return notFound(err, "webhook not found")
		}
		if in.Active != nil && !*in.Active {
			if err := q.CancelPendingDeliveries(ctx, store.CancelPendingDeliveriesParams{WebhookID: id, Reason: "webhook disabled"}); err != nil {
				return err
			}
		}
		return s.audit(ctx, q, hook.PlaceID, p, "webhook.update", "webhook", &hook.ID, "", meta)
	})
	return hook, err
}

func (s *Service) DeleteWebhook(ctx context.Context, p *Principal, id uuid.UUID) error {
	return s.tx(ctx, func(q *store.Queries) error {
		hook, _, err := s.webhookFor(ctx, q, p, id)
		if err != nil {
			return err
		}
		if err := q.DeleteWebhook(ctx, hook.ID); err != nil {
			return err
		}
		return s.audit(ctx, q, hook.PlaceID, p, "webhook.delete", "webhook", &hook.ID, "", map[string]any{"name": hook.Name})
	})
}

// RotateWebhookSecret replaces the signing secret. Deliveries already in flight are signed
// with the new secret from their next attempt on.
func (s *Service) RotateWebhookSecret(ctx context.Context, p *Principal, id uuid.UUID) (store.Webhook, error) {
	var hook store.Webhook
	err := s.tx(ctx, func(q *store.Queries) error {
		if _, _, err := s.webhookFor(ctx, q, p, id); err != nil {
			return err
		}
		secret, err := newWebhookSecret()
		if err != nil {
			return err
		}
		if hook, err = q.RotateWebhookSecret(ctx, store.RotateWebhookSecretParams{ID: id, Secret: secret}); err != nil {
			return err
		}
		return s.audit(ctx, q, hook.PlaceID, p, "webhook.rotate_secret", "webhook", &hook.ID, "", map[string]any{"name": hook.Name})
	})
	return hook, err
}

// PingWebhook queues a ping delivery so managers can check their endpoint.
func (s *Service) PingWebhook(ctx context.Context, p *Principal, id uuid.UUID) (store.WebhookDelivery, error) {
	hook, _, err := s.webhookFor(ctx, s.q, p, id)
	if err != nil {
		return store.WebhookDelivery{}, err
	}
	if !hook.IsActive {
		return store.WebhookDelivery{}, apperr.Conflict("the webhook is disabled; enable it first")
	}
	raw, ok, err := s.encode(map[string]any{"webhook_id": hook.ID, "place_id": hook.PlaceID, "name": hook.Name})
	if err != nil || !ok {
		return store.WebhookDelivery{}, apperr.Unavailable("webhook delivery is not available")
	}
	d, err := s.enqueueDelivery(ctx, s.q, hook.ID, WebhookPing, raw)
	if err != nil {
		return d, err
	}
	s.wakeWebhooks()
	return d, nil
}

func (s *Service) ListWebhookDeliveries(ctx context.Context, p *Principal, id uuid.UUID, status string, page Pagination) ([]store.WebhookDelivery, error) {
	var st *string
	if status != "" {
		if status != "pending" && status != "succeeded" && status != "failed" {
			return nil, apperr.Invalid("status must be one of pending, succeeded, failed")
		}
		st = &status
	}
	hook, _, err := s.webhookFor(ctx, s.q, p, id)
	if err != nil {
		return nil, err
	}
	page = page.normalized()
	return s.q.ListWebhookDeliveries(ctx, store.ListWebhookDeliveriesParams{WebhookID: hook.ID, Status: st, Lim: page.Limit, Off: page.Offset})
}

// RedeliverWebhook queues a new delivery with the same event and payload as an earlier one.
func (s *Service) RedeliverWebhook(ctx context.Context, p *Principal, id, deliveryID uuid.UUID) (store.WebhookDelivery, error) {
	hook, _, err := s.webhookFor(ctx, s.q, p, id)
	if err != nil {
		return store.WebhookDelivery{}, err
	}
	if !hook.IsActive {
		return store.WebhookDelivery{}, apperr.Conflict("the webhook is disabled; enable it first")
	}
	prev, err := s.q.GetWebhookDelivery(ctx, store.GetWebhookDeliveryParams{ID: deliveryID, WebhookID: hook.ID})
	if err != nil {
		return store.WebhookDelivery{}, notFound(err, "delivery not found")
	}
	d, err := s.enqueueDelivery(ctx, s.q, hook.ID, prev.Event, prev.Payload)
	if err != nil {
		return d, err
	}
	s.wakeWebhooks()
	return d, nil
}

// RunWebhookDelivery delivers queued webhooks until ctx is cancelled. Every replica runs
// it; row locks keep them from delivering the same attempt twice.
func (s *Service) RunWebhookDelivery(ctx context.Context) {
	client := newWebhookClient(s.cfg.Webhooks)
	poll := time.NewTicker(webhookPoll)
	defer poll.Stop()
	prune := time.NewTicker(time.Hour)
	defer prune.Stop()
	for {
		for s.deliverWebhookBatch(ctx, client) == webhookBatch && ctx.Err() == nil {
		}
		select {
		case <-ctx.Done():
			return
		case <-poll.C:
		case <-s.webhookWake:
		case <-prune.C:
			before := time.Now().Add(-s.cfg.Webhooks.DeliveryRetention)
			if _, err := s.q.DeleteOldWebhookDeliveries(ctx, &before); err != nil && ctx.Err() == nil {
				s.log.Warn("pruning webhook deliveries", "error", err)
			}
		}
	}
}

func (s *Service) deliverWebhookBatch(ctx context.Context, client *http.Client) int {
	// Each attempt is bounded by the client timeout; the margin covers the database work.
	lock := s.cfg.Webhooks.Timeout + 30*time.Second
	claimed, err := s.q.ClaimWebhookDeliveries(ctx, store.ClaimWebhookDeliveriesParams{
		LockSeconds: int32(lock / time.Second), //nolint:gosec // config caps the timeout at one minute
		Lim:         webhookBatch,
	})
	if err != nil {
		if ctx.Err() == nil {
			s.log.Warn("claiming webhook deliveries", "error", err)
		}
		return 0
	}
	var wg sync.WaitGroup
	for _, d := range claimed {
		wg.Go(func() { s.attemptDelivery(ctx, client, d) })
	}
	wg.Wait()
	return len(claimed)
}

type webhookEnvelope struct {
	ID        uuid.UUID       `json:"id"`
	Type      string          `json:"type"`
	WebhookID uuid.UUID       `json:"webhook_id"`
	PlaceID   uuid.UUID       `json:"place_id"`
	CreatedAt time.Time       `json:"created_at"`
	Data      json.RawMessage `json:"data"`
}

// attemptDelivery sends one claimed delivery and records the outcome.
func (s *Service) attemptDelivery(ctx context.Context, client *http.Client, d store.WebhookDelivery) {
	hook, err := s.q.GetWebhook(ctx, d.WebhookID)
	if errors.Is(err, pgx.ErrNoRows) {
		return
	}
	if err != nil {
		s.log.Warn("loading webhook for delivery", "delivery", d.ID, "error", err)
		return
	}
	body, err := json.Marshal(webhookEnvelope{
		ID: d.ID, Type: d.Event, WebhookID: hook.ID, PlaceID: hook.PlaceID, CreatedAt: d.CreatedAt.UTC(), Data: d.Payload,
	})
	if err != nil {
		s.log.Error("encoding webhook delivery", "delivery", d.ID, "error", err)
		return
	}
	start := time.Now()
	status, sendErr := sendWebhook(ctx, client, hook, d, body)
	if ctx.Err() != nil {
		// Shutting down: the lock expires and another replica (or the next start) retries.
		return
	}
	elapsed := int32(time.Since(start).Milliseconds()) //nolint:gosec // bounded by the client timeout (at most a minute)
	params := store.FinishWebhookAttemptParams{
		ID: d.ID, ResponseStatus: status, DurationMs: &elapsed, NextAttemptAt: d.NextAttemptAt, Status: "succeeded",
	}
	if sendErr != nil {
		params.LastError = truncate(sendErr.Error(), maxWebhookErrorLen)
		if d.Attempts >= MaxWebhookAttempts {
			params.Status = "failed"
		} else {
			params.Status = "pending"
			params.NextAttemptAt = time.Now().Add(webhookBackoff[min(int(d.Attempts), len(webhookBackoff))-1])
		}
	}
	err = s.tx(ctx, func(q *store.Queries) error {
		if _, err := q.FinishWebhookAttempt(ctx, params); err != nil {
			return err
		}
		switch params.Status {
		case "succeeded":
			return q.RecordWebhookSuccess(ctx, hook.ID)
		case "pending":
			return q.RecordWebhookAttemptFailure(ctx, hook.ID)
		}
		failures, err := q.RecordWebhookFailure(ctx, hook.ID)
		if err != nil || failures < webhookDisableAfter {
			return err
		}
		return s.disableWebhook(ctx, q, hook,
			fmt.Sprintf("disabled after %d deliveries in a row failed every attempt", failures))
	})
	if err != nil && ctx.Err() == nil {
		s.log.Warn("recording webhook delivery", "delivery", d.ID, "error", err)
	}
}

// disableWebhook turns a failing webhook off, drops its queue and records why.
func (s *Service) disableWebhook(ctx context.Context, q *store.Queries, hook store.Webhook, reason string) error {
	n, err := q.DisableWebhook(ctx, store.DisableWebhookParams{ID: hook.ID, DisabledReason: reason})
	if err != nil || n == 0 {
		return err
	}
	if err := q.CancelPendingDeliveries(ctx, store.CancelPendingDeliveriesParams{WebhookID: hook.ID, Reason: "webhook disabled"}); err != nil {
		return err
	}
	s.log.Warn("webhook disabled", "webhook", hook.ID, "place", hook.PlaceID, "reason", reason)
	return s.audit(ctx, q, hook.PlaceID, nil, "webhook.disable", "webhook", &hook.ID, reason, map[string]any{"name": hook.Name})
}

// SignWebhook returns the Gotalk-Signature header for a body sent at ts: the hex
// HMAC-SHA256 of "<ts>.<body>" keyed with the webhook secret.
func SignWebhook(secret string, ts int64, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(ts, 10) + "."))
	mac.Write(body)
	return "t=" + strconv.FormatInt(ts, 10) + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

func sendWebhook(ctx context.Context, client *http.Client, hook store.Webhook, d store.WebhookDelivery, body []byte) (*int32, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, hook.Url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	h := req.Header
	h.Set("Content-Type", "application/json")
	h.Set("User-Agent", "Gotalk-Webhook/1.0")
	h.Set(HeaderWebhookID, hook.ID.String())
	h.Set(HeaderDeliveryID, d.ID.String())
	h.Set(HeaderWebhookEvent, d.Event)
	h.Set(HeaderDeliveryAttempt, strconv.Itoa(int(d.Attempts)))
	h.Set(HeaderSignature, SignWebhook(hook.Secret, time.Now().Unix(), body))
	resp, err := client.Do(req)
	if err != nil {
		var blocked *blockedAddressError
		if errors.As(err, &blocked) {
			return nil, blocked
		}
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	code := int32(resp.StatusCode) //nolint:gosec // HTTP status codes are three digits
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &code, fmt.Errorf("endpoint responded with HTTP %d", resp.StatusCode)
	}
	return &code, nil
}

type blockedAddressError struct{ addr string }

func (e *blockedAddressError) Error() string {
	return "refused to connect to non-public address " + e.addr
}

// nonPublicPrefixes are special-purpose ranges that netip's predicates do not cover.
var nonPublicPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
}

// publicAddress reports whether ip is a globally routable unicast address, so webhooks
// cannot reach the server's own network or cloud metadata endpoints.
func publicAddress(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	for _, p := range nonPublicPrefixes {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}

// newWebhookClient builds the delivery client. It ignores proxy settings, does not follow
// redirects, and (unless private networks are allowed) refuses to connect to non-public
// addresses after DNS resolution.
func newWebhookClient(cfg config.Webhooks) *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	if !cfg.AllowPrivateNetworks {
		dialer.Control = func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip, err := netip.ParseAddr(host)
			if err != nil || !publicAddress(ip) {
				return &blockedAddressError{addr: host}
			}
			return nil
		}
	}
	transport := &http.Transport{
		DialContext:            dialer.DialContext,
		ForceAttemptHTTP2:      true,
		MaxIdleConns:           50,
		MaxIdleConnsPerHost:    4,
		IdleConnTimeout:        90 * time.Second,
		TLSHandshakeTimeout:    10 * time.Second,
		ResponseHeaderTimeout:  cfg.Timeout,
		MaxResponseHeaderBytes: 64 << 10,
	}
	return &http.Client{
		Transport:     transport,
		Timeout:       cfg.Timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}
