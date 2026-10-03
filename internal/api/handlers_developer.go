package api

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"github.com/parkerbrown98/gotalk-server/internal/service"
)

const tagDeveloper = "Developer"

type TokenPath struct {
	TokenID string `path:"tokenID" format:"uuid"`
}

type ApplicationPath struct {
	ApplicationID string `path:"applicationID" format:"uuid"`
}

func (p ApplicationPath) id() (uuid.UUID, error) { return parseID("applicationID", p.ApplicationID) }

type CreateTokenRequest struct {
	Name          string   `json:"name" minLength:"1" maxLength:"100"`
	Scopes        []string `json:"scopes" minItems:"1" maxItems:"4" doc:"read (GET requests), write (everything else), gateway (real-time connection), admin (keep instance administrator powers; administrators only)"`
	ExpiresInDays int      `json:"expires_in_days,omitempty" minimum:"0" maximum:"366" doc:"0 or omitted means the token never expires"`
}

type CreateApplicationRequest struct {
	Name        string  `json:"name" minLength:"1" maxLength:"100"`
	Description string  `json:"description,omitempty" maxLength:"1000"`
	IconURL     *string `json:"icon_url,omitempty" maxLength:"2048"`
	IsPublic    bool    `json:"is_public,omitempty" doc:"Let anyone with MANAGE_PLACE add the bot to their place"`
	BotUsername string  `json:"bot_username" minLength:"3" maxLength:"32" pattern:"^[a-zA-Z0-9_.-]+$" doc:"Username of the application's bot account"`
}

type UpdateApplicationRequest struct {
	Name        *string `json:"name,omitempty" minLength:"1" maxLength:"100"`
	Description *string `json:"description,omitempty" maxLength:"1000"`
	IconURL     *string `json:"icon_url,omitempty" maxLength:"2048" doc:"Empty string clears the icon"`
	IsPublic    *bool   `json:"is_public,omitempty"`
}

type BotToken struct {
	BotToken string `json:"bot_token" doc:"Shown only once"`
}

type CommandRequest struct {
	Name        string          `json:"name" minLength:"1" maxLength:"32" pattern:"^[a-z0-9_-]+$"`
	Description string          `json:"description" minLength:"1" maxLength:"100"`
	Options     []CommandOption `json:"options,omitempty" maxItems:"10"`
}

type AddBotRequest struct {
	ApplicationID string `json:"application_id" format:"uuid"`
}

type InteractionRequest struct {
	ApplicationID string         `json:"application_id" format:"uuid"`
	Command       string         `json:"command" minLength:"1" maxLength:"32"`
	Options       map[string]any `json:"options,omitempty" doc:"Option values by name"`
}

func (s *Server) registerDeveloper() {
	huma.Register(s.api, withSessionOnly(withAuth(operation("list-my-tokens", http.MethodGet, "/users/@me/tokens",
		"List personal access tokens", tagDeveloper))),
		handle(s, func(ctx context.Context, _ *struct{}) (*Body[[]APIToken], error) {
			tokens, err := s.Service.ListPersonalTokens(ctx, mustPrincipal(ctx))
			if err != nil {
				return nil, err
			}
			return ok(mapSlice(tokens, toAPIToken))
		}))

	huma.Register(s.api, withAuthRateLimit(withSessionOnly(withStatus(withAuth(operation("create-my-token", http.MethodPost,
		"/users/@me/tokens", "Create a personal access token for scripts and integrations", tagDeveloper)), http.StatusCreated))),
		handle(s, func(ctx context.Context, in *Body[CreateTokenRequest]) (*Body[CreatedAPIToken], error) {
			t, secret, err := s.Service.CreatePersonalToken(ctx, mustPrincipal(ctx), service.TokenInput{
				Name: in.Body.Name, Scopes: in.Body.Scopes, ExpiresIn: time.Duration(in.Body.ExpiresInDays) * 24 * time.Hour,
			})
			if err != nil {
				return nil, err
			}
			return ok(CreatedAPIToken{APIToken: toAPIToken(t), Token: secret})
		}))

	huma.Register(s.api, withSessionOnly(withAuth(operation("revoke-my-token", http.MethodDelete, "/users/@me/tokens/{tokenID}",
		"Revoke a personal access token; its gateway connections close", tagDeveloper))),
		handle(s, func(ctx context.Context, in *TokenPath) (*struct{}, error) {
			id, err := parseID("tokenID", in.TokenID)
			if err != nil {
				return nil, err
			}
			return nil, s.Service.RevokePersonalToken(ctx, mustPrincipal(ctx), id)
		}))

	huma.Register(s.api, withOptionalAuth(operation("get-rate-limits", http.MethodGet, "/rate-limits",
		"Show the caller's standing in every rate limit tier (per user, or per IP when signed out)", tagDeveloper)),
		handle(s, func(ctx context.Context, _ *struct{}) (*Body[[]RateLimitStatus], error) {
			out := []RateLimitStatus{}
			if !s.Config.RateLimit.Enabled {
				return ok(out)
			}
			key := rateLimitKey(ctx)
			for _, tier := range s.Limiter.Tiers() {
				res, err := s.Limiter.Peek(ctx, tier.Name, key)
				if err != nil {
					return nil, err
				}
				out = append(out, RateLimitStatus{
					Tier: tier.Name, Limit: tier.Limit, Period: tier.Period, Remaining: res.Remaining, Reset: res.Reset,
				})
			}
			return ok(out)
		}))

	s.registerApplications()
	s.registerCommands()
}

