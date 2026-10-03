package service

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/parkerbrown98/gotalk-server/internal/store"
)

// Methods used by the real-time gateway.

// UserPlaceIDs lists the places a user belongs to.
func (s *Service) UserPlaceIDs(ctx context.Context, userID uuid.UUID) ([]uuid.UUID, error) {
	return s.q.ListUserPlaceIDs(ctx, userID)
}

// VisibleChannels returns, per user, the categories and text channels of a place they can
// view. Non-members (and every user, once the place is deleted) get an empty set.
func (s *Service) VisibleChannels(ctx context.Context, placeID uuid.UUID, userIDs []uuid.UUID) (map[uuid.UUID]map[uuid.UUID]bool, error) {
	out := make(map[uuid.UUID]map[uuid.UUID]bool, len(userIDs))
	for _, u := range userIDs {
		out[u] = map[uuid.UUID]bool{}
	}
	place, err := s.q.GetPlaceByID(ctx, placeID)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	c, err := s.loadChannels(ctx, s.q, place)
	if err != nil {
		return nil, err
	}
	st, err := s.standings(ctx, s.q, place, c.defaultRole, userIDs)
	if err != nil {
		return nil, err
	}
	for u, m := range st {
		for _, ch := range c.ordered {
			if c.visibleTo(m, ch.ID) {
				out[u][ch.ID] = true
			}
		}
	}
	return out, nil
}

// PresenceAudience lists the places and direct-message partners that should hear about a
// user's presence.
func (s *Service) PresenceAudience(ctx context.Context, userID uuid.UUID) (places, users []uuid.UUID, err error) {
	if places, err = s.q.ListUserPlaceIDs(ctx, userID); err != nil {
		return nil, nil, err
	}
	users, err = s.q.ListDMPartnerIDs(ctx, userID)
	return places, users, err
}

// PresenceVisible filters userIDs to those whose presence the caller may see: people who
// share a place or a conversation with them.
func (s *Service) PresenceVisible(ctx context.Context, p *Principal, userIDs []uuid.UUID) ([]uuid.UUID, error) {
	if len(userIDs) == 0 {
		return []uuid.UUID{}, nil
	}
	return s.q.FilterPresenceAudience(ctx, store.FilterPresenceAudienceParams{ViewerID: p.User.ID, UserIds: dedupe(userIDs)})
}
