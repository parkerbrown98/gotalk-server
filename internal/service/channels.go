package service

import (
	"context"
	"errors"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/parkerbrown98/gotalk-server/internal/apperr"
	"github.com/parkerbrown98/gotalk-server/internal/database"
	"github.com/parkerbrown98/gotalk-server/internal/permissions"
	"github.com/parkerbrown98/gotalk-server/internal/realtime"
	"github.com/parkerbrown98/gotalk-server/internal/store"
)

const (
	maxChannelsPerPlace = 200
	maxChannelNameLen   = 100
	maxChannelTopicLen  = 1024

	kindCategory = "category"
	kindText     = "text"
	kindThread   = "thread"
	kindDM       = "dm"
	kindGroupDM  = "group_dm"
)

// dmPermissions is what every recipient may do in a direct message.
const dmPermissions = permissions.ViewChannels | permissions.SendMessages | permissions.AddReactions | permissions.AttachFiles

// hasMessages reports whether channels of kind hold messages.
func hasMessages(kind string) bool { return kind != kindCategory && kind != kindVoice }

// noMessages rejects message operations on channels without messages.
func noMessages(kind string) error {
	if kind == kindVoice {
		return apperr.Invalid("voice channels do not have messages")
	}
	return apperr.Invalid("categories do not have messages")
}

// overwritable is what a role overwrite may allow or deny on a channel of kind.
func overwritable(kind string) permissions.Permission {
	switch kind {
	case kindVoice:
		return permissions.Voice
	case kindCategory:
		return permissions.Chat | permissions.Voice
	}
	return permissions.Chat
}

// chatScope is a place's channels plus their overwrites, loaded once so permissions can be
// evaluated in memory. Only categories, text and voice channels are loaded; threads use
// their parent's permissions.
type chatScope struct {
	place       store.Place
	acc         Access
	defaultRole store.Role
	channels    map[uuid.UUID]store.Channel
	ordered     []store.Channel
	overwrites  map[uuid.UUID][]permissions.Overwrite
}

func (s *Service) loadChannels(ctx context.Context, q *store.Queries, place store.Place) (*chatScope, error) {
	def, err := q.GetDefaultRole(ctx, place.ID)
	if err != nil {
		return nil, err
	}
	channels, err := q.ListPlaceChannels(ctx, &place.ID)
	if err != nil {
		return nil, err
	}
	rows, err := q.ListChannelOverwrites(ctx, place.ID)
	if err != nil {
		return nil, err
	}
	c := &chatScope{
		place: place, defaultRole: def,
		channels:   make(map[uuid.UUID]store.Channel, len(channels)),
		overwrites: map[uuid.UUID][]permissions.Overwrite{},
	}
	for _, ch := range channels {
		c.channels[ch.ID] = ch
	}
	c.ordered = channelTreeOrder(channels)
	for _, o := range rows {
		c.overwrites[o.ChannelID] = append(c.overwrites[o.ChannelID], permissions.Overwrite{
			RoleID: o.RoleID, Allow: permissions.Permission(o.Allow), Deny: permissions.Permission(o.Deny),
		})
	}
	return c, nil
}

// channelTreeOrder lists top-level entries by position, each category followed by its
// channels. channels must already be sorted by position.
func channelTreeOrder(channels []store.Channel) []store.Channel {
	children := map[uuid.UUID][]store.Channel{}
	var roots []store.Channel
	for _, ch := range channels {
		if ch.ParentID == nil {
			roots = append(roots, ch)
		} else {
			children[*ch.ParentID] = append(children[*ch.ParentID], ch)
		}
	}
	out := make([]store.Channel, 0, len(channels))
	for _, r := range roots {
		out = append(out, r)
		out = append(out, children[r.ID]...)
	}
	return out
}

// chat resolves a place's channels for a member. Chat is members-only, even in public places.
func (s *Service) chat(ctx context.Context, q *store.Queries, p *Principal, ref string) (*chatScope, error) {
	place, acc, err := s.placeFor(ctx, q, p, ref)
	if err != nil {
		return nil, err
	}
	if !acc.IsMember {
		return nil, apperr.Forbidden("you must be a member of this place to use its channels")
	}
	c, err := s.loadChannels(ctx, q, place)
	if err != nil {
		return nil, err
	}
	c.acc = acc
	return c, nil
}

// chain returns the channel's ancestry, category first, or nil if unknown.
func (c *chatScope) chain(channelID uuid.UUID) []store.Channel {
	ch, ok := c.channels[channelID]
	if !ok {
		return nil
	}
	if ch.ParentID == nil {
		return []store.Channel{ch}
	}
	parent, ok := c.channels[*ch.ParentID]
	if !ok {
		return nil
	}
	return []store.Channel{parent, ch}
}