func (s *Server) registerApplications() {
	huma.Register(s.api, withAuth(operation("list-my-applications", http.MethodGet, "/applications",
		"List applications the caller owns", tagDeveloper)),
		handle(s, func(ctx context.Context, _ *struct{}) (*Body[[]Application], error) {
			apps, err := s.Service.ListMyApplications(ctx, mustPrincipal(ctx))
			if err != nil {
				return nil, err
			}
			return ok(mapSlice(apps, toApplication))
		}))

	huma.Register(s.api, withAuthRateLimit(withSessionOnly(withStatus(withAuth(operation("create-application", http.MethodPost,
		"/applications", "Register an application and its bot account", tagDeveloper)), http.StatusCreated))),
		handle(s, func(ctx context.Context, in *Body[CreateApplicationRequest]) (*Body[ApplicationWithToken], error) {
			b := in.Body
			app, token, err := s.Service.CreateApplication(ctx, mustPrincipal(ctx), service.ApplicationInput{
				Name: b.Name, Description: b.Description, IconURL: b.IconURL, IsPublic: b.IsPublic, BotUsername: b.BotUsername,
			})
			if err != nil {
				return nil, err
			}
			return ok(ApplicationWithToken{Application: toApplication(app), BotToken: token})
		}))

	huma.Register(s.api, withAuth(operation("get-application", http.MethodGet, "/applications/{applicationID}",
		"Get an application (owners and its bot; anyone for public applications)", tagDeveloper)),
		handle(s, func(ctx context.Context, in *ApplicationPath) (*Body[Application], error) {
			id, err := in.id()
			if err != nil {
				return nil, err
			}
			app, err := s.Service.GetApplication(ctx, mustPrincipal(ctx), id)
			if err != nil {
				return nil, err
			}
			return ok(toApplication(app))
		}))

	huma.Register(s.api, withAuth(operation("update-application", http.MethodPatch, "/applications/{applicationID}",
		"Update an application (owner only)", tagDeveloper)),
		handle(s, func(ctx context.Context, in *struct {
			ApplicationPath
			Body UpdateApplicationRequest
		}) (*Body[Application], error) {
			id, err := in.id()
			if err != nil {
				return nil, err
			}
			b := in.Body
			app, err := s.Service.UpdateApplication(ctx, mustPrincipal(ctx), id, service.ApplicationUpdate{
				Name: b.Name, Description: b.Description, IconURL: b.IconURL, IsPublic: b.IsPublic,
			})
			if err != nil {
				return nil, err
			}
			return ok(toApplication(app))
		}))

	huma.Register(s.api, withSessionOnly(withAuth(operation("delete-application", http.MethodDelete, "/applications/{applicationID}",
		"Delete an application; its bot leaves every place and its token stops working", tagDeveloper))),
		handle(s, func(ctx context.Context, in *ApplicationPath) (*struct{}, error) {
			id, err := in.id()
			if err != nil {
				return nil, err
			}
			return nil, s.Service.DeleteApplication(ctx, mustPrincipal(ctx), id)
		}))

	huma.Register(s.api, withAuthRateLimit(withSessionOnly(withAuth(operation("reset-bot-token", http.MethodPost,
		"/applications/{applicationID}/bot/token", "Revoke the bot's token and issue a new one", tagDeveloper)))),
		handle(s, func(ctx context.Context, in *ApplicationPath) (*Body[BotToken], error) {
			id, err := in.id()
			if err != nil {
				return nil, err
			}
			token, err := s.Service.ResetBotToken(ctx, mustPrincipal(ctx), id)
			if err != nil {
				return nil, err
			}
			return ok(BotToken{BotToken: token})
		}))

	huma.Register(s.api, withAuth(operation("add-bot", http.MethodPost, "/places/{place}/bots",
		"Add an application's bot to the place (MANAGE_PLACE)", tagDeveloper)),
		handle(s, func(ctx context.Context, in *struct {
			PlacePath
			Body AddBotRequest
		}) (*Body[Member], error) {
			id, err := parseID("application_id", in.Body.ApplicationID)
			if err != nil {
				return nil, err
			}
			m, err := s.Service.AddBot(ctx, mustPrincipal(ctx), in.Place, id)
			if err != nil {
				return nil, err
			}
			return ok(toMember(m))
		}))
}

