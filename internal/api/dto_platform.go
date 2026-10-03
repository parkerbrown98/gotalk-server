package api

import (
	"encoding/json"
	"time"

	"github.com/parkerbrown98/gotalk-server/internal/service"
	"github.com/parkerbrown98/gotalk-server/internal/store"
)

type APIToken struct {
	ID         string     `json:"id" format:"uuid"`
	Name       string     `json:"name"`
	Hint       string     `json:"hint" doc:"The token's first characters, to recognize it"`
	Scopes     []string   `json:"scopes"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at" doc:"Updated at most once a minute"`
	ExpiresAt  *time.Time `json:"expires_at" doc:"null for tokens that never expire"`
	Expired    bool       `json:"expired"`
}

func toAPIToken(t store.ApiToken) APIToken {
	return APIToken{
		ID: t.ID.String(), Name: t.Name, Hint: t.TokenHint, Scopes: t.Scopes, CreatedAt: t.CreatedAt,
		LastUsedAt: t.LastUsedAt, ExpiresAt: t.ExpiresAt, Expired: t.ExpiresAt != nil && t.ExpiresAt.Before(time.Now()),
	}
}

// CreatedAPIToken includes the secret token, which is shown only once.
type CreatedAPIToken struct {
	APIToken
	Token string `json:"token" doc:"Send as 'Authorization: Bearer <token>'. Shown only once."`
}