func (c *chatScope) levels(chain []store.Channel) [][]permissions.Overwrite {
	out := make([][]permissions.Overwrite, len(chain))
	for i, ch := range chain {
		out[i] = c.overwrites[ch.ID]
	}
	return out
}

func (c *chatScope) permsFor(m permissions.Member, channelID uuid.UUID) permissions.Permission {
	chain := c.chain(channelID)
	if chain == nil {
		return 0
	}
	return m.InScope(c.levels(chain)...)
}

// visibleTo reports whether m can view the channel and its category.
func (c *chatScope) visibleTo(m permissions.Member, channelID uuid.UUID) bool {
	chain := c.chain(channelID)
	if chain == nil {
		return false
	}
	levels := c.levels(chain)
	for i := range chain {
		if m.InScope(levels[:i+1]...)&permissions.ViewChannels == 0 {
			return false
		}
	}
	return true
}

func (c *chatScope) perms(channelID uuid.UUID) permissions.Permission {
	return c.permsFor(c.acc.Member, channelID)
}

func (c *chatScope) has(channelID uuid.UUID, perm permissions.Permission) bool {
	return c.perms(channelID)&perm == perm
}

func (c *chatScope) canView(channelID uuid.UUID) bool { return c.visibleTo(c.acc.Member, channelID) }

// canManageUnder reports whether the caller may create or move channels under parent (or
// at the top level when parent is nil).
func (c *chatScope) canManageUnder(parentID *uuid.UUID) bool {
	if parentID == nil {
		return c.acc.Member.Has(permissions.ManageChannels)
	}
	return c.has(*parentID, permissions.ManageChannels)
}

// standings evaluates several users' standing in a place in a few queries. Non-members
// are omitted.
func (s *Service) standings(ctx context.Context, q *store.Queries, place store.Place, def store.Role, userIDs []uuid.UUID) (map[uuid.UUID]permissions.Member, error) {
	out := map[uuid.UUID]permissions.Member{}
	if len(userIDs) == 0 {
		return out, nil
	}
	members, err := q.ListMembersAmong(ctx, store.ListMembersAmongParams{PlaceID: place.ID, UserIds: userIDs})
	if err != nil {
		return nil, err
	}
	assigned, err := q.ListMemberRoleIDs(ctx, store.ListMemberRoleIDsParams{PlaceID: place.ID, UserIds: userIDs})
	if err != nil {
		return nil, err
	}
	roles, err := q.ListRoles(ctx, place.ID)
	if err != nil {
		return nil, err
	}
	rolePerms := make(map[uuid.UUID]store.Role, len(roles))
	for _, r := range roles {
		rolePerms[r.ID] = r
	}
	roleIDs := map[uuid.UUID][]uuid.UUID{}
	for _, a := range assigned {
		roleIDs[a.UserID] = append(roleIDs[a.UserID], a.RoleID)
	}
	for _, m := range members {
		std := permissions.Member{
			IsOwner:       place.OwnerID == m.UserID,
			Raw:           permissions.Permission(def.Permissions),
			DefaultRoleID: def.ID,
			RoleIDs:       append([]uuid.UUID{def.ID}, roleIDs[m.UserID]...),
			TimedOut:      timedOut(m.TimeoutUntil),
		}
		for _, id := range roleIDs[m.UserID] {
			std.Raw |= permissions.Permission(rolePerms[id].Permissions)
			std.TopPosition = max(std.TopPosition, rolePerms[id].Position)
		}
		out[m.UserID] = std
	}
	return out, nil
}

// channelCtx is a channel the caller can see, with their permissions in it.
type channelCtx struct {
	ch store.Channel
	// scope is nil for direct messages.
	scope *chatScope
	// permID is the channel whose permissions apply: the parent for threads.
	permID     uuid.UUID
	perms      permissions.Permission
	recipients []uuid.UUID
}

func (c *channelCtx) isDM() bool { return c.scope == nil }

func (c *channelCtx) has(perm permissions.Permission) bool { return c.perms&perm == perm }

// require checks the caller may take an action in the channel.
func (c *channelCtx) require(perm permissions.Permission) error {
	if c.has(perm) {
		return nil
	}
	if c.scope != nil && c.scope.acc.Member.TimedOut && perm&permissions.Participation != 0 {
		return apperr.Forbidden("you are timed out in this place")
	}
	return missing(perm)
}

// canModerate reports whether the caller holds MANAGE_MESSAGES in a place channel.
func (c *channelCtx) canModerate() bool {
	return !c.isDM() && c.has(permissions.ManageMessages)
}

// event routes ev to everyone who can see the channel.
func (c *channelCtx) event(eventType string, data any) Event {
	if c.isDM() {
		return Event{Type: eventType, Data: data, Users: c.recipients}
	}
	return Event{Type: eventType, Data: data, Places: []uuid.UUID{c.scope.place.ID}, ChannelID: &c.permID}
}

