package api

import (
	"context"
	"encoding/base64"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/parkerbrown98/gotalk-server/internal/apperr"
	"github.com/parkerbrown98/gotalk-server/internal/service"
	"github.com/parkerbrown98/gotalk-server/internal/store"
)

const tagPush = "Push notifications"

// PushKeys is a device's encryption key material for one account.
type PushKeys struct {
	P256dh string `json:"p256dh" minLength:"86" maxLength:"90" doc:"Uncompressed P-256 public key (65 bytes), base64url"`
	Auth   string `json:"auth" minLength:"21" maxLength:"24" doc:"16-byte auth secret, base64url"`
}

// PushSubscriptionRequest registers a device's gateway subscription for the caller.
type PushSubscriptionRequest struct {
	SubscriptionID string   `json:"subscription_id" pattern:"^sub_[A-Za-z0-9_-]{8,64}$" doc:"The subscription_id the gateway returned from POST /v1/devices"`
	GatewayURL     string   `json:"gateway_url,omitempty" maxLength:"2048" doc:"The gateway the device registered with; must be this instance's push.gateway_url when given"`
	AppID          string   `json:"app_id" pattern:"^[a-z0-9][a-z0-9-]{0,31}$" doc:"The gateway app the device registered for, such as gotalk"`
	Platform       string   `json:"platform" enum:"ios,android,web"`
	Keys           PushKeys `json:"keys"`
	Mode           string   `json:"mode,omitempty" enum:"encrypted,id_only" default:"encrypted" doc:"encrypted pushes carry the notification encrypted to keys; id_only pushes carry only its ID for the device to fetch (not for web)"`
	DeviceName     string   `json:"device_name,omitempty" maxLength:"100" doc:"Shown in the caller's device list, e.g. Pixel 9 or Firefox on Linux"`
}

