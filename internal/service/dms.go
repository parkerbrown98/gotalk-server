package service

import (
	"context"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/parkerbrown98/gotalk-server/internal/apperr"
	"github.com/parkerbrown98/gotalk-server/internal/database"
	"github.com/parkerbrown98/gotalk-server/internal/permissions"
	"github.com/parkerbrown98/gotalk-server/internal/store"
)

// maxGroupRecipients includes the creator.
const maxGroupRecipients = 10

// RecipientEvent announces someone joining a group conversation.
type RecipientEvent struct {
	ChannelID uuid.UUID
	User      store.User
}

func dmKey(a, b uuid.UUID) string {
	if strings.Compare(a.String(), b.String()) > 0 {
		a, b = b, a
	}
	return a.String() + ":" + b.String()
}

func dmPerms(store.Channel) permissions.Permission { return dmPermissions }

// ListDirectChannels lists the caller's direct and group conversations, most recently
// active first.
func (s *Service) ListDirectChannels(ctx context.Context, p *Principal, page Pagination) ([]ChannelView, error) {
	page = page.normalized()
	chans, err := s.q.ListUserDMChannels(ctx, store.ListUserDMChannelsParams{UserID: p.User.ID, Lim: page.Limit, Off: page.Offset})
	if err != nil {
		return nil, err
	}
	return s.channelViews(ctx, s.q, p, chans, dmPerms)
}

type OpenDMInput struct {
	RecipientIDs []uuid.UUID
	// Name only applies to group conversations.
	Name string
}

// OpenDirectChannel returns the caller's conversation with one other user, creating it if
// needed, or starts a new group conversation with several. Recipients must share a place
// with the caller (instance admins are exempt).
func (s *Service) OpenDirectChannel(ctx context.Context, p *Principal, in OpenDMInput) (ChannelView, error) {
	ids := slicesFilter(dedupe(in.RecipientIDs), func(id uuid.UUID) bool { return id != p.User.ID })
	switch {
	case len(ids) == 0:
		return ChannelView{}, apperr.Invalid("recipient_ids must include someone other than yourself")
	case len(ids) >= maxGroupRecipients:
		return ChannelView{}, apperr.Invalid("a group conversation can have at most %d people", maxGroupRecipients)
	}
	group := len(ids) > 1
	if in.Name != "" {
		if !group {
			return ChannelView{}, apperr.Invalid("only group conversations have names")
		}
		if err := normalizeChannelName(&in.Name); err != nil {
			return ChannelView{}, err
		}
	}

	var view ChannelView
	err := s.tx(ctx, func(q *store.Queries) error {
		for _, id := range ids {
			if err := s.canMessageUser(ctx, q, p, id); err != nil {
				return err
			}
		}
		params := store.CreateChannelParams{Kind: kindGroupDM, Name: in.Name, OwnerID: &p.User.ID}
		if !group {
			key := dmKey(p.User.ID, ids[0])
			if existing, err := q.GetDMChannelByKey(ctx, &key); err == nil {
				views, err := s.channelViews(ctx, q, p, []store.Channel{existing}, dmPerms)
				if err != nil {
					return err
				}
				view = views[0]
				return nil
			}
			params = store.CreateChannelParams{Kind: kindDM, DmKey: &key}
		}
		id, err := uuid.NewV7()
		if err != nil {
			return err
		}
		params.ID = id
		ch, err := q.CreateChannel(ctx, params)
		if database.IsUniqueViolation(err, "channels_dm_key") {
			return apperr.Conflict("the conversation was just created; try again")
		}
		if err != nil {
			return err
		}
		for _, u := range append([]uuid.UUID{p.User.ID}, ids...) {
			if _, err := q.AddChannelRecipient(ctx, store.AddChannelRecipientParams{ChannelID: ch.ID, UserID: u}); err != nil {
				return err
			}
		}
		pub, err := s.publicChannelView(ctx, q, ch)
		if err != nil {
			return err
		}
		s.emit(ctx, q, Event{Type: EventChannelCreate, Data: pub, Users: append([]uuid.UUID{p.User.ID}, ids...)})
		views, err := s.channelViews(ctx, q, p, []store.Channel{ch}, dmPerms)
		if err != nil {
			return err
		}
		view = views[0]
		return nil
	})
	return view, err
}