func (c *channelCtx) placeID() *uuid.UUID { return c.ch.PlaceID }

// channelFor resolves any channel the caller can see. Unknown and hidden channels are
// both reported as not found.
func (s *Service) channelFor(ctx context.Context, q *store.Queries, p *Principal, channelID uuid.UUID) (*channelCtx, error) {
	ch, err := q.GetChannel(ctx, channelID)
	if err != nil {
		return nil, notFound(err, "channel not found")
	}
	return s.channelCtxOf(ctx, q, p, ch)
}

func (s *Service) channelCtxOf(ctx context.Context, q *store.Queries, p *Principal, ch store.Channel) (*channelCtx, error) {
	if ch.PlaceID == nil {
		rows, err := q.ListChannelRecipients(ctx, []uuid.UUID{ch.ID})
		if err != nil {
			return nil, err
		}
		cc := &channelCtx{ch: ch, permID: ch.ID, perms: dmPermissions}
		for _, r := range rows {
			cc.recipients = append(cc.recipients, r.UserID)
		}
		if !slices.Contains(cc.recipients, p.User.ID) {
			return nil, apperr.NotFound("channel not found")
		}
		return cc, nil
	}
	scope, err := s.chat(ctx, q, p, ch.PlaceID.String())
	if err != nil {
		return nil, hideAsNotFound(err, "channel not found")
	}
	permID := ch.ID
	if ch.Kind == kindThread {
		permID = *ch.ParentID
	}
	if !scope.canView(permID) {
		return nil, apperr.NotFound("channel not found")
	}
	return &channelCtx{ch: ch, scope: scope, permID: permID, perms: scope.perms(permID)}, nil
}

// ReadState is a user's position in a channel.
type ReadState struct {
	LastReadMessageID *uuid.UUID
	MentionCount      int32
}

// ChannelView is a channel plus, for the caller, their permissions, read state and
// notification level. Caller fields are nil in gateway events.
type ChannelView struct {
	Channel      store.Channel
	Recipients   []store.User
	Permissions  *permissions.Permission
	ReadState    *ReadState
	Subscription string
}

// channelViews decorates channels with recipients and, when p is set, caller state.
// perms supplies the caller's permissions per channel.
func (s *Service) channelViews(ctx context.Context, q *store.Queries, p *Principal, chans []store.Channel, perms func(store.Channel) permissions.Permission) ([]ChannelView, error) {
	out := make([]ChannelView, len(chans))
	if len(chans) == 0 {
		return out, nil
	}
	ids := make([]uuid.UUID, len(chans))
	var dmIDs []uuid.UUID
	for i, ch := range chans {
		ids[i] = ch.ID
		if ch.PlaceID == nil {
			dmIDs = append(dmIDs, ch.ID)
		}
	}
	recipients := map[uuid.UUID][]store.User{}
	if len(dmIDs) > 0 {
		rows, err := q.ListChannelRecipients(ctx, dmIDs)
		if err != nil {
			return nil, err
		}
		var userIDs []uuid.UUID
		for _, r := range rows {
			userIDs = append(userIDs, r.UserID)
		}
		users, err := s.usersByID(ctx, q, userIDs)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			if u, ok := users[r.UserID]; ok {
				recipients[r.ChannelID] = append(recipients[r.ChannelID], u)
			}
		}
	}
	reads := map[uuid.UUID]store.ChannelRead{}
	subs := map[uuid.UUID]string{}
	if p != nil {
		rows, err := q.ListChannelReads(ctx, store.ListChannelReadsParams{UserID: p.User.ID, ChannelIds: ids})
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			reads[r.ChannelID] = r
		}
		subRows, err := q.ListUserSubscriptions(ctx, store.ListUserSubscriptionsParams{UserID: p.User.ID, TargetIds: ids})
		if err != nil {
			return nil, err
		}
		for _, r := range subRows {
			subs[r.TargetID] = r.Level
		}
	}
	for i, ch := range chans {
		v := ChannelView{Channel: ch, Recipients: recipients[ch.ID]}
		if p != nil && hasMessages(ch.Kind) {
			pp := perms(ch)
			v.Permissions = &pp
			r := reads[ch.ID]
			v.ReadState = &ReadState{LastReadMessageID: r.LastReadMessageID, MentionCount: r.MentionCount}
			v.Subscription = subs[ch.ID]
			if v.Subscription == "" {
				v.Subscription = "normal"
			}
		} else if p != nil {
			pp := perms(ch)
			v.Permissions = &pp
		}
		out[i] = v
	}
	return out, nil
}