// PushSubscription is one of the caller's devices. Keys are never returned.
type PushSubscription struct {
	ID             string     `json:"id" format:"uuid"`
	SubscriptionID string     `json:"subscription_id"`
	GatewayURL     string     `json:"gateway_url"`
	AppID          string     `json:"app_id"`
	Platform       string     `json:"platform" enum:"ios,android,web"`
	Mode           string     `json:"mode" enum:"encrypted,id_only"`
	DeviceName     string     `json:"device_name"`
	Current        bool       `json:"current" doc:"Added by the session making this request"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	LastPushAt     *time.Time `json:"last_push_at"`
}

func toPushSubscription(sub store.PushSubscription, current bool) PushSubscription {
	return PushSubscription{
		ID: sub.ID.String(), SubscriptionID: sub.SubscriptionID, GatewayURL: sub.GatewayUrl, AppID: sub.AppID,
		Platform: sub.Platform, Mode: sub.Mode, DeviceName: sub.DeviceName, Current: current,
		CreatedAt: sub.CreatedAt, UpdatedAt: sub.UpdatedAt, LastPushAt: sub.LastPushAt,
	}
}

type PushSubscriptionPath struct {
	ID string `path:"subscriptionID" format:"uuid"`
}

// QuietHoursSetting holds back pushes every day between start and end.
type QuietHoursSetting struct {
	Start    string `json:"start" pattern:"^([01][0-9]|2[0-3]):[0-5][0-9]$" doc:"HH:MM"`
	End      string `json:"end" pattern:"^([01][0-9]|2[0-3]):[0-5][0-9]$" doc:"HH:MM; earlier than start to run past midnight"`
	Timezone string `json:"timezone,omitempty" maxLength:"64" doc:"IANA time zone, e.g. Europe/Berlin (default UTC)"`
}

// PushPreferences decide which notifications also become pushes. Mutes on places, boards,
// topics, channels and conversations apply first: muted activity creates no notification.
type PushPreferences struct {
	Categories  map[string]bool    `json:"categories" doc:"Whether each category is pushed: mentions, replies, direct_messages, reactions, new_topics, solutions, moderation. Missing categories are on"`
	QuietHours  *QuietHoursSetting `json:"quiet_hours,omitempty" doc:"Hold back pushes daily in this window; omitted when there are none (leave it out to turn quiet hours off)"`
	PausedUntil *time.Time         `json:"paused_until" required:"false" nullable:"true" doc:"Hold back every push until then (do not disturb); null when not paused"`
}

func toPushPreferences(p service.PushPreferences) PushPreferences {
	out := PushPreferences{Categories: p.Categories, PausedUntil: p.PausedUntil}
	if q := p.QuietHours; q != nil {
		out.QuietHours = &QuietHoursSetting{Start: q.Start, End: q.End, Timezone: q.Timezone}
	}
	return out
}

// decodeKey accepts base64url (padded or not) and standard base64.
func decodeKey(field, v string) ([]byte, error) {
	v = strings.TrimRight(strings.NewReplacer("+", "-", "/", "_").Replace(v), "=")
	b, err := base64.RawURLEncoding.DecodeString(v)
	if err != nil {
		return nil, apperr.Invalid("keys.%s must be base64url", field)
	}
	return b, nil
}

func (s *Server) registerPush() {
	huma.Register(s.api, withSessionOnly(withStatus(withAuth(operation("add-push-subscription", http.MethodPost, "/users/@me/push-subscriptions",
		"Register a device's push gateway subscription for this account and session; registering the same subscription again replaces its keys", tagPush)), http.StatusCreated)),
		handle(s, func(ctx context.Context, in *Body[PushSubscriptionRequest]) (*Body[PushSubscription], error) {
			b := in.Body
			p256dh, err := decodeKey("p256dh", b.Keys.P256dh)
			if err != nil {
				return nil, err
			}
			auth, err := decodeKey("auth", b.Keys.Auth)
			if err != nil {
				return nil, err
			}
			p := mustPrincipal(ctx)
			sub, err := s.Service.AddPushSubscription(ctx, p, service.PushSubscriptionInput{
				GatewayURL: b.GatewayURL, SubscriptionID: b.SubscriptionID, AppID: b.AppID, Platform: b.Platform,
				P256dh: p256dh, Auth: auth, Mode: b.Mode, DeviceName: b.DeviceName,
			})
			if err != nil {
				return nil, err
			}
			return ok(toPushSubscription(sub, true))
		}))

	huma.Register(s.api, withAuth(operation("list-push-subscriptions", http.MethodGet, "/users/@me/push-subscriptions",
		"List the devices that receive this account's push notifications", tagPush)),
		handle(s, func(ctx context.Context, _ *struct{}) (*Body[[]PushSubscription], error) {
			p := mustPrincipal(ctx)
			subs, err := s.Service.ListPushSubscriptions(ctx, p)
			if err != nil {
				return nil, err
			}
			out := make([]PushSubscription, len(subs))
			for i, sub := range subs {
				out[i] = toPushSubscription(sub, p.Token == nil && sub.SessionID == p.SessionID)
			}
			return ok(out)
		}))

	huma.Register(s.api, withStatus(withAuth(operation("delete-push-subscription", http.MethodDelete, "/users/@me/push-subscriptions/{subscriptionID}",
		"Stop pushes to a device; the gateway subscription is deleted too when no other account here uses it", tagPush)), http.StatusNoContent),
		handle(s, func(ctx context.Context, in *PushSubscriptionPath) (*struct{}, error) {
			id, err := parseID("subscriptionID", in.ID)
			if err != nil {
				return nil, err
			}
			return nil, s.Service.DeletePushSubscription(ctx, mustPrincipal(ctx), id)
		}))

	huma.Register(s.api, withAuth(operation("get-push-preferences", http.MethodGet, "/users/@me/push-preferences",
		"Show which notifications are pushed, quiet hours and do-not-disturb", tagPush)),
		handle(s, func(ctx context.Context, _ *struct{}) (*Body[PushPreferences], error) {
			prefs, err := s.Service.GetPushPreferences(ctx, mustPrincipal(ctx))
			if err != nil {
				return nil, err
			}
			return ok(toPushPreferences(prefs))
		}))

	huma.Register(s.api, withAuth(operation("set-push-preferences", http.MethodPut, "/users/@me/push-preferences",
		"Replace which notifications are pushed, quiet hours and do-not-disturb", tagPush)),
		handle(s, func(ctx context.Context, in *Body[PushPreferences]) (*Body[PushPreferences], error) {
			b := in.Body
			prefs := service.PushPreferences{Categories: b.Categories, PausedUntil: b.PausedUntil}
			if q := b.QuietHours; q != nil {
				prefs.QuietHours = &service.QuietHours{Start: q.Start, End: q.End, Timezone: q.Timezone}
			}
			saved, err := s.Service.SetPushPreferences(ctx, mustPrincipal(ctx), prefs)
			if err != nil {
				return nil, err
			}
			return ok(toPushPreferences(saved))
		}))

	s.registerPushAdmin()
}

// PushWebApp is a gateway app that browsers can subscribe for.
type PushWebApp struct {
	AppID          string `json:"app_id"`
	VAPIDPublicKey string `json:"vapid_public_key" doc:"The applicationServerKey for pushManager.subscribe, base64url"`
}

func toPushWebApps(apps []service.PushWebApp) []PushWebApp {
	out := make([]PushWebApp, len(apps))
	for i, a := range apps {
		out[i] = PushWebApp{AppID: a.AppID, VAPIDPublicKey: a.VAPIDPublicKey}
	}
	return out
}

// PushAdminStatus is the administrators' view of push notifications.
type PushAdminStatus struct {
	Enabled       bool                                `json:"enabled"`
	Source        string                              `json:"source" enum:"default,config,settings"`
	Editable      bool                                `json:"editable" doc:"Change push with PATCH /instance/config (section push) unless it is set in the config file or environment"`
	GatewayURL    string                              `json:"gateway_url"`
	GatewayName   string                              `json:"gateway_name,omitempty"`
	Origin        string                              `json:"origin" doc:"The origin the instance registers as (server.public_url)"`
	Status        string                              `json:"status" enum:"unregistered,pending,active,suspended,blocked,error" doc:"active once the gateway verified the instance; pushes wait in the queue otherwise"`
	Detail        string                              `json:"detail" doc:"Why the status is not active, from the gateway where it said"`
	InstanceID    *string                             `json:"instance_id" doc:"The gateway's ID for this instance"`
	KeyID         *string                             `json:"key_id"`
	KeySource     string                              `json:"key_source" enum:"none,generated,configured"`
	PublicKey     string                              `json:"public_key" doc:"Published in /.well-known/gotalk-instance as push.keys"`
	VerifiedAt    *time.Time                          `json:"verified_at"`
	CheckedAt     *time.Time                          `json:"checked_at"`
	Subscriptions int64                               `json:"subscriptions" doc:"Devices registered on this instance"`
	WebApps       []PushWebApp                        `json:"web_apps"`
	Categories    []string                            `json:"categories" doc:"Notification categories people can turn pushes off for"`
	Settings      ConfigSection[service.PushSettings] `json:"settings"`
}

func (s *Server) pushAdminStatus(ctx context.Context) (*Body[PushAdminStatus], error) {
	st, err := s.Service.PushStatus(ctx, mustPrincipal(ctx))
	if err != nil {
		return nil, err
	}
	out := PushAdminStatus{
		Enabled: st.Enabled, Source: st.Source, Editable: st.Editable, GatewayURL: st.GatewayURL, GatewayName: st.GatewayName,
		Origin: st.Origin, Status: st.Status, Detail: st.Detail, InstanceID: st.InstanceID, KeyID: st.KeyID,
		KeySource: st.KeySource, PublicKey: st.PublicKey, VerifiedAt: st.VerifiedAt, CheckedAt: st.CheckedAt,
		Subscriptions: st.Subscriptions, WebApps: toPushWebApps(st.WebApps), Categories: service.PushCategories,
	}
	out.Settings = s.instanceConfig().Push
	return ok(out)
}

func (s *Server) registerPushAdmin() {
	huma.Register(s.api, withSessionOnly(withAuth(operation("get-instance-push", http.MethodGet, "/instance/push",
		"Show the push gateway registration, its status and published key (instance admins only)", tagInstance))),
		handle(s, func(ctx context.Context, _ *struct{}) (*Body[PushAdminStatus], error) {
			return s.pushAdminStatus(ctx)
		}))

	huma.Register(s.api, withSessionOnly(withAuth(operation("verify-instance-push", http.MethodPost, "/instance/push/verify",
		"Register with the push gateway or have it verify the instance again now (instance admins only)", tagInstance))),
		handle(s, func(ctx context.Context, _ *struct{}) (*Body[PushAdminStatus], error) {
			if err := s.Service.VerifyPushGateway(ctx, mustPrincipal(ctx)); err != nil {
				return nil, err
			}
			return s.pushAdminStatus(ctx)
		}))

	huma.Register(s.api, withSessionOnly(withAuth(operation("rotate-push-key", http.MethodPost, "/instance/push/keys/rotate",
		"Replace the generated gateway signing key; the gateway revokes the old one once it verifies the new one (instance admins only)", tagInstance))),
		handle(s, func(ctx context.Context, _ *struct{}) (*Body[PushAdminStatus], error) {
			if err := s.Service.RotatePushKey(ctx, mustPrincipal(ctx)); err != nil {
				return nil, err
			}
			return s.pushAdminStatus(ctx)
		}))
}
