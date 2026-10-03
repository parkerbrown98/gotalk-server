package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/parkerbrown98/gotalk-server/internal/apperr"
	"github.com/parkerbrown98/gotalk-server/internal/database"
	"github.com/parkerbrown98/gotalk-server/internal/permissions"
	"github.com/parkerbrown98/gotalk-server/internal/store"
)

const (
	MaxApplicationsPerUser  = 25
	MaxCommandsPerApp       = 50
	maxCommandOptions       = 10
	maxAppNameLen           = 100
	maxAppDescriptionLen    = 1000
	maxCommandDescription   = 100
	maxInteractionStringLen = 1000

	EventInteractionCreate = "INTERACTION_CREATE"
)

var commandNamePattern = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)

var commandOptionTypes = []string{"string", "integer", "number", "boolean", "user", "channel"}

// ApplicationView is an application with its bot account.
type ApplicationView struct {
	App store.Application
	Bot store.User
}

type ApplicationInput struct {
	Name        string
	Description string
	IconURL     *string
	IsPublic    bool
	BotUsername string
}

type ApplicationUpdate struct {
	Name        *string
	Description *string
	IconURL     *string
	IsPublic    *bool
}

func normalizeAppName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > maxAppNameLen {
		return "", apperr.Invalid("name must be 1-%d characters", maxAppNameLen)
	}
	return name, nil
}

func validateAppDescription(d string) error {
	if utf8.RuneCountInString(d) > maxAppDescriptionLen {
		return apperr.Invalid("description must be at most %d characters", maxAppDescriptionLen)
	}
	return nil
}

// CreateApplication registers an application together with its bot account and returns
// the bot's token, which is only shown once.
func (s *Service) CreateApplication(ctx context.Context, p *Principal, in ApplicationInput) (ApplicationView, string, error) {
	if p.User.IsBot {
		return ApplicationView{}, "", apperr.Forbidden("bots cannot create applications")
	}
	name, err := normalizeAppName(in.Name)
	if err != nil {
		return ApplicationView{}, "", err
	}
	if err := errors.Join(validateAppDescription(in.Description), validateOptionalURL("icon_url", in.IconURL),
		validateUsername(in.BotUsername)); err != nil {
		return ApplicationView{}, "", firstAppErr(err)
	}
	var icon *string
	if in.IconURL != nil && *in.IconURL != "" {
		icon = in.IconURL
	}

	var (
		view  ApplicationView
		token string
	)
	err = s.tx(ctx, func(q *store.Queries) error {
		n, err := q.CountOwnedApplications(ctx, p.User.ID)
		if err != nil {
			return err
		}
		if n >= MaxApplicationsPerUser {
			return apperr.Conflict("you can have at most %d applications", MaxApplicationsPerUser)
		}
		botID, err := uuid.NewV7()
		if err != nil {
			return err
		}
		bot, err := q.CreateBotUser(ctx, store.CreateBotUserParams{
			ID: botID, Username: in.BotUsername, Email: "bot-" + botID.String() + "@bots.invalid", DisplayName: name,
		})
		if database.IsUniqueViolation(err, "users_username_key") {
			return apperr.Conflict("username is already taken")
		}
		if err != nil {
			return err
		}
		appID, err := uuid.NewV7()
		if err != nil {
			return err
		}
		app, err := q.CreateApplication(ctx, store.CreateApplicationParams{
			ID: appID, OwnerID: p.User.ID, BotUserID: bot.ID, Name: name, Description: in.Description,
			IconUrl: icon, IsPublic: in.IsPublic,
		})
		if err != nil {
			return err
		}
		token, err = s.issueBotToken(ctx, q, app)
		view = ApplicationView{App: app, Bot: bot}
		return err
	})
	return view, token, err
}

func (s *Service) applicationViews(ctx context.Context, q *store.Queries, apps []store.Application) ([]ApplicationView, error) {
	ids := make([]uuid.UUID, len(apps))
	for i, a := range apps {
		ids[i] = a.BotUserID
	}
	bots, err := s.usersByID(ctx, q, ids)
	if err != nil {
		return nil, err
	}
	out := make([]ApplicationView, len(apps))
	for i, a := range apps {
		out[i] = ApplicationView{App: a, Bot: bots[a.BotUserID]}
	}
	return out, nil
}