func (s *Service) channelView(ctx context.Context, q *store.Queries, p *Principal, cc *channelCtx) (ChannelView, error) {
	views, err := s.channelViews(ctx, q, p, []store.Channel{cc.ch}, func(store.Channel) permissions.Permission { return cc.perms })
	if err != nil {
		return ChannelView{}, err
	}
	return views[0], nil
}

// publicChannelView is a channel without caller state, for gateway events.
func (s *Service) publicChannelView(ctx context.Context, q *store.Queries, ch store.Channel) (ChannelView, error) {
	views, err := s.channelViews(ctx, q, nil, []store.Channel{ch}, nil)
	if err != nil {
		return ChannelView{}, err
	}
	return views[0], nil
}

// ListChannels returns the place's categories and text channels the caller can see, each
// category followed by its channels.
func (s *Service) ListChannels(ctx context.Context, p *Principal, ref string) ([]ChannelView, error) {
	c, err := s.chat(ctx, s.q, p, ref)
	if err != nil {
		return nil, err
	}
	visible := []store.Channel{}
	for _, ch := range c.ordered {
		if c.canView(ch.ID) {
			visible = append(visible, ch)
		}
	}
	return s.channelViews(ctx, s.q, p, visible, func(ch store.Channel) permissions.Permission { return c.perms(ch.ID) })
}

func (s *Service) GetChannel(ctx context.Context, p *Principal, channelID uuid.UUID) (ChannelView, error) {
	cc, err := s.channelFor(ctx, s.q, p, channelID)
	if err != nil {
		return ChannelView{}, err
	}
	return s.channelView(ctx, s.q, p, cc)
}

type CreateChannelInput struct {
	Kind     string
	Name     string
	Topic    string
	ParentID *uuid.UUID
	IsNSFW   bool
	// UserLimit caps a voice channel's participants; 0 is unlimited.
	UserLimit int32
}

type ChannelUpdate struct {
	Name  *string
	Topic *string
	// ParentID moves a channel: nil leaves it, "" moves it out of its category.
	ParentID   *string
	Position   *int32
	IsNSFW     *bool
	IsArchived *bool
	UserLimit  *int32
}

func validateUserLimit(kind string, limit *int32) error {
	if limit == nil || *limit == 0 {
		return nil
	}
	if kind != kindVoice {
		return apperr.Invalid("only voice channels have a user limit")
	}
	if *limit < 0 || *limit > maxVoiceUserLimit {
		return apperr.Invalid("user_limit must be between 0 (unlimited) and %d", maxVoiceUserLimit)
	}
	return nil
}

func normalizeChannelName(name *string) error {
	if name == nil {
		return nil
	}
	*name = strings.Join(strings.Fields(*name), " ")
	if *name == "" || utf8.RuneCountInString(*name) > maxChannelNameLen {
		return apperr.Invalid("channel name must be 1-%d characters", maxChannelNameLen)
	}
	return nil
}

func validateChannelTopic(topic *string) error {
	if topic != nil && utf8.RuneCountInString(*topic) > maxChannelTopicLen {
		return apperr.Invalid("topic must be at most %d characters", maxChannelTopicLen)
	}
	return nil
}

// checkParent validates placing a channel of kind under parent.
func (c *chatScope) checkParent(kind string, parentID *uuid.UUID) error {
	if parentID == nil {
		return nil
	}
	if kind == kindCategory {
		return apperr.Invalid("categories must be top-level")
	}
	parent, ok := c.channels[*parentID]
	if !ok || !c.canView(parent.ID) || parent.Kind != kindCategory {
		return apperr.Invalid("parent category not found")
	}
	return nil
}

// emitChannelChange announces a place channel change. Visibility caches are refreshed
// first when invalidate is set, so newly visible channels reach the right people.
func (s *Service) emitChannelChange(ctx context.Context, q *store.Queries, eventType string, ch store.Channel, invalidate bool) error {
	view, err := s.publicChannelView(ctx, q, ch)
	if err != nil {
		return err
	}
	routeID := ch.ID
	if ch.Kind == kindThread {
		routeID = *ch.ParentID
	}
	ev := Event{Type: eventType, Data: view, Places: []uuid.UUID{*ch.PlaceID}, ChannelID: &routeID}
	if invalidate {
		ev.Control = &realtime.Control{PlaceID: ch.PlaceID, Invalidate: true}
		s.queueVoiceSync(ctx, q, *ch.PlaceID)
	}
	s.emit(ctx, q, ev)
	return nil
}