func (s *Server) registerCommands() {
	huma.Register(s.api, withAuth(operation("list-application-commands", http.MethodGet, "/applications/{applicationID}/commands",
		"List an application's slash commands", tagDeveloper)),
		handle(s, func(ctx context.Context, in *ApplicationPath) (*Body[[]Command], error) {
			id, err := in.id()
			if err != nil {
				return nil, err
			}
			cmds, err := s.Service.ListApplicationCommands(ctx, mustPrincipal(ctx), id)
			if err != nil {
				return nil, err
			}
			return ok(mapSlice(cmds, toCommand))
		}))

	huma.Register(s.api, withAuth(operation("set-application-commands", http.MethodPut, "/applications/{applicationID}/commands",
		"Replace an application's slash commands (owner or the bot itself)", tagDeveloper)),
		handle(s, func(ctx context.Context, in *struct {
			ApplicationPath
			Body []CommandRequest `maxItems:"50"`
		}) (*Body[[]Command], error) {
			id, err := in.id()
			if err != nil {
				return nil, err
			}
			cmds := make([]service.CommandInput, len(in.Body))
			for i, c := range in.Body {
				opts := make([]service.CommandOption, len(c.Options))
				for j, o := range c.Options {
					opts[j] = service.CommandOption(o)
				}
				cmds[i] = service.CommandInput{Name: c.Name, Description: c.Description, Options: opts}
			}
			views, err := s.Service.SetApplicationCommands(ctx, mustPrincipal(ctx), id, cmds)
			if err != nil {
				return nil, err
			}
			return ok(mapSlice(views, toCommand))
		}))

	huma.Register(s.api, withAuth(operation("list-channel-commands", http.MethodGet, "/channels/{channelID}/commands",
		"List slash commands of the bots that can see this channel", tagDeveloper)),
		handle(s, func(ctx context.Context, in *ChannelPath) (*Body[[]ChannelCommand], error) {
			id, err := in.id()
			if err != nil {
				return nil, err
			}
			cmds, err := s.Service.ListChannelCommands(ctx, mustPrincipal(ctx), id)
			if err != nil {
				return nil, err
			}
			return ok(mapSlice(cmds, toChannelCommand))
		}))

	huma.Register(s.api, withChatRateLimit(withStatus(withAuth(operation("invoke-command", http.MethodPost,
		"/channels/{channelID}/interactions",
		"Invoke a slash command (SEND_MESSAGES); the bot receives INTERACTION_CREATE and answers with a message", tagDeveloper)),
		http.StatusAccepted)),
		handle(s, func(ctx context.Context, in *struct {
			ChannelPath
			Body InteractionRequest
		}) (*Body[Interaction], error) {
			channelID, err := in.id()
			if err != nil {
				return nil, err
			}
			appID, err := parseID("application_id", in.Body.ApplicationID)
			if err != nil {
				return nil, err
			}
			v, err := s.Service.InvokeCommand(ctx, mustPrincipal(ctx), channelID, service.InteractionInput{
				ApplicationID: appID, Command: in.Body.Command, Options: in.Body.Options,
			})
			if err != nil {
				return nil, err
			}
			return ok(toInteraction(v))
		}))
}