type Application struct {
	ID          string    `json:"id" format:"uuid"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	IconURL     *string   `json:"icon_url"`
	IsPublic    bool      `json:"is_public" doc:"Anyone with MANAGE_PLACE may add the bot to their place"`
	OwnerID     string    `json:"owner_id" format:"uuid"`
	Bot         User      `json:"bot"`
	CreatedAt   time.Time `json:"created_at"`
}

func toApplication(v service.ApplicationView) Application {
	a := v.App
	return Application{
		ID: a.ID.String(), Name: a.Name, Description: a.Description, IconURL: a.IconUrl, IsPublic: a.IsPublic,
		OwnerID: a.OwnerID.String(), Bot: toUser(v.Bot), CreatedAt: a.CreatedAt,
	}
}

// ApplicationWithToken includes the bot token, which is shown only once.
type ApplicationWithToken struct {
	Application
	BotToken string `json:"bot_token" doc:"The bot's API token. Shown only once; reset it if lost."`
}

type CommandOption struct {
	Name        string `json:"name" minLength:"1" maxLength:"32" pattern:"^[a-z0-9_-]+$"`
	Description string `json:"description" minLength:"1" maxLength:"100"`
	Type        string `json:"type" enum:"string,integer,number,boolean,user,channel"`
	Required    bool   `json:"required,omitempty"`
}

type Command struct {
	ID            string          `json:"id" format:"uuid"`
	ApplicationID string          `json:"application_id" format:"uuid"`
	Name          string          `json:"name"`
	Description   string          `json:"description"`
	Options       []CommandOption `json:"options"`
}

func toCommandOptions(opts []service.CommandOption) []CommandOption {
	out := make([]CommandOption, len(opts))
	for i, o := range opts {
		out[i] = CommandOption(o)
	}
	return out
}

func toCommand(v service.CommandView) Command {
	c := v.Command
	return Command{
		ID: c.ID.String(), ApplicationID: c.ApplicationID.String(), Name: c.Name, Description: c.Description,
		Options: toCommandOptions(v.Options),
	}
}

type ChannelCommand struct {
	Command
	ApplicationName string `json:"application_name"`
	Bot             User   `json:"bot"`
}

func toChannelCommand(v service.ChannelCommand) ChannelCommand {
	return ChannelCommand{Command: toCommand(v.Command), ApplicationName: v.Application.Name, Bot: toUser(v.Bot)}
}

type Interaction struct {
	ID            string         `json:"id" format:"uuid"`
	ApplicationID string         `json:"application_id" format:"uuid"`
	Command       string         `json:"command"`
	Options       map[string]any `json:"options"`
	PlaceID       string         `json:"place_id" format:"uuid"`
	ChannelID     string         `json:"channel_id" format:"uuid"`
	User          User           `json:"user" doc:"Who invoked the command"`
	CreatedAt     time.Time      `json:"created_at"`
}

func toInteraction(v service.InteractionView) Interaction {
	return Interaction{
		ID: v.ID.String(), ApplicationID: v.ApplicationID.String(), Command: v.Command, Options: v.Options,
		PlaceID: v.PlaceID.String(), ChannelID: v.ChannelID.String(), User: toUser(v.User), CreatedAt: v.CreatedAt,
	}
}

type Webhook struct {
	ID                  string     `json:"id" format:"uuid"`
	PlaceID             string     `json:"place_id" format:"uuid"`
	Name                string     `json:"name"`
	URL                 string     `json:"url"`
	Events              []string   `json:"events"`
	Active              bool       `json:"active"`
	DisabledReason      string     `json:"disabled_reason" doc:"Why the webhook is inactive, if it is"`
	ConsecutiveFailures int32      `json:"consecutive_failures" doc:"Deliveries in a row that failed every attempt"`
	LastDeliveryAt      *time.Time `json:"last_delivery_at"`
	LastSuccessAt       *time.Time `json:"last_success_at"`
	CreatedBy           *string    `json:"created_by" format:"uuid"`
	CreatedAt           time.Time  `json:"created_at"`
	UpdatedAt           time.Time  `json:"updated_at"`
	Secret              string     `json:"secret,omitempty" doc:"Signing secret; only returned when the webhook is created or its secret rotated"`
}

func toWebhook(w store.Webhook) Webhook {
	return Webhook{
		ID: w.ID.String(), PlaceID: w.PlaceID.String(), Name: w.Name, URL: w.Url, Events: w.Events,
		Active: w.IsActive, DisabledReason: w.DisabledReason, ConsecutiveFailures: w.ConsecutiveFailures,
		LastDeliveryAt: w.LastDeliveryAt, LastSuccessAt: w.LastSuccessAt, CreatedBy: idString(w.CreatedBy),
		CreatedAt: w.CreatedAt, UpdatedAt: w.UpdatedAt,
	}
}

func toWebhookWithSecret(w store.Webhook) Webhook {
	out := toWebhook(w)
	out.Secret = w.Secret
	return out
}

type WebhookDelivery struct {
	ID             string          `json:"id" format:"uuid"`
	WebhookID      string          `json:"webhook_id" format:"uuid"`
	Event          string          `json:"event"`
	Status         string          `json:"status" enum:"pending,succeeded,failed"`
	Attempts       int32           `json:"attempts"`
	NextAttemptAt  *time.Time      `json:"next_attempt_at" doc:"When a pending delivery is tried next"`
	ResponseStatus *int32          `json:"response_status" doc:"HTTP status of the last attempt, if it got a response"`
	Error          string          `json:"error" doc:"Why the last attempt failed"`
	DurationMs     *int32          `json:"duration_ms"`
	Payload        json.RawMessage `json:"payload" doc:"The event's data, as sent in the body's data field"`
	CreatedAt      time.Time       `json:"created_at"`
	CompletedAt    *time.Time      `json:"completed_at"`
}

func toWebhookDelivery(d store.WebhookDelivery) WebhookDelivery {
	out := WebhookDelivery{
		ID: d.ID.String(), WebhookID: d.WebhookID.String(), Event: d.Event, Status: d.Status, Attempts: d.Attempts,
		ResponseStatus: d.ResponseStatus, Error: d.LastError, DurationMs: d.DurationMs, Payload: d.Payload,
		CreatedAt: d.CreatedAt, CompletedAt: d.CompletedAt,
	}
	if d.Status == "pending" {
		next := d.NextAttemptAt
		out.NextAttemptAt = &next
	}
	return out
}

// PolicySummary describes a policy version without its text.
type PolicySummary struct {
	Kind            string    `json:"kind" enum:"terms,privacy,guidelines"`
	Version         int32     `json:"version"`
	Title           string    `json:"title"`
	Summary         string    `json:"summary" doc:"What changed since the previous version"`
	RequiresConsent bool      `json:"requires_consent"`
	EffectiveAt     time.Time `json:"effective_at"`
	PublishedAt     time.Time `json:"published_at"`
}

type Policy struct {
	PolicySummary
	Content string `json:"content" doc:"Markdown"`
}

func toPolicySummary(d store.PolicyDocument) PolicySummary {
	return PolicySummary{
		Kind: d.Kind, Version: d.Version, Title: d.Title, Summary: d.Summary, RequiresConsent: d.RequiresConsent,
		EffectiveAt: d.EffectiveAt, PublishedAt: d.CreatedAt,
	}
}

func toPolicy(d store.PolicyDocument) Policy {
	return Policy{PolicySummary: toPolicySummary(d), Content: d.Content}
}

type Consent struct {
	ID            string    `json:"id" format:"uuid"`
	Purpose       string    `json:"purpose"`
	PolicyVersion *int32    `json:"policy_version" doc:"The accepted policy version, for policy purposes"`
	Granted       bool      `json:"granted"`
	CreatedAt     time.Time `json:"created_at"`
}

func toConsent(c store.ConsentRecord) Consent {
	return Consent{ID: c.ID.String(), Purpose: c.Purpose, PolicyVersion: c.PolicyVersion, Granted: c.Granted, CreatedAt: c.CreatedAt}
}

type ConsentStatus struct {
	Consents    []Consent       `json:"consents" doc:"The latest decision for each purpose"`
	Outstanding []PolicySummary `json:"outstanding" doc:"Policies in effect that require consent to their current version"`
}

type TransparencyReport struct {
	PlaceID *string   `json:"place_id" format:"uuid" doc:"null for the whole instance"`
	Since   time.Time `json:"since"`
	Until   time.Time `json:"until"`
	Reports struct {
		Total    int64            `json:"total"`
		ByReason map[string]int64 `json:"by_reason"`
		ByStatus map[string]int64 `json:"by_status" doc:"Current status of the reports filed in the period"`
	} `json:"reports"`
	Actions        map[string]int64 `json:"actions" doc:"Moderation actions taken in the period, by audit log action"`
	ContentRemoved int64            `json:"content_removed" doc:"Messages, posts and topics removed by moderators"`
}

func toTransparency(r service.TransparencyReport) TransparencyReport {
	out := TransparencyReport{
		PlaceID: idString(r.PlaceID), Since: r.Since, Until: r.Until, Actions: r.Actions, ContentRemoved: r.ContentRemoved,
	}
	out.Reports.Total = r.ReportsTotal
	out.Reports.ByReason = r.ReportsReason
	out.Reports.ByStatus = r.ReportsStatus
	return out
}

type RateLimitStatus struct {
	Tier      string    `json:"tier"`
	Limit     int64     `json:"limit"`
	Period    string    `json:"period"`
	Remaining int64     `json:"remaining"`
	Reset     time.Time `json:"reset"`
}