// CreateChannel adds a category, text or voice channel (MANAGE_CHANNELS, in the parent
// category when given).
func (s *Service) CreateChannel(ctx context.Context, p *Principal, ref string, in CreateChannelInput) (ChannelView, error) {
	if in.Kind == "" {
		in.Kind = kindText
	}
	if in.Kind != kindText && in.Kind != kindCategory && in.Kind != kindVoice {
		return ChannelView{}, apperr.Invalid("kind must be text, voice or category")
	}
	if err := errors.Join(normalizeChannelName(&in.Name), validateChannelTopic(&in.Topic), validateUserLimit(in.Kind, &in.UserLimit)); err != nil {
		return ChannelView{}, firstAppErr(err)
	}
	var view ChannelView
	err := s.tx(ctx, func(q *store.Queries) error {
		c, err := s.chat(ctx, q, p, ref)
		if err != nil {
			return err
		}
		if _, err := q.LockPlace(ctx, c.place.ID); err != nil {
			return err
		}
		if !c.canManageUnder(in.ParentID) {
			return missing(permissions.ManageChannels)
		}
		if err := c.checkParent(in.Kind, in.ParentID); err != nil {
			return err
		}
		if len(c.channels) >= maxChannelsPerPlace {
			return apperr.Conflict("a place can have at most %d channels", maxChannelsPerPlace)
		}
		pos, err := q.NextChannelPosition(ctx, store.NextChannelPositionParams{PlaceID: &c.place.ID, ParentID: in.ParentID})
		if err != nil {
			return err
		}
		id, err := uuid.NewV7()
		if err != nil {
			return err
		}
		ch, err := q.CreateChannel(ctx, store.CreateChannelParams{
			ID: id, PlaceID: &c.place.ID, ParentID: in.ParentID, Kind: in.Kind, Name: in.Name, Topic: in.Topic,
			Position: pos, IsNsfw: in.IsNSFW, UserLimit: in.UserLimit,
		})
		if err != nil {
			return err
		}
		if err := s.audit(ctx, q, c.place.ID, p, "channel.create", "channel", &ch.ID, "",
			map[string]any{"name": ch.Name, "kind": ch.Kind}); err != nil {
			return err
		}
		if err := s.emitChannelChange(ctx, q, EventChannelCreate, ch, true); err != nil {
			return err
		}
		c.channels[ch.ID] = ch
		view, err = s.channelView(ctx, q, p, &channelCtx{ch: ch, scope: c, permID: ch.ID, perms: c.perms(ch.ID)})
		return err
	})
	return view, err
}

// UpdateChannel edits a channel. Place channels need MANAGE_CHANNELS; threads can also be
// renamed or archived by their creator or MANAGE_MESSAGES; any recipient may rename a group DM.
func (s *Service) UpdateChannel(ctx context.Context, p *Principal, channelID uuid.UUID, in ChannelUpdate) (ChannelView, error) {
	if err := errors.Join(normalizeChannelName(in.Name), validateChannelTopic(in.Topic)); err != nil {
		return ChannelView{}, firstAppErr(err)
	}
	if in.Position != nil && *in.Position < 0 {
		return ChannelView{}, apperr.Invalid("position must not be negative")
	}
	var view ChannelView
	err := s.tx(ctx, func(q *store.Queries) error {
		cc, err := s.channelFor(ctx, q, p, channelID)
		if err != nil {
			return err
		}
		params := store.UpdateChannelParams{ID: cc.ch.ID, Name: in.Name}
		invalidate := false
		switch cc.ch.Kind {
		case kindDM:
			return apperr.Invalid("direct messages cannot be edited")
		case kindGroupDM:
			if in.Topic != nil || in.ParentID != nil || in.Position != nil || in.IsNSFW != nil || in.IsArchived != nil || in.UserLimit != nil {
				return apperr.Invalid("only the name of a group conversation can be changed")
			}
		case kindThread:
			if in.Topic != nil || in.ParentID != nil || in.Position != nil || in.IsNSFW != nil || in.UserLimit != nil {
				return apperr.Invalid("only a thread's name and archived state can be changed")
			}
			owner := cc.ch.OwnerID != nil && *cc.ch.OwnerID == p.User.ID && cc.has(permissions.SendMessages)
			if !owner && !cc.has(permissions.ManageMessages) && !cc.has(permissions.ManageChannels) {
				return apperr.Forbidden("only the thread's creator or a moderator can change it")
			}
			params.IsArchived = in.IsArchived
		default:
			if in.IsArchived != nil {
				return apperr.Invalid("only threads can be archived")
			}
			if err := validateUserLimit(cc.ch.Kind, in.UserLimit); err != nil {
				return err
			}
			if _, err := q.LockPlace(ctx, cc.scope.place.ID); err != nil {
				return err
			}
			if !cc.has(permissions.ManageChannels) {
				return missing(permissions.ManageChannels)
			}
			params.Topic, params.Position, params.IsNsfw, params.UserLimit = in.Topic, in.Position, in.IsNSFW, in.UserLimit
			if in.ParentID != nil {
				var parent *uuid.UUID
				if *in.ParentID != "" {
					id, err := uuid.Parse(*in.ParentID)
					if err != nil {
						return apperr.Invalid("parent_id must be a UUID or empty")
					}
					parent = &id
				}
				if !uuidPtrEqual(parent, cc.ch.ParentID) {
					if !cc.scope.canManageUnder(parent) {
						return apperr.Forbidden("you cannot move channels there")
					}
					if err := cc.scope.checkParent(cc.ch.Kind, parent); err != nil {
						return err
					}
					params.SetParent, params.ParentID = true, parent
					invalidate = true
				}
			}
		}
		updated, err := q.UpdateChannel(ctx, params)
		if err != nil {
			return err
		}
		cc.ch = updated
		if cc.isDM() {
			pub, err := s.publicChannelView(ctx, q, updated)
			if err != nil {
				return err
			}
			s.emit(ctx, q, cc.event(EventChannelUpdate, pub))
		} else {
			if err := s.emitChannelChange(ctx, q, EventChannelUpdate, updated, invalidate); err != nil {
				return err
			}
			meta := map[string]any{"name": updated.Name}
			setIf(meta, "topic", in.Topic)
			setIf(meta, "position", in.Position)
			setIf(meta, "is_nsfw", in.IsNSFW)
			setIf(meta, "is_archived", in.IsArchived)
			setIf(meta, "parent_id", in.ParentID)
			setIf(meta, "user_limit", in.UserLimit)
			if err := s.audit(ctx, q, cc.scope.place.ID, p, "channel.update", "channel", &updated.ID, "", meta); err != nil {
				return err
			}
		}
		view, err = s.channelView(ctx, q, p, cc)
		return err
	})
	return view, err
}

