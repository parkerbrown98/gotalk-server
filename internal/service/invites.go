package service

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/parkerbrown98/gotalk-server/internal/apperr"
	"github.com/parkerbrown98/gotalk-server/internal/auth"
	"github.com/parkerbrown98/gotalk-server/internal/database"
	"github.com/parkerbrown98/gotalk-server/internal/permissions"
	"github.com/parkerbrown98/gotalk-server/internal/store"

	"github.com/google/uuid"
)

const (
	inviteCodeLen     = 10
	defaultInviteTTL  = 7 * 24 * time.Hour
	maxInviteTTL      = 30 * 24 * time.Hour
	maxInviteMaxUses  = 10000
	inviteCodeRetries = 3
)

type InviteInput struct {
	// MaxUses of 0 means unlimited.
	MaxUses int32
	// MaxAge of 0 means the invite never expires; nil uses the 7 day default.
	MaxAge *time.Duration
}

type InvitePreview struct {
	Invite store.Invite
	Place  store.Place
}

func (s *Service) CreateInvite(ctx context.Context, p *Principal, ref string, in InviteInput) (store.Invite, error) {
	if in.MaxUses < 0 || in.MaxUses > maxInviteMaxUses {
		return store.Invite{}, apperr.Invalid("max_uses must be between 0 (unlimited) and %d", maxInviteMaxUses)
	}
	ttl := defaultInviteTTL
	if in.MaxAge != nil {
		ttl = *in.MaxAge
	}
	if ttl < 0 || ttl > maxInviteTTL {
		return store.Invite{}, apperr.Invalid("max_age must be between 0 (never expires) and %d seconds", int(maxInviteTTL.Seconds()))
	}

	place, _, err := s.requirePermission(ctx, s.q, p, ref, permissions.CreateInvites)
	if err != nil {
		return store.Invite{}, err
	}
	params := store.CreateInviteParams{PlaceID: place.ID, CreatedBy: &p.User.ID}
	if in.MaxUses > 0 {
		params.MaxUses = &in.MaxUses
	}
	if ttl > 0 {
		exp := time.Now().Add(ttl)
		params.ExpiresAt = &exp
	}
	for range inviteCodeRetries {
		if params.Code, err = auth.RandomString(inviteCodeLen, auth.Base62); err != nil {
			return store.Invite{}, err
		}
		invite, err := s.q.CreateInvite(ctx, params)
		if database.IsUniqueViolation(err, "invites_pkey") {
			continue
		}
		return invite, err
	}
	return store.Invite{}, errors.New("could not generate a unique invite code")
}

func (s *Service) ListInvites(ctx context.Context, p *Principal, ref string) ([]store.Invite, error) {
	place, _, err := s.requirePermission(ctx, s.q, p, ref, permissions.ManageInvites)
	if err != nil {
		return nil, err
	}
	return s.q.ListInvites(ctx, place.ID)
}

// DeleteInvite revokes an invite. Its creator may always revoke it; others need MANAGE_INVITES.
func (s *Service) DeleteInvite(ctx context.Context, p *Principal, ref, code string) error {
	place, acc, err := s.requireMember(ctx, s.q, p, ref)
	if err != nil {
		return err
	}
	invite, err := s.q.GetInvite(ctx, code)
	if err != nil || invite.PlaceID != place.ID {
		return apperr.NotFound("invite not found")
	}
	isCreator := invite.CreatedBy != nil && *invite.CreatedBy == p.User.ID
	if !isCreator && !acc.Member.Has(permissions.ManageInvites) {
		return apperr.Forbidden("missing permission: MANAGE_INVITES")
	}
	_, err = s.q.DeleteInvite(ctx, store.DeleteInviteParams{Code: code, PlaceID: place.ID})
	return err
}

func inviteUsable(inv store.Invite) bool {
	if inv.ExpiresAt != nil && time.Now().After(*inv.ExpiresAt) {
		return false
	}
	return inv.MaxUses == nil || inv.Uses < *inv.MaxUses
}

// PreviewInvite returns the invite and its place, even for private places, so a client
// can show what the user is about to join.
func (s *Service) PreviewInvite(ctx context.Context, code string) (InvitePreview, error) {
	invite, err := s.q.GetInvite(ctx, code)
	if err != nil {
		return InvitePreview{}, notFound(err, "invite not found")
	}
	if !inviteUsable(invite) {
		return InvitePreview{}, apperr.NotFound("invite has expired or reached its use limit")
	}
	place, err := s.q.GetPlaceByID(ctx, invite.PlaceID)
	if err != nil {
		return InvitePreview{}, notFound(err, "invite not found")
	}
	return InvitePreview{Invite: invite, Place: place}, nil
}

// redeemInvite joins userID to the invite's place, consuming one use unless the user is
// already a member.
func (s *Service) redeemInvite(ctx context.Context, q *store.Queries, userID uuid.UUID, code string) (store.Place, error) {
	invite, err := q.GetInvite(ctx, code)
	if err != nil {
		return store.Place{}, notFound(err, "invite not found")
	}
	place, err := q.GetPlaceByID(ctx, invite.PlaceID)
	if err != nil {
		return store.Place{}, notFound(err, "invite not found")
	}
	isMember, err := q.IsMember(ctx, store.IsMemberParams{PlaceID: place.ID, UserID: userID})
	if err != nil || isMember {
		return place, err
	}
	banned, err := q.IsBanned(ctx, store.IsBannedParams{PlaceID: place.ID, UserID: userID})
	if err != nil {
		return place, err
	}
	if banned {
		return place, apperr.Forbidden("you are banned from this place")
	}
	if _, err := q.ConsumeInvite(ctx, code); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return place, apperr.NotFound("invite has expired or reached its use limit")
		}
		return place, err
	}
	if err := s.addMember(ctx, q, place.ID, userID); err != nil {
		return place, err
	}
	place.MemberCount++
	return place, nil
}

func (s *Service) AcceptInvite(ctx context.Context, p *Principal, code string) (PlaceView, error) {
	var view PlaceView
	err := s.tx(ctx, func(q *store.Queries) error {
		place, err := s.redeemInvite(ctx, q, p.User.ID, code)
		if err != nil {
			return err
		}
		acc, err := s.access(ctx, q, place, p.User.ID)
		view = viewOf(place, acc)
		return err
	})
	return view, err
}