// groupChannel resolves a group conversation the caller belongs to, locked for update.
func (s *Service) groupChannel(ctx context.Context, q *store.Queries, p *Principal, channelID uuid.UUID) (*channelCtx, error) {
	ch, err := q.GetChannelForUpdate(ctx, channelID)
	if err != nil {
		return nil, notFound(err, "channel not found")
	}
	cc, err := s.channelCtxOf(ctx, q, p, ch)
	if err != nil {
		return nil, err
	}
	if ch.Kind != kindGroupDM {
		return nil, apperr.Invalid("only group conversations have changeable recipients")
	}
	return cc, nil
}

// AddRecipient adds someone the caller shares a place with to a group conversation.
func (s *Service) AddRecipient(ctx context.Context, p *Principal, channelID, userID uuid.UUID) (ChannelView, error) {
	var view ChannelView
	err := s.tx(ctx, func(q *store.Queries) error {
		cc, err := s.groupChannel(ctx, q, p, channelID)
		if err != nil {
			return err
		}
		if slices.Contains(cc.recipients, userID) {
			view, err = s.channelView(ctx, q, p, cc)
			return err
		}
		if len(cc.recipients) >= maxGroupRecipients {
			return apperr.Conflict("a group conversation can have at most %d people", maxGroupRecipients)
		}
		if err := s.canMessageUser(ctx, q, p, userID); err != nil {
			return err
		}
		if _, err := q.AddChannelRecipient(ctx, store.AddChannelRecipientParams{ChannelID: cc.ch.ID, UserID: userID}); err != nil {
			return err
		}
		user, err := q.GetUserByID(ctx, userID)
		if err != nil {
			return err
		}
		cc.recipients = append(cc.recipients, userID)
		s.emit(ctx, q, cc.event(EventRecipientAdd, RecipientEvent{ChannelID: cc.ch.ID, User: user}))
		pub, err := s.publicChannelView(ctx, q, cc.ch)
		if err != nil {
			return err
		}
		s.emit(ctx, q, Event{Type: EventChannelCreate, Data: pub, Users: []uuid.UUID{userID}})
		view, err = s.channelView(ctx, q, p, cc)
		return err
	})
	return view, err
}

// RemoveRecipient leaves a group conversation (userID is the caller) or, for its owner,
// removes someone else. When the owner leaves, the longest-standing recipient takes over;
// the conversation is deleted when the last person leaves.
func (s *Service) RemoveRecipient(ctx context.Context, p *Principal, channelID, userID uuid.UUID) error {
	return s.tx(ctx, func(q *store.Queries) error {
		cc, err := s.groupChannel(ctx, q, p, channelID)
		if err != nil {
			return err
		}
		isOwner := cc.ch.OwnerID != nil && *cc.ch.OwnerID == p.User.ID
		if userID != p.User.ID && !isOwner {
			return apperr.Forbidden("only the conversation's owner can remove other people")
		}
		if !slices.Contains(cc.recipients, userID) {
			return apperr.NotFound("recipient not found")
		}
		if _, err := q.RemoveChannelRecipient(ctx, store.RemoveChannelRecipientParams{ChannelID: cc.ch.ID, UserID: userID}); err != nil {
			return err
		}
		if err := q.DeleteChannelReadsFor(ctx, store.DeleteChannelReadsForParams{ChannelID: cc.ch.ID, UserID: userID}); err != nil {
			return err
		}
		s.emit(ctx, q, cc.event(EventRecipientRemove, map[string]any{"channel_id": cc.ch.ID, "user_id": userID}))
		s.emit(ctx, q, Event{Type: EventChannelDelete, Users: []uuid.UUID{userID},
			Data: map[string]any{"id": cc.ch.ID, "kind": cc.ch.Kind}})
		remaining := slicesFilter(cc.recipients, func(id uuid.UUID) bool { return id != userID })
		if len(remaining) == 0 {
			return q.DeleteChannel(ctx, cc.ch.ID)
		}
		if cc.ch.OwnerID != nil && *cc.ch.OwnerID == userID {
			// Recipients are ordered by join time.
			updated, err := q.UpdateChannel(ctx, store.UpdateChannelParams{ID: cc.ch.ID, OwnerID: &remaining[0]})
			if err != nil {
				return err
			}
			pub, err := s.publicChannelView(ctx, q, updated)
			if err != nil {
				return err
			}
			s.emit(ctx, q, Event{Type: EventChannelUpdate, Data: pub, Users: remaining})
		}
		return nil
	})
}