// DeleteChannel removes a place channel with all its messages and threads (MANAGE_CHANNELS).
// Deleting a category moves its channels to the top level, and deleting a voice channel
// disconnects its participants. Thread creators and MANAGE_MESSAGES may delete threads.
func (s *Service) DeleteChannel(ctx context.Context, p *Principal, channelID uuid.UUID) error {
	return s.tx(ctx, func(q *store.Queries) error {
		cc, err := s.channelFor(ctx, q, p, channelID)
		if err != nil {
			return err
		}
		if cc.isDM() {
			return apperr.Invalid("leave a group conversation by removing yourself from its recipients")
		}
		if _, err := q.LockPlace(ctx, cc.scope.place.ID); err != nil {
			return err
		}
		ch := cc.ch
		if ch.Kind == kindThread {
			owner := ch.OwnerID != nil && *ch.OwnerID == p.User.ID
			if !owner && !cc.has(permissions.ManageMessages) && !cc.has(permissions.ManageChannels) {
				return apperr.Forbidden("only the thread's creator or a moderator can delete it")
			}
		} else if !cc.has(permissions.ManageChannels) {
			return missing(permissions.ManageChannels)
		}
		// Events are published after commit, when the channel no longer resolves, so work out
		// who can see it now and address them directly.
		if ch.Kind == kindThread {
			if err := s.emitChannelChange(ctx, q, EventChannelDelete, ch, false); err != nil {
				return err
			}
		} else {
			members, err := q.ListPlaceMemberIDs(ctx, cc.scope.place.ID)
			if err != nil {
				return err
			}
			viewers, err := s.viewersAmong(ctx, q, &channelCtx{ch: ch, scope: cc.scope, permID: ch.ID}, members)
			if err != nil {
				return err
			}
			pub, err := s.publicChannelView(ctx, q, ch)
			if err != nil {
				return err
			}
			s.emit(ctx, q, Event{Type: EventChannelDelete, Data: pub, Users: viewers})
		}
		var moved []store.Channel
		if ch.Kind == kindCategory {
			for _, child := range cc.scope.ordered {
				if child.ParentID != nil && *child.ParentID == ch.ID {
					moved = append(moved, child)
				}
			}
			if err := q.MoveChildChannelsToRoot(ctx, &ch.ID); err != nil {
				return err
			}
		}
		if err := q.DeleteTargetSubscriptions(ctx, store.DeleteTargetSubscriptionsParams{TargetType: "channel", TargetID: ch.ID}); err != nil {
			return err
		}
		if ch.Kind == kindVoice {
			if err := s.voiceChannelDeleted(ctx, q, ch); err != nil {
				return err
			}
		}
		if err := q.DeleteChannel(ctx, ch.ID); err != nil {
			return err
		}
		if ch.Kind != kindThread {
			s.emitPermissionsChanged(ctx, q, cc.scope.place.ID)
		}
		for _, child := range moved {
			child.ParentID = nil
			if err := s.emitChannelChange(ctx, q, EventChannelUpdate, child, false); err != nil {
				return err
			}
		}
		return s.audit(ctx, q, cc.scope.place.ID, p, "channel.delete", "channel", &ch.ID, "",
			map[string]any{"name": ch.Name, "kind": ch.Kind})
	})
}

