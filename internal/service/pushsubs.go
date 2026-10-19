package service

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/parkerbrown98/gotalk-server/internal/apperr"
	"github.com/parkerbrown98/gotalk-server/internal/pushgw"
	"github.com/parkerbrown98/gotalk-server/internal/store"
)

// MaxPushSubscriptions bounds one person's devices.
const MaxPushSubscriptions = 25

// Push platforms.
var PushPlatforms = []string{"ios", "android", "web"}

// PushCategories are the notification groups a person can turn pushes off for. Every
// notification type belongs to one.
var PushCategories = []string{"mentions", "replies", "direct_messages", "reactions", "new_topics", "solutions", "moderation"}

// pushCategory maps a notification type to its category.
func pushCategory(kind string) string {
	switch kind {
	case NotifyMention:
		return "mentions"
	case NotifyReply, NotifyTopicReply:
		return "replies"
	case NotifyDirectMessage:
		return "direct_messages"
	case NotifyReaction:
		return "reactions"
	case NotifyNewTopic:
		return "new_topics"
	case NotifySolution:
		return "solutions"
	case NotifyModeration:
		return "moderation"
	}
	return kind
}

var appIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// PushSubscriptionInput registers a device's gateway subscription for the caller.
type PushSubscriptionInput struct {
	// GatewayURL is the gateway the device registered with; empty means this instance's.
	GatewayURL     string
	SubscriptionID string
	AppID          string
	Platform       string
	P256dh         []byte
	Auth           []byte
	Mode           string
	DeviceName     string
}

func (in *PushSubscriptionInput) validate(gateway string) error {
	if in.GatewayURL != "" && NormalizeGatewayURL(in.GatewayURL) != gateway {
		return apperr.Invalid("this instance pushes through %s; register the device with that gateway", gateway)
	}
	if !validGatewaySubscriptionID(in.SubscriptionID) {
		return apperr.Invalid("subscription_id must be the gateway's sub_… ID")
	}
	if !appIDPattern.MatchString(in.AppID) {
		return apperr.Invalid("app_id must be the gateway's app ID, such as gotalk")
	}
	if !slices.Contains(PushPlatforms, in.Platform) {
		return apperr.Invalid("platform must be one of ios, android, web")
	}
	if err := pushgw.ValidateDeviceKeys(in.P256dh, in.Auth); err != nil {
		return apperr.Invalid("keys: %s", err.Error())
	}
	switch in.Mode {
	case "":
		in.Mode = pushgw.ModeEncrypted
	case pushgw.ModeEncrypted, pushgw.ModeIDOnly:
	default:
		return apperr.Invalid("mode must be encrypted or id_only")
	}
	if in.Mode == pushgw.ModeIDOnly && in.Platform == "web" {
		return apperr.Invalid("web subscriptions use encrypted mode; browsers decrypt every push")
	}
	if utf8.RuneCountInString(in.DeviceName) > 100 {
		return apperr.Invalid("device_name must be at most 100 characters")
	}
	return nil
}

// AddPushSubscription stores (or updates) a device's subscription for the caller's account.
// It belongs to the caller's login session: signing that session out stops the pushes.
// Registering the same gateway subscription again replaces its keys.
func (s *Service) AddPushSubscription(ctx context.Context, p *Principal, in PushSubscriptionInput) (store.PushSubscription, error) {
	if p.Token != nil {
		return store.PushSubscription{}, apperr.Forbidden("push subscriptions belong to a login session, not an API token")
	}
	if !s.PushEnabled() {
		return store.PushSubscription{}, apperr.Conflict("push notifications are not enabled on this instance")
	}
	gateway := s.PushGatewayURL()
	if err := in.validate(gateway); err != nil {
		return store.PushSubscription{}, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return store.PushSubscription{}, err
	}
	var sub store.PushSubscription
	err = s.tx(ctx, func(q *store.Queries) error {
		var err error
		sub, err = q.UpsertPushSubscription(ctx, store.UpsertPushSubscriptionParams{
			ID: id, UserID: p.User.ID, SessionID: p.SessionID, GatewayUrl: gateway, SubscriptionID: in.SubscriptionID,
			AppID: in.AppID, Platform: in.Platform, P256dh: in.P256dh, Auth: in.Auth, Mode: in.Mode, DeviceName: in.DeviceName,
		})
		if err != nil {
			return err
		}
		n, err := q.CountPushSubscriptions(ctx, p.User.ID)
		if err != nil {
			return err
		}
		if n > MaxPushSubscriptions {
			return apperr.Conflict("you have %d push subscriptions, the most an account may have; remove one first", MaxPushSubscriptions)
		}
		return nil
	})
	return sub, err
}

// ListPushSubscriptions lists the caller's devices.
func (s *Service) ListPushSubscriptions(ctx context.Context, p *Principal) ([]store.PushSubscription, error) {
	return s.q.ListPushSubscriptions(ctx, p.User.ID)
}

