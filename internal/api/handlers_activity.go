package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

const (
	tagNotifications = "Notifications"
	tagDrafts        = "Drafts"
)

type NotificationPath struct {
	NotificationID string `path:"notificationID" format:"uuid"`
}

type DraftPath struct {
	Key string `path:"key" minLength:"1" maxLength:"100" pattern:"^[a-zA-Z0-9:_.-]+$" doc:"Client-chosen, e.g. topic:<boardID> or reply:<topicID>"`
}

type UnreadCount struct {
	Count int64 `json:"count"`
}

type DraftRequest struct {
	Data map[string]any `json:"data" doc:"Any JSON object up to 64 KiB, e.g. {\"title\": ..., \"content\": ...}"`
}

func (s *Server) registerNotifications() {
	huma.Register(s.api, withAuth(operation("list-notifications", http.MethodGet, "/users/@me/notifications",
		"List the caller's notifications, newest first", tagNotifications)),
		handle(s, func(ctx context.Context, in *struct {
			PageQuery
			Unread bool `query:"unread" doc:"Only unread notifications"`
		}) (*Body[Page[Notification]], error) {
			page := in.pagination()
			items, err := s.Service.ListNotifications(ctx, mustPrincipal(ctx), in.Unread, page)
			if err != nil {
				return nil, err
			}
			return ok(pageOf(items, toNotification, page))
		}))

	huma.Register(s.api, withAuth(operation("count-unread-notifications", http.MethodGet, "/users/@me/notifications/unread-count",
		"Count unread notifications (for badges)", tagNotifications)),
		handle(s, func(ctx context.Context, _ *struct{}) (*Body[UnreadCount], error) {
			n, err := s.Service.CountUnreadNotifications(ctx, mustPrincipal(ctx))
			if err != nil {
				return nil, err
			}
			return ok(UnreadCount{Count: n})
		}))

	huma.Register(s.api, withStatus(withAuth(operation("mark-all-notifications-read", http.MethodPost,
		"/users/@me/notifications/read-all", "Mark every notification read", tagNotifications)), http.StatusNoContent),
		handle(s, func(ctx context.Context, _ *struct{}) (*struct{}, error) {
			return nil, s.Service.MarkAllNotificationsRead(ctx, mustPrincipal(ctx))
		}))

	huma.Register(s.api, withStatus(withAuth(operation("mark-notification-read", http.MethodPost,
		"/users/@me/notifications/{notificationID}/read", "Mark a notification read", tagNotifications)), http.StatusNoContent),
		handle(s, func(ctx context.Context, in *NotificationPath) (*struct{}, error) {
			id, err := parseID("notificationID", in.NotificationID)
			if err != nil {
				return nil, err
			}
			return nil, s.Service.MarkNotificationRead(ctx, mustPrincipal(ctx), id)
		}))

	huma.Register(s.api, withAuth(operation("delete-notification", http.MethodDelete,
		"/users/@me/notifications/{notificationID}", "Dismiss a notification", tagNotifications)),
		handle(s, func(ctx context.Context, in *NotificationPath) (*struct{}, error) {
			id, err := parseID("notificationID", in.NotificationID)
			if err != nil {
				return nil, err
			}
			return nil, s.Service.DeleteNotification(ctx, mustPrincipal(ctx), id)
		}))

	huma.Register(s.api, withStatus(withAuth(operation("set-place-subscription", http.MethodPut, "/places/{place}/subscription",
		"Watch (notify on every new topic), mute, or reset notifications for a place", tagNotifications)), http.StatusNoContent),
		handle(s, func(ctx context.Context, in *struct {
			PlacePath
			Body SubscriptionRequest
		}) (*struct{}, error) {
			return nil, s.Service.SetPlaceSubscription(ctx, mustPrincipal(ctx), in.Place, in.Body.Level)
		}))
}

func (s *Server) registerDrafts() {
	huma.Register(s.api, withAuth(operation("list-drafts", http.MethodGet, "/users/@me/drafts",
		"List saved drafts, most recently updated first", tagDrafts)),
		handle(s, func(ctx context.Context, _ *struct{}) (*Body[[]Draft], error) {
			drafts, err := s.Service.ListDrafts(ctx, mustPrincipal(ctx))
			if err != nil {
				return nil, err
			}
			return ok(mapSlice(drafts, toDraft))
		}))

	huma.Register(s.api, withAuth(operation("get-draft", http.MethodGet, "/users/@me/drafts/{key}",
		"Get a draft", tagDrafts)),
		handle(s, func(ctx context.Context, in *DraftPath) (*Body[Draft], error) {
			d, err := s.Service.GetDraft(ctx, mustPrincipal(ctx), in.Key)
			if err != nil {
				return nil, err
			}
			return ok(toDraft(d))
		}))

	huma.Register(s.api, withAuth(operation("save-draft", http.MethodPut, "/users/@me/drafts/{key}",
		"Create or replace a draft (at most 50 per user)", tagDrafts)),
		handle(s, func(ctx context.Context, in *struct {
			DraftPath
			Body DraftRequest
		}) (*Body[Draft], error) {
			d, err := s.Service.SaveDraft(ctx, mustPrincipal(ctx), in.Key, in.Body.Data)
			if err != nil {
				return nil, err
			}
			return ok(toDraft(d))
		}))

	huma.Register(s.api, withAuth(operation("delete-draft", http.MethodDelete, "/users/@me/drafts/{key}",
		"Delete a draft", tagDrafts)),
		handle(s, func(ctx context.Context, in *DraftPath) (*struct{}, error) {
			return nil, s.Service.DeleteDraft(ctx, mustPrincipal(ctx), in.Key)
		}))
}