// placeChannel resolves a category, text or voice channel the caller can see.
func (s *Service) placeChannel(ctx context.Context, q *store.Queries, p *Principal, channelID uuid.UUID) (*channelCtx, error) {
	cc, err := s.channelFor(ctx, q, p, channelID)
	if err != nil {
		return nil, err
	}
	if cc.ch.Kind != kindText && cc.ch.Kind != kindCategory && cc.ch.Kind != kindVoice {
		return nil, apperr.Invalid("only categories, text and voice channels have permission overwrites")
	}
	return cc, nil
}

// ListChannelOverwrites returns a channel's own overwrites (MANAGE_CHANNELS).
func (s *Service) ListChannelOverwrites(ctx context.Context, p *Principal, channelID uuid.UUID) ([]permissions.Overwrite, error) {
	cc, err := s.placeChannel(ctx, s.q, p, channelID)
	if err != nil {
		return nil, err
	}
	if !cc.has(permissions.ManageChannels) {
		return nil, missing(permissions.ManageChannels)
	}
	ows := cc.scope.overwrites[cc.ch.ID]
	if ows == nil {
		ows = []permissions.Overwrite{}
	}
	return ows, nil
}

func (s *Service) overwriteChannelGuard(ctx context.Context, q *store.Queries, p *Principal, channelID, roleID uuid.UUID) (*channelCtx, store.Role, error) {
	cc, err := s.placeChannel(ctx, q, p, channelID)
	if err != nil {
		return nil, store.Role{}, err
	}
	if _, err := q.LockPlace(ctx, cc.scope.place.ID); err != nil {
		return nil, store.Role{}, err
	}
	if !cc.has(permissions.ManageChannels) {
		return nil, store.Role{}, missing(permissions.ManageChannels)
	}
	role, err := q.GetRole(ctx, store.GetRoleParams{ID: roleID, PlaceID: cc.scope.place.ID})
	if err != nil {
		return nil, role, notFound(err, "role not found")
	}
	if !role.IsDefault && !cc.scope.acc.Member.CanManageRoleAt(role.Position) {
		return nil, role, apperr.Forbidden("you can only set overwrites for roles ranked below your highest role")
	}
	return cc, role, nil
}

// SetChannelOverwrite allows or denies permissions for a role within a category, text or
// voice channel: chat permissions on text channels, voice permissions on voice channels,
// and both on categories. Callers may only set bits they hold in that channel.
func (s *Service) SetChannelOverwrite(ctx context.Context, p *Principal, channelID, roleID uuid.UUID, allow, deny int64) (permissions.Overwrite, error) {
	a, d := permissions.Permission(allow), permissions.Permission(deny)
	if (a|d)&^(permissions.Chat|permissions.Voice) != 0 {
		return permissions.Overwrite{}, apperr.Invalid("overwrites may only contain channel permissions: %s",
			strings.Join(permissions.Names(permissions.Chat|permissions.Voice), ", "))
	}
	if a&d != 0 {
		return permissions.Overwrite{}, apperr.Invalid("a permission cannot be both allowed and denied")
	}
	var out permissions.Overwrite
	err := s.tx(ctx, func(q *store.Queries) error {
		cc, role, err := s.overwriteChannelGuard(ctx, q, p, channelID, roleID)
		if err != nil {
			return err
		}
		if allowed := overwritable(cc.ch.Kind); (a|d)&^allowed != 0 {
			return apperr.Invalid("overwrites on %s channels may only contain: %s", cc.ch.Kind,
				strings.Join(permissions.Names(allowed), ", "))
		}
		if (a|d)&^cc.perms != 0 {
			return apperr.Forbidden("you cannot allow or deny permissions you do not have in this channel")
		}
		row, err := q.UpsertChannelOverwrite(ctx, store.UpsertChannelOverwriteParams{
			ChannelID: cc.ch.ID, RoleID: role.ID, PlaceID: cc.scope.place.ID, Allow: allow, Deny: deny,
		})
		if err != nil {
			return err
		}
		out = permissions.Overwrite{RoleID: row.RoleID, Allow: permissions.Permission(row.Allow), Deny: permissions.Permission(row.Deny)}
		if err := s.emitChannelChange(ctx, q, EventChannelUpdate, cc.ch, true); err != nil {
			return err
		}
		return s.audit(ctx, q, cc.scope.place.ID, p, "channel.overwrite_update", "channel", &cc.ch.ID, "",
			map[string]any{"role_id": role.ID, "allow": allow, "deny": deny})
	})
	return out, err
}