// DeletePushSubscription removes one of the caller's devices. When no other account on
// this instance uses the device's gateway subscription, it is deleted at the gateway too.
func (s *Service) DeletePushSubscription(ctx context.Context, p *Principal, id uuid.UUID) error {
	sub, err := s.q.DeletePushSubscription(ctx, store.DeletePushSubscriptionParams{ID: id, UserID: p.User.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		return apperr.NotFound("push subscription not found")
	}
	if err != nil {
		return err
	}
	s.gatewayDeleteDevice(ctx, sub.GatewayUrl, sub.SubscriptionID)
	return nil
}

// pruneSignedOutPushSubscriptions drops subscriptions of ended sessions and deletes their
// gateway subscriptions when nothing else uses them.
func (s *Service) pruneSignedOutPushSubscriptions(ctx context.Context) error {
	rows, err := s.q.DeleteSignedOutPushSubscriptions(ctx)
	if err != nil {
		return err
	}
	seen := map[[2]string]bool{}
	for _, r := range rows {
		k := [2]string{r.GatewayUrl, r.SubscriptionID}
		if !seen[k] {
			seen[k] = true
			s.gatewayDeleteDevice(ctx, r.GatewayUrl, r.SubscriptionID)
		}
	}
	return nil
}

// QuietHours hold back pushes daily between Start and End (HH:MM) in Timezone. A window
// whose start is after its end runs past midnight.
type QuietHours struct {
	Start    string
	End      string
	Timezone string
}

// PushPreferences decide which notifications also become pushes.
type PushPreferences struct {
	// Categories maps every category in PushCategories to whether it is pushed.
	Categories  map[string]bool
	QuietHours  *QuietHours
	PausedUntil *time.Time
}

func preferencesOf(row store.PushPreference) PushPreferences {
	out := PushPreferences{Categories: map[string]bool{}}
	for _, c := range PushCategories {
		out.Categories[c] = !slices.Contains(row.DisabledCategories, c)
	}
	if row.QuietHoursStart != nil && row.QuietHoursEnd != nil {
		out.QuietHours = &QuietHours{Start: clock(*row.QuietHoursStart), End: clock(*row.QuietHoursEnd), Timezone: row.Timezone}
	}
	if row.PausedUntil != nil && row.PausedUntil.After(time.Now()) {
		out.PausedUntil = row.PausedUntil
	}
	return out
}

func clock(minutes int16) string { return fmt.Sprintf("%02d:%02d", minutes/60, minutes%60) }

func parseClock(field, v string) (int16, error) {
	t, err := time.Parse("15:04", v)
	if err != nil {
		return 0, apperr.Invalid("%s must be a time of day such as 22:00", field)
	}
	return int16(t.Hour()*60 + t.Minute()), nil //nolint:gosec // at most 1439
}

// GetPushPreferences returns the caller's push preferences; everything is pushed by default.
func (s *Service) GetPushPreferences(ctx context.Context, p *Principal) (PushPreferences, error) {
	row, err := s.q.GetPushPreferences(ctx, p.User.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		row = store.PushPreference{Timezone: "UTC"}
	} else if err != nil {
		return PushPreferences{}, err
	}
	return preferencesOf(row), nil
}

// SetPushPreferences replaces the caller's push preferences. Categories left out stay on.
func (s *Service) SetPushPreferences(ctx context.Context, p *Principal, in PushPreferences) (PushPreferences, error) {
	params := store.UpsertPushPreferencesParams{UserID: p.User.ID, DisabledCategories: []string{}, Timezone: "UTC", PausedUntil: in.PausedUntil}
	for c, on := range in.Categories {
		if !slices.Contains(PushCategories, c) {
			return PushPreferences{}, apperr.Invalid("unknown category %q; categories are %v", c, PushCategories)
		}
		if !on {
			params.DisabledCategories = append(params.DisabledCategories, c)
		}
	}
	slices.Sort(params.DisabledCategories)
	if q := in.QuietHours; q != nil {
		start, err := parseClock("quiet_hours.start", q.Start)
		if err != nil {
			return PushPreferences{}, err
		}
		end, err := parseClock("quiet_hours.end", q.End)
		if err != nil {
			return PushPreferences{}, err
		}
		if start == end {
			return PushPreferences{}, apperr.Invalid("quiet_hours.start and quiet_hours.end must differ")
		}
		if q.Timezone != "" {
			if _, err := time.LoadLocation(q.Timezone); err != nil {
				return PushPreferences{}, apperr.Invalid("quiet_hours.timezone must be an IANA time zone such as Europe/Berlin")
			}
			params.Timezone = q.Timezone
		}
		params.QuietHoursStart, params.QuietHoursEnd = &start, &end
	}
	if params.PausedUntil != nil && !params.PausedUntil.After(time.Now()) {
		params.PausedUntil = nil
	}
	row, err := s.q.UpsertPushPreferences(ctx, params)
	if err != nil {
		return PushPreferences{}, err
	}
	return preferencesOf(row), nil
}

// pushQuiet reports whether pushes are held back at now: paused, or within quiet hours.
func pushQuiet(row store.PushPreference, now time.Time) bool {
	if row.PausedUntil != nil && now.Before(*row.PausedUntil) {
		return true
	}
	if row.QuietHoursStart == nil || row.QuietHoursEnd == nil {
		return false
	}
	loc, err := time.LoadLocation(row.Timezone)
	if err != nil {
		loc = time.UTC
	}
	t := now.In(loc)
	m := int16(t.Hour()*60 + t.Minute()) //nolint:gosec // at most 1439
	start, end := *row.QuietHoursStart, *row.QuietHoursEnd
	if start < end {
		return m >= start && m < end
	}
	return m >= start || m < end
}