func (s *Service) ListMyApplications(ctx context.Context, p *Principal) ([]ApplicationView, error) {
	apps, err := s.q.ListOwnedApplications(ctx, p.User.ID)
	if err != nil {
		return nil, err
	}
	return s.applicationViews(ctx, s.q, apps)
}

type appAccess int

const (
	// appOwner may manage the application.
	appOwner appAccess = iota
	// appOwnerOrBot may also be the application's own bot.
	appOwnerOrBot
	// appVisible is anyone, for public applications.
	appVisible
)

// application loads an application the caller may access. Applications the caller cannot
// see are reported as not found.
func (s *Service) application(ctx context.Context, q *store.Queries, p *Principal, id uuid.UUID, need appAccess) (store.Application, error) {
	app, err := q.GetApplication(ctx, id)
	if err != nil {
		return app, notFound(err, "application not found")
	}
	owner := app.OwnerID == p.User.ID
	isBot := app.BotUserID == p.User.ID
	switch {
	case owner:
		return app, nil
	case isBot && need >= appOwnerOrBot:
		return app, nil
	case app.IsPublic && need == appVisible:
		return app, nil
	case app.IsPublic || isBot:
		return app, apperr.Forbidden("only the application's owner can do this")
	}
	return app, apperr.NotFound("application not found")
}

// GetApplication returns an application to its owner, its bot, or anyone if it is public.
func (s *Service) GetApplication(ctx context.Context, p *Principal, id uuid.UUID) (ApplicationView, error) {
	app, err := s.application(ctx, s.q, p, id, appVisible)
	if err != nil {
		return ApplicationView{}, err
	}
	views, err := s.applicationViews(ctx, s.q, []store.Application{app})
	if err != nil {
		return ApplicationView{}, err
	}
	return views[0], nil
}

func (s *Service) UpdateApplication(ctx context.Context, p *Principal, id uuid.UUID, in ApplicationUpdate) (ApplicationView, error) {
	if in.Name != nil {
		name, err := normalizeAppName(*in.Name)
		if err != nil {
			return ApplicationView{}, err
		}
		in.Name = &name
	}
	if in.Description != nil {
		if err := validateAppDescription(*in.Description); err != nil {
			return ApplicationView{}, err
		}
	}
	if err := validateOptionalURL("icon_url", in.IconURL); err != nil {
		return ApplicationView{}, err
	}
	if _, err := s.application(ctx, s.q, p, id, appOwner); err != nil {
		return ApplicationView{}, err
	}
	app, err := s.q.UpdateApplication(ctx, store.UpdateApplicationParams{
		ID: id, Name: in.Name, Description: in.Description, IconUrl: in.IconURL, IsPublic: in.IsPublic,
	})
	if err != nil {
		return ApplicationView{}, notFound(err, "application not found")
	}
	views, err := s.applicationViews(ctx, s.q, []store.Application{app})
	if err != nil {
		return ApplicationView{}, err
	}
	return views[0], nil
}

// DeleteApplication removes an application. Its bot leaves every place, its tokens stop
// working, and its messages and posts stay, attributed to a deleted account.
func (s *Service) DeleteApplication(ctx context.Context, p *Principal, id uuid.UUID) error {
	return s.tx(ctx, func(q *store.Queries) error {
		app, err := s.application(ctx, q, p, id, appOwner)
		if err != nil {
			return err
		}
		return s.deleteApplication(ctx, q, app)
	})
}

func (s *Service) deleteApplication(ctx context.Context, q *store.Queries, app store.Application) error {
	if err := s.deactivateUser(ctx, q, app.BotUserID); err != nil {
		return err
	}
	return q.DeleteApplication(ctx, app.ID)
}

// ResetBotToken revokes the bot's tokens (closing its gateway connections) and issues a new one.
func (s *Service) ResetBotToken(ctx context.Context, p *Principal, id uuid.UUID) (string, error) {
	var token string
	err := s.tx(ctx, func(q *store.Queries) error {
		app, err := s.application(ctx, q, p, id, appOwner)
		if err != nil {
			return err
		}
		revoked, err := q.RevokeApplicationTokens(ctx, &app.ID)
		if err != nil {
			return err
		}
		if len(revoked) > 0 {
			s.emitSessionsEnded(ctx, q, app.BotUserID, revoked, nil)
		}
		token, err = s.issueBotToken(ctx, q, app)
		return err
	})
	return token, err
}