func (s *Service) DeleteChannelOverwrite(ctx context.Context, p *Principal, channelID, roleID uuid.UUID) error {
	return s.tx(ctx, func(q *store.Queries) error {
		cc, role, err := s.overwriteChannelGuard(ctx, q, p, channelID, roleID)
		if err != nil {
			return err
		}
		n, err := q.DeleteChannelOverwrite(ctx, store.DeleteChannelOverwriteParams{ChannelID: cc.ch.ID, RoleID: role.ID})
		if err != nil {
			return err
		}
		if n == 0 {
			return apperr.NotFound("overwrite not found")
		}
		if err := s.emitChannelChange(ctx, q, EventChannelUpdate, cc.ch, true); err != nil {
			return err
		}
		return s.audit(ctx, q, cc.scope.place.ID, p, "channel.overwrite_delete", "channel", &cc.ch.ID, "",
			map[string]any{"role_id": role.ID})
	})
}

type CreateThreadInput struct {
	Name      string
	MessageID *uuid.UUID
}

// CreateThread starts a thread under a text channel, optionally from one of its messages.
// It needs SEND_MESSAGES in the channel.
func (s *Service) CreateThread(ctx context.Context, p *Principal, channelID uuid.UUID, in CreateThreadInput) (ChannelView, error) {
	var view ChannelView
	err := s.tx(ctx, func(q *store.Queries) error {
		cc, err := s.channelFor(ctx, q, p, channelID)
		if err != nil {
			return err
		}
		if cc.ch.Kind != kindText {
			return apperr.Invalid("threads can only be started in text channels")
		}
		if err := cc.require(permissions.SendMessages); err != nil {
			return err
		}
		var starter *store.Message
		if in.MessageID != nil {
			m, err := q.GetMessage(ctx, *in.MessageID)
			if err != nil || m.ChannelID != cc.ch.ID {
				return apperr.Invalid("message not found in this channel")
			}
			starter = &m
		}
		name := in.Name
		if strings.TrimSpace(name) == "" && starter != nil {
			name = excerpt(starter.Content)
			if utf8.RuneCountInString(name) > maxChannelNameLen {
				name = string([]rune(name)[:maxChannelNameLen-1]) + "…"
			}
		}
		if err := normalizeChannelName(&name); err != nil {
			return err
		}
		id, err := uuid.NewV7()
		if err != nil {
			return err
		}
		th, err := q.CreateChannel(ctx, store.CreateChannelParams{
			ID: id, PlaceID: cc.ch.PlaceID, ParentID: &cc.ch.ID, Kind: kindThread, Name: name,
			IsNsfw: cc.ch.IsNsfw, OwnerID: &p.User.ID, ThreadMessageID: in.MessageID,
		})
		if database.IsUniqueViolation(err, "channels_thread_message_key") {
			return apperr.Conflict("that message already has a thread")
		}
		if err != nil {
			return err
		}
		if err := s.emitChannelChange(ctx, q, EventChannelCreate, th, false); err != nil {
			return err
		}
		if starter != nil {
			mv, err := s.messageViews(ctx, q, uuid.Nil, []store.Message{*starter})
			if err != nil {
				return err
			}
			s.emit(ctx, q, cc.event(EventMessageUpdate, mv[0]))
		}
		view, err = s.channelView(ctx, q, p, &channelCtx{ch: th, scope: cc.scope, permID: cc.ch.ID, perms: cc.perms})
		return err
	})
	return view, err
}

// ListThreads lists a text channel's threads, most recently active first.
func (s *Service) ListThreads(ctx context.Context, p *Principal, channelID uuid.UUID, archived bool, page Pagination) ([]ChannelView, error) {
	cc, err := s.channelFor(ctx, s.q, p, channelID)
	if err != nil {
		return nil, err
	}
	if cc.ch.Kind != kindText {
		return nil, apperr.Invalid("only text channels have threads")
	}
	page = page.normalized()
	threads, err := s.q.ListThreads(ctx, store.ListThreadsParams{ParentID: &cc.ch.ID, Archived: archived, Lim: page.Limit, Off: page.Offset})
	if err != nil {
		return nil, err
	}
	return s.channelViews(ctx, s.q, p, threads, func(store.Channel) permissions.Permission { return cc.perms })
}

// SetChannelSubscription mutes ("muted") or unmutes ("normal") mention and direct-message
// notifications from a channel.
func (s *Service) SetChannelSubscription(ctx context.Context, p *Principal, channelID uuid.UUID, level string) error {
	if level != "normal" && level != "muted" {
		return apperr.Invalid("level must be normal or muted")
	}
	cc, err := s.channelFor(ctx, s.q, p, channelID)
	if err != nil {
		return err
	}
	if cc.ch.Kind == kindCategory {
		return apperr.Invalid("mute the channels in a category, or the whole place, instead")
	}
	if cc.ch.Kind == kindVoice {
		return apperr.Invalid("voice channels do not send notifications")
	}
	return s.setSubscription(ctx, s.q, p, cc.placeID(), "channel", cc.ch.ID, level)
}
