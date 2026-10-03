package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"github.com/parkerbrown98/gotalk-server/internal/service"
)

const tagWebhooks = "Webhooks"

type WebhookPath struct {
	WebhookID string `path:"webhookID" format:"uuid"`
}

func (p WebhookPath) id() (uuid.UUID, error) { return parseID("webhookID", p.WebhookID) }

type DeliveryPath struct {
	WebhookPath
	DeliveryID string `path:"deliveryID" format:"uuid"`
}

type CreateWebhookRequest struct {
	Name   string   `json:"name" minLength:"1" maxLength:"100"`
	URL    string   `json:"url" minLength:"1" maxLength:"2048" doc:"Absolute http(s) URL that receives POSTed events"`
	Events []string `json:"events" minItems:"1" maxItems:"20" doc:"Events to send; see GET /instance webhooks.events"`
	Active *bool    `json:"active,omitempty" doc:"Defaults to true"`
}

type UpdateWebhookRequest struct {
	Name   *string  `json:"name,omitempty" minLength:"1" maxLength:"100"`
	URL    *string  `json:"url,omitempty" minLength:"1" maxLength:"2048"`
	Events []string `json:"events,omitempty" maxItems:"20"`
	Active *bool    `json:"active,omitempty" doc:"Re-enabling a webhook resets its failure count"`
}

func (s *Server) registerWebhooks() {
	huma.Register(s.api, withAuth(operation("list-webhooks", http.MethodGet, "/places/{place}/webhooks",
		"List the place's outgoing webhooks (MANAGE_WEBHOOKS)", tagWebhooks)),
		handle(s, func(ctx context.Context, in *PlacePath) (*Body[[]Webhook], error) {
			hooks, err := s.Service.ListWebhooks(ctx, mustPrincipal(ctx), in.Place)
			if err != nil {
				return nil, err
			}
			return ok(mapSlice(hooks, toWebhook))
		}))

	huma.Register(s.api, withStatus(withAuth(operation("create-webhook", http.MethodPost, "/places/{place}/webhooks",
		"Create an outgoing webhook (MANAGE_WEBHOOKS); the response includes its signing secret", tagWebhooks)), http.StatusCreated),
		handle(s, func(ctx context.Context, in *struct {
			PlacePath
			Body CreateWebhookRequest
		}) (*Body[Webhook], error) {
			active := in.Body.Active == nil || *in.Body.Active
			hook, err := s.Service.CreateWebhook(ctx, mustPrincipal(ctx), in.Place, service.WebhookInput{
				Name: in.Body.Name, URL: in.Body.URL, Events: in.Body.Events, Active: active,
			})
			if err != nil {
				return nil, err
			}
			return ok(toWebhookWithSecret(hook))
		}))

	huma.Register(s.api, withAuth(operation("get-webhook", http.MethodGet, "/webhooks/{webhookID}",
		"Get a webhook (MANAGE_WEBHOOKS)", tagWebhooks)),
		handle(s, func(ctx context.Context, in *WebhookPath) (*Body[Webhook], error) {
			id, err := in.id()
			if err != nil {
				return nil, err
			}
			hook, err := s.Service.GetWebhook(ctx, mustPrincipal(ctx), id)
			if err != nil {
				return nil, err
			}
			return ok(toWebhook(hook))
		}))

	huma.Register(s.api, withAuth(operation("update-webhook", http.MethodPatch, "/webhooks/{webhookID}",
		"Update a webhook (MANAGE_WEBHOOKS)", tagWebhooks)),
		handle(s, func(ctx context.Context, in *struct {
			WebhookPath
			Body UpdateWebhookRequest
		}) (*Body[Webhook], error) {
			id, err := in.id()
			if err != nil {
				return nil, err
			}
			upd := service.WebhookUpdate{Name: in.Body.Name, URL: in.Body.URL, Active: in.Body.Active}
			if in.Body.Events != nil {
				events := in.Body.Events
				upd.Events = &events
			}
			hook, err := s.Service.UpdateWebhook(ctx, mustPrincipal(ctx), id, upd)
			if err != nil {
				return nil, err
			}
			return ok(toWebhook(hook))
		}))

	huma.Register(s.api, withAuth(operation("delete-webhook", http.MethodDelete, "/webhooks/{webhookID}",
		"Delete a webhook and its delivery log (MANAGE_WEBHOOKS)", tagWebhooks)),
		handle(s, func(ctx context.Context, in *WebhookPath) (*struct{}, error) {
			id, err := in.id()
			if err != nil {
				return nil, err
			}
			return nil, s.Service.DeleteWebhook(ctx, mustPrincipal(ctx), id)
		}))

	huma.Register(s.api, withAuth(operation("rotate-webhook-secret", http.MethodPost, "/webhooks/{webhookID}/secret",
		"Replace a webhook's signing secret (MANAGE_WEBHOOKS)", tagWebhooks)),
		handle(s, func(ctx context.Context, in *WebhookPath) (*Body[Webhook], error) {
			id, err := in.id()
			if err != nil {
				return nil, err
			}
			hook, err := s.Service.RotateWebhookSecret(ctx, mustPrincipal(ctx), id)
			if err != nil {
				return nil, err
			}
			return ok(toWebhookWithSecret(hook))
		}))

	huma.Register(s.api, withStatus(withAuth(operation("ping-webhook", http.MethodPost, "/webhooks/{webhookID}/ping",
		"Send a ping event to check the endpoint (MANAGE_WEBHOOKS)", tagWebhooks)), http.StatusAccepted),
		handle(s, func(ctx context.Context, in *WebhookPath) (*Body[WebhookDelivery], error) {
			id, err := in.id()
			if err != nil {
				return nil, err
			}
			d, err := s.Service.PingWebhook(ctx, mustPrincipal(ctx), id)
			if err != nil {
				return nil, err
			}
			return ok(toWebhookDelivery(d))
		}))

	huma.Register(s.api, withAuth(operation("list-webhook-deliveries", http.MethodGet, "/webhooks/{webhookID}/deliveries",
		"Delivery log, newest first (MANAGE_WEBHOOKS)", tagWebhooks)),
		handle(s, func(ctx context.Context, in *struct {
			WebhookPath
			PageQuery
			Status string `query:"status" enum:"pending,succeeded,failed"`
		}) (*Body[Page[WebhookDelivery]], error) {
			id, err := in.id()
			if err != nil {
				return nil, err
			}
			page := in.pagination()
			ds, err := s.Service.ListWebhookDeliveries(ctx, mustPrincipal(ctx), id, in.Status, page)
			if err != nil {
				return nil, err
			}
			return ok(pageOf(ds, toWebhookDelivery, page))
		}))

	huma.Register(s.api, withStatus(withAuth(operation("redeliver-webhook", http.MethodPost,
		"/webhooks/{webhookID}/deliveries/{deliveryID}/redeliver",
		"Queue a new delivery with the same event and payload (MANAGE_WEBHOOKS)", tagWebhooks)), http.StatusAccepted),
		handle(s, func(ctx context.Context, in *DeliveryPath) (*Body[WebhookDelivery], error) {
			id, err := in.id()
			if err != nil {
				return nil, err
			}
			deliveryID, err := parseID("deliveryID", in.DeliveryID)
			if err != nil {
				return nil, err
			}
			d, err := s.Service.RedeliverWebhook(ctx, mustPrincipal(ctx), id, deliveryID)
			if err != nil {
				return nil, err
			}
			return ok(toWebhookDelivery(d))
		}))
}