// AddBot adds an application's bot to a place (MANAGE_PLACE). Public applications can be
// added by anyone with the permission; private ones only by their owner. The bot then
// holds the @everyone role like any new member.
func (s *Service) AddBot(ctx context.Context, p *Principal, ref string, appID uuid.UUID) (MemberView, error) {
	var view MemberView
	err := s.tx(ctx, func(q *store.Queries) error {
		place, _, err := s.requirePermission(ctx, q, p, ref, permissions.ManagePlace)
		if err != nil {
			return err
		}
		app, err := q.GetApplication(ctx, appID)
		if err != nil || (!app.IsPublic && app.OwnerID != p.User.ID) {
			return apperr.NotFound("application not found")
		}
		isMember, err := q.IsMember(ctx, store.IsMemberParams{PlaceID: place.ID, UserID: app.BotUserID})
		if err != nil {
			return err
		}
		if !isMember {
			banned, err := q.IsBanned(ctx, store.IsBannedParams{PlaceID: place.ID, UserID: app.BotUserID})
			if err != nil {
				return err
			}
			if banned {
				return apperr.Forbidden("this bot is banned from the place")
			}
			if err := s.addMember(ctx, q, place.ID, app.BotUserID); err != nil {
				return err
			}
			if err := s.audit(ctx, q, place.ID, p, "member.bot_add", "user", &app.BotUserID, "",
				map[string]any{"application_id": app.ID, "application_name": app.Name}); err != nil {
				return err
			}
		}
		view, err = s.getMember(ctx, q, place.ID, app.BotUserID)
		return err
	})
	return view, err
}

// CommandOption describes one argument of a slash command.
type CommandOption struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Type        string `json:"type"`
	Required    bool   `json:"required"`
}

type CommandInput struct {
	Name        string
	Description string
	Options     []CommandOption
}

type CommandView struct {
	Command store.ApplicationCommand
	Options []CommandOption
}

func validateCommandText(what, name, description string) error {
	if !commandNamePattern.MatchString(name) {
		return apperr.Invalid("%s name %q must be 1-32 lowercase letters, numbers, '-' or '_'", what, name)
	}
	if strings.TrimSpace(description) == "" || utf8.RuneCountInString(description) > maxCommandDescription {
		return apperr.Invalid("%s %q needs a description of 1-%d characters", what, name, maxCommandDescription)
	}
	return nil
}

func validateCommands(cmds []CommandInput) error {
	if len(cmds) > MaxCommandsPerApp {
		return apperr.Invalid("an application can have at most %d commands", MaxCommandsPerApp)
	}
	names := map[string]bool{}
	for _, c := range cmds {
		if err := validateCommandText("command", c.Name, c.Description); err != nil {
			return err
		}
		if names[c.Name] {
			return apperr.Invalid("command %q is defined twice", c.Name)
		}
		names[c.Name] = true
		if len(c.Options) > maxCommandOptions {
			return apperr.Invalid("command %q can have at most %d options", c.Name, maxCommandOptions)
		}
		opts := map[string]bool{}
		for _, o := range c.Options {
			if err := validateCommandText("option", o.Name, o.Description); err != nil {
				return err
			}
			if opts[o.Name] {
				return apperr.Invalid("command %q defines option %q twice", c.Name, o.Name)
			}
			opts[o.Name] = true
			if !slices.Contains(commandOptionTypes, o.Type) {
				return apperr.Invalid("option %q has an unknown type; use one of %s", o.Name, strings.Join(commandOptionTypes, ", "))
			}
		}
	}
	return nil
}

func commandViews(rows []store.ApplicationCommand) ([]CommandView, error) {
	out := make([]CommandView, len(rows))
	for i, r := range rows {
		opts := []CommandOption{}
		if err := json.Unmarshal(r.Options, &opts); err != nil {
			return nil, fmt.Errorf("decoding options of command %s: %w", r.ID, err)
		}
		out[i] = CommandView{Command: r, Options: opts}
	}
	return out, nil
}

// SetApplicationCommands replaces an application's slash commands. The owner or the bot
// itself may do this.
func (s *Service) SetApplicationCommands(ctx context.Context, p *Principal, appID uuid.UUID, cmds []CommandInput) ([]CommandView, error) {
	if err := validateCommands(cmds); err != nil {
		return nil, err
	}
	var rows []store.ApplicationCommand
	err := s.tx(ctx, func(q *store.Queries) error {
		app, err := s.application(ctx, q, p, appID, appOwnerOrBot)
		if err != nil {
			return err
		}
		if err := q.DeleteApplicationCommands(ctx, app.ID); err != nil {
			return err
		}
		for _, c := range cmds {
			opts := c.Options
			if opts == nil {
				opts = []CommandOption{}
			}
			raw, err := json.Marshal(opts)
			if err != nil {
				return err
			}
			id, err := uuid.NewV7()
			if err != nil {
				return err
			}
			row, err := q.CreateApplicationCommand(ctx, store.CreateApplicationCommandParams{
				ID: id, ApplicationID: app.ID, Name: c.Name, Description: strings.TrimSpace(c.Description), Options: raw,
			})
			if err != nil {
				return err
			}
			rows = append(rows, row)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(rows, func(a, b store.ApplicationCommand) int { return strings.Compare(a.Name, b.Name) })
	return commandViews(rows)
}

func (s *Service) ListApplicationCommands(ctx context.Context, p *Principal, appID uuid.UUID) ([]CommandView, error) {
	app, err := s.application(ctx, s.q, p, appID, appVisible)
	if err != nil {
		return nil, err
	}
	rows, err := s.q.ListApplicationCommands(ctx, app.ID)
	if err != nil {
		return nil, err
	}
	return commandViews(rows)
}

// ChannelCommand is a command a member can invoke in a channel.
type ChannelCommand struct {
	Application store.Application
	Bot         store.User
	Command     CommandView
}

// channelBots returns the applications whose bots can see a place channel.
func (s *Service) channelBots(ctx context.Context, q *store.Queries, cc *channelCtx) ([]store.Application, error) {
	apps, err := q.ListPlaceApplications(ctx, cc.scope.place.ID)
	if err != nil || len(apps) == 0 {
		return nil, err
	}
	ids := make([]uuid.UUID, len(apps))
	for i, a := range apps {
		ids[i] = a.BotUserID
	}
	st, err := s.standings(ctx, q, cc.scope.place, cc.scope.defaultRole, ids)
	if err != nil {
		return nil, err
	}
	var out []store.Application
	for _, a := range apps {
		if m, ok := st[a.BotUserID]; ok && cc.scope.visibleTo(m, cc.permID) {
			out = append(out, a)
		}
	}
	return out, nil
}

// ListChannelCommands lists the slash commands of bots that can see the channel. Direct
// messages have none.
func (s *Service) ListChannelCommands(ctx context.Context, p *Principal, channelID uuid.UUID) ([]ChannelCommand, error) {
	cc, err := s.channelFor(ctx, s.q, p, channelID)
	if err != nil {
		return nil, err
	}
	out := []ChannelCommand{}
	if cc.isDM() || !hasMessages(cc.ch.Kind) {
		return out, nil
	}
	apps, err := s.channelBots(ctx, s.q, cc)
	if err != nil || len(apps) == 0 {
		return out, err
	}
	appIDs := make([]uuid.UUID, len(apps))
	byID := map[uuid.UUID]store.Application{}
	for i, a := range apps {
		appIDs[i] = a.ID
		byID[a.ID] = a
	}
	views, err := s.applicationViews(ctx, s.q, apps)
	if err != nil {
		return nil, err
	}
	bots := map[uuid.UUID]store.User{}
	for _, v := range views {
		bots[v.App.ID] = v.Bot
	}
	rows, err := s.q.ListCommandsForApplications(ctx, appIDs)
	if err != nil {
		return nil, err
	}
	cmds, err := commandViews(rows)
	if err != nil {
		return nil, err
	}
	for _, c := range cmds {
		app := byID[c.Command.ApplicationID]
		out = append(out, ChannelCommand{Application: app, Bot: bots[app.ID], Command: c})
	}
	return out, nil
}

type InteractionInput struct {
	ApplicationID uuid.UUID
	Command       string
	Options       map[string]any
}

// InteractionView is a slash command invocation, relayed to the application's bot as an
// INTERACTION_CREATE gateway event.
type InteractionView struct {
	ID            uuid.UUID
	ApplicationID uuid.UUID
	Command       string
	Options       map[string]any
	PlaceID       uuid.UUID
	ChannelID     uuid.UUID
	User          store.User
	CreatedAt     time.Time
}

// InvokeCommand runs a slash command in a place channel. The caller needs SEND_MESSAGES
// there, and the command's bot must be able to see the channel. Bots answer by sending
// messages; invocations are not stored, so a bot that is offline misses them.
func (s *Service) InvokeCommand(ctx context.Context, p *Principal, channelID uuid.UUID, in InteractionInput) (InteractionView, error) {
	cc, err := s.channelFor(ctx, s.q, p, channelID)
	if err != nil {
		return InteractionView{}, err
	}
	if cc.isDM() {
		return InteractionView{}, apperr.Invalid("commands are only available in place channels")
	}
	if !hasMessages(cc.ch.Kind) {
		return InteractionView{}, noMessages(cc.ch.Kind)
	}
	if err := cc.require(permissions.SendMessages); err != nil {
		return InteractionView{}, err
	}
	apps, err := s.channelBots(ctx, s.q, cc)
	if err != nil {
		return InteractionView{}, err
	}
	i := slices.IndexFunc(apps, func(a store.Application) bool { return a.ID == in.ApplicationID })
	if i < 0 {
		return InteractionView{}, apperr.NotFound("command not found")
	}
	app := apps[i]
	cmd, err := s.q.GetApplicationCommand(ctx, store.GetApplicationCommandParams{ApplicationID: app.ID, Name: in.Command})
	if err != nil {
		return InteractionView{}, notFound(err, "command not found")
	}
	views, err := commandViews([]store.ApplicationCommand{cmd})
	if err != nil {
		return InteractionView{}, err
	}
	opts, err := interactionOptions(views[0].Options, in.Options)
	if err != nil {
		return InteractionView{}, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return InteractionView{}, err
	}
	view := InteractionView{
		ID: id, ApplicationID: app.ID, Command: cmd.Name, Options: opts, PlaceID: cc.scope.place.ID,
		ChannelID: cc.ch.ID, User: p.User, CreatedAt: time.Now().UTC(),
	}
	s.emit(ctx, s.q, Event{Type: EventInteractionCreate, Data: view, Users: []uuid.UUID{app.BotUserID}})
	return view, nil
}

// interactionOptions checks supplied option values against the command's definition.
func interactionOptions(defs []CommandOption, given map[string]any) (map[string]any, error) {
	out := map[string]any{}
	for name := range given {
		if !slices.ContainsFunc(defs, func(d CommandOption) bool { return d.Name == name }) {
			return nil, apperr.Invalid("unknown option %q", name)
		}
	}
	for _, d := range defs {
		v, ok := given[d.Name]
		if !ok || v == nil {
			if d.Required {
				return nil, apperr.Invalid("option %q is required", d.Name)
			}
			continue
		}
		bad := apperr.Invalid("option %q must be a %s", d.Name, d.Type)
		switch d.Type {
		case "string":
			str, ok := v.(string)
			if !ok || utf8.RuneCountInString(str) > maxInteractionStringLen {
				return nil, apperr.Invalid("option %q must be a string of at most %d characters", d.Name, maxInteractionStringLen)
			}
		case "integer":
			f, ok := v.(float64)
			if !ok || f != math.Trunc(f) || math.Abs(f) > 1<<53 {
				return nil, bad
			}
		case "number":
			if f, ok := v.(float64); !ok || math.IsInf(f, 0) || math.IsNaN(f) {
				return nil, bad
			}
		case "boolean":
			if _, ok := v.(bool); !ok {
				return nil, bad
			}
		case "user", "channel":
			str, ok := v.(string)
			if !ok {
				return nil, apperr.Invalid("option %q must be a %s ID", d.Name, d.Type)
			}
			if _, err := uuid.Parse(str); err != nil {
				return nil, apperr.Invalid("option %q must be a %s ID", d.Name, d.Type)
			}
		}
		out[d.Name] = v
	}
	return out, nil
}

// requireHuman rejects bots from actions reserved for people.
func requireHuman(p *Principal, msg string) error {
	if p.User.IsBot {
		return apperr.Forbidden("%s", msg)
	}
	return nil
}

// isBotUser reports whether userID is a bot account.
func isBotUser(ctx context.Context, q *store.Queries, userID uuid.UUID) (bool, error) {
	u, err := q.GetUserByID(ctx, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return u.IsBot, err
}
