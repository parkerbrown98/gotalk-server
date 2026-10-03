package service

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/parkerbrown98/gotalk-server/internal/apperr"
	"github.com/parkerbrown98/gotalk-server/internal/database"
	"github.com/parkerbrown98/gotalk-server/internal/permissions"
	"github.com/parkerbrown98/gotalk-server/internal/store"
)

const DefaultRoleName = "@everyone"

// Access is the caller's standing in a place.
type Access struct {
	IsMember bool
	Member   permissions.Member
}

// PlaceView is a place plus the caller's effective permissions (nil when not a member).
type PlaceView struct {
	Place       store.Place
	Permissions *permissions.Permission
}

func (s *Service) resolvePlace(ctx context.Context, q *store.Queries, ref string) (store.Place, error) {
	var (
		place store.Place
		err   error
	)
	if id, perr := uuid.Parse(ref); perr == nil {
		place, err = q.GetPlaceByID(ctx, id)
	} else {
		place, err = q.GetPlaceBySlug(ctx, ref)
	}
	return place, notFound(err, "place not found")
}

func (s *Service) access(ctx context.Context, q *store.Queries, place store.Place, userID uuid.UUID) (Access, error) {
	isMember, err := q.IsMember(ctx, store.IsMemberParams{PlaceID: place.ID, UserID: userID})
	if err != nil || !isMember {
		return Access{}, err
	}
	m, err := s.memberStanding(ctx, q, place, userID)
	return Access{IsMember: true, Member: m}, err
}

func (s *Service) memberStanding(ctx context.Context, q *store.Queries, place store.Place, userID uuid.UUID) (permissions.Member, error) {
	rows, err := q.GetMemberRolesForPermissions(ctx, store.GetMemberRolesForPermissionsParams{PlaceID: place.ID, UserID: userID})
	if err != nil {
		return permissions.Member{}, err
	}
	m := permissions.Member{IsOwner: place.OwnerID == userID, RoleIDs: make([]uuid.UUID, 0, len(rows))}
	for _, r := range rows {
		m.Raw |= permissions.Permission(r.Permissions)
		m.TopPosition = max(m.TopPosition, r.Position)
		m.RoleIDs = append(m.RoleIDs, r.ID)
		if r.IsDefault {
			m.DefaultRoleID = r.ID
		}
	}
	until, err := q.GetMemberTimeout(ctx, store.GetMemberTimeoutParams{PlaceID: place.ID, UserID: userID})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return permissions.Member{}, err
	}
	m.TimedOut = timedOut(until)
	return m, nil
}

func timedOut(until *time.Time) bool { return until != nil && until.After(time.Now()) }

// placeFor resolves a place the caller can see. Private places are hidden (404) from
// non-members, except instance administrators.
func (s *Service) placeFor(ctx context.Context, q *store.Queries, p *Principal, ref string) (store.Place, Access, error) {
	place, err := s.resolvePlace(ctx, q, ref)
	if err != nil {
		return place, Access{}, err
	}
	var acc Access
	if p != nil {
		if acc, err = s.access(ctx, q, place, p.User.ID); err != nil {
			return place, acc, err
		}
	}
	if place.Visibility == "private" && !acc.IsMember && (p == nil || !p.User.IsInstanceAdmin) {
		return place, acc, apperr.NotFound("place not found")
	}
	return place, acc, nil
}

// requireMember resolves a place and ensures the caller belongs to it.
func (s *Service) requireMember(ctx context.Context, q *store.Queries, p *Principal, ref string) (store.Place, Access, error) {
	place, acc, err := s.placeFor(ctx, q, p, ref)
	if err != nil {
		return place, acc, err
	}
	if !acc.IsMember {
		return place, acc, apperr.Forbidden("you are not a member of this place")
	}
	return place, acc, nil
}

func (s *Service) requirePermission(ctx context.Context, q *store.Queries, p *Principal, ref string, perm permissions.Permission) (store.Place, Access, error) {
	place, acc, err := s.requireMember(ctx, q, p, ref)
	if err != nil {
		return place, acc, err
	}
	if !acc.Member.Has(perm) {
		return place, acc, apperr.Forbidden("missing permission: %s", strings.Join(permissions.Names(perm), ", "))
	}
	return place, acc, nil
}

func viewOf(place store.Place, acc Access) PlaceView {
	v := PlaceView{Place: place}
	if acc.IsMember {
		eff := acc.Member.Effective()
		v.Permissions = &eff
	}
	return v
}

type CreatePlaceInput struct {
	Slug        string
	Name        string
	Description string
	Visibility  string
	IsNSFW      bool
	Locale      string
}

func validateVisibility(v string) error {
	switch v {
	case "public", "invite_only", "private":
		return nil
	}
	return apperr.Invalid("visibility must be one of public, invite_only, private")
}

func (s *Service) CreatePlace(ctx context.Context, p *Principal, in CreatePlaceInput) (PlaceView, error) {
	in.Slug = strings.ToLower(strings.TrimSpace(in.Slug))
	in.Name = strings.TrimSpace(in.Name)
	if in.Visibility == "" {
		in.Visibility = "public"
	}
	if in.Locale == "" {
		in.Locale = "en"
	}
	if in.Name == "" {
		return PlaceView{}, apperr.Invalid("name must not be empty")
	}
	if err := errors.Join(validateSlug(in.Slug), validateVisibility(in.Visibility)); err != nil {
		return PlaceView{}, firstAppErr(err)
	}

	var view PlaceView
	err := s.tx(ctx, func(q *store.Queries) error {
		id, err := uuid.NewV7()
		if err != nil {
			return err
		}
		place, err := q.CreatePlace(ctx, store.CreatePlaceParams{
			ID:          id,
			Slug:        in.Slug,
			Name:        in.Name,
			Description: in.Description,
			Visibility:  in.Visibility,
			IsNsfw:      in.IsNSFW,
			Locale:      in.Locale,
			OwnerID:     p.User.ID,
		})
		if database.IsUniqueViolation(err, "places_slug_key") {
			return apperr.Conflict("slug %q is already in use", in.Slug)
		}
		if err != nil {
			return err
		}
		roleID, err := uuid.NewV7()
		if err != nil {
			return err
		}
		if _, err := q.CreateRole(ctx, store.CreateRoleParams{
			ID:          roleID,
			PlaceID:     place.ID,
			Name:        DefaultRoleName,
			Position:    0,
			Permissions: int64(permissions.Default),
			IsDefault:   true,
		}); err != nil {
			return err
		}
		if err := s.addMember(ctx, q, place.ID, p.User.ID); err != nil {
			return err
		}
		place.MemberCount = 1
		view = viewOf(place, Access{IsMember: true, Member: permissions.Member{IsOwner: true}})
		return nil
	})
	return view, err
}

func (s *Service) GetPlace(ctx context.Context, p *Principal, ref string) (PlaceView, error) {
	place, acc, err := s.placeFor(ctx, s.q, p, ref)
	if err != nil {
		return PlaceView{}, err
	}
	return viewOf(place, acc), nil
}

func (s *Service) DiscoverPlaces(ctx context.Context, query string, page Pagination) ([]store.Place, error) {
	page = page.normalized()
	var qp *string
	if query = strings.TrimSpace(query); query != "" {
		escaped := likeEscaper.Replace(query)
		qp = &escaped
	}
	return s.q.ListDiscoverablePlaces(ctx, store.ListDiscoverablePlacesParams{Query: qp, Lim: page.Limit, Off: page.Offset})
}

// likeEscaper escapes LIKE wildcards so user input is matched literally.
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

func (s *Service) ListMyPlaces(ctx context.Context, p *Principal) ([]store.Place, error) {
	return s.q.ListUserPlaces(ctx, p.User.ID)
}

type PlaceUpdate struct {
	Name        *string
	Description *string
	Slug        *string
	Visibility  *string
	IsNSFW      *bool
	Locale      *string
	IconURL     *string
	BannerURL   *string
}

func (s *Service) UpdatePlace(ctx context.Context, p *Principal, ref string, in PlaceUpdate) (PlaceView, error) {
	place, acc, err := s.requirePermission(ctx, s.q, p, ref, permissions.ManagePlace)
	if err != nil {
		return PlaceView{}, err
	}
	if in.Name != nil {
		trimmed := strings.TrimSpace(*in.Name)
		if trimmed == "" {
			return PlaceView{}, apperr.Invalid("name must not be empty")
		}
		in.Name = &trimmed
	}
	if in.Slug != nil {
		slug := strings.ToLower(strings.TrimSpace(*in.Slug))
		if err := validateSlug(slug); err != nil {
			return PlaceView{}, err
		}
		in.Slug = &slug
	}
	if in.Visibility != nil {
		if err := validateVisibility(*in.Visibility); err != nil {
			return PlaceView{}, err
		}
	}
	if err := errors.Join(validateOptionalURL("icon_url", in.IconURL), validateOptionalURL("banner_url", in.BannerURL)); err != nil {
		return PlaceView{}, firstAppErr(err)
	}
	var updated store.Place
	err = s.tx(ctx, func(q *store.Queries) error {
		var err error
		updated, err = q.UpdatePlace(ctx, store.UpdatePlaceParams{
			ID:          place.ID,
			Name:        in.Name,
			Description: in.Description,
			Slug:        in.Slug,
			Visibility:  in.Visibility,
			IsNsfw:      in.IsNSFW,
			Locale:      in.Locale,
			IconUrl:     in.IconURL,
			BannerUrl:   in.BannerURL,
		})
		if database.IsUniqueViolation(err, "places_slug_key") {
			return apperr.Conflict("slug is already in use")
		}
		if err != nil {
			return notFound(err, "place not found")
		}
		if updated.Visibility != place.Visibility {
			if err := s.refreshPublicBoards(ctx, q, place.ID); err != nil {
				return err
			}
		}
		return s.audit(ctx, q, place.ID, p, "place.update", "place", &place.ID, "", placeChanges(in))
	})
	if err != nil {
		return PlaceView{}, err
	}
	return viewOf(updated, acc), nil
}

func placeChanges(in PlaceUpdate) map[string]any {
	m := map[string]any{}
	setIf(m, "name", in.Name)
	setIf(m, "description", in.Description)
	setIf(m, "slug", in.Slug)
	setIf(m, "visibility", in.Visibility)
	setIf(m, "is_nsfw", in.IsNSFW)
	setIf(m, "locale", in.Locale)
	setIf(m, "icon_url", in.IconURL)
	setIf(m, "banner_url", in.BannerURL)
	return m
}

func (s *Service) DeletePlace(ctx context.Context, p *Principal, ref string) error {
	place, acc, err := s.placeFor(ctx, s.q, p, ref)
	if err != nil {
		return err
	}
	if !acc.Member.IsOwner && !p.User.IsInstanceAdmin {
		return apperr.Forbidden("only the owner can delete a place")
	}
	return s.q.SoftDeletePlace(ctx, place.ID)
}

func (s *Service) TransferOwnership(ctx context.Context, p *Principal, ref string, newOwner uuid.UUID) (PlaceView, error) {
	var view PlaceView
	err := s.tx(ctx, func(q *store.Queries) error {
		place, acc, err := s.requireMember(ctx, q, p, ref)
		if err != nil {
			return err
		}
		if !acc.Member.IsOwner {
			return apperr.Forbidden("only the owner can transfer ownership")
		}
		if newOwner == p.User.ID {
			return apperr.Invalid("you already own this place")
		}
		if _, err := q.LockPlace(ctx, place.ID); err != nil {
			return err
		}
		isMember, err := q.IsMember(ctx, store.IsMemberParams{PlaceID: place.ID, UserID: newOwner})
		if err != nil {
			return err
		}
		if !isMember {
			return apperr.Invalid("the new owner must be a member of the place")
		}
		if err := q.TransferPlaceOwnership(ctx, store.TransferPlaceOwnershipParams{ID: place.ID, OwnerID: newOwner}); err != nil {
			return err
		}
		if err := s.audit(ctx, q, place.ID, p, "place.transfer", "user", &newOwner, "", nil); err != nil {
			return err
		}
		place.OwnerID = newOwner
		standing, err := s.memberStanding(ctx, q, place, p.User.ID)
		if err != nil {
			return err
		}
		view = viewOf(place, Access{IsMember: true, Member: standing})
		return nil
	})
	return view, err
}

func (s *Service) addMember(ctx context.Context, q *store.Queries, placeID, userID uuid.UUID) error {
	n, err := q.AddMember(ctx, store.AddMemberParams{PlaceID: placeID, UserID: userID})
	if err != nil || n == 0 {
		return err
	}
	return q.AdjustMemberCount(ctx, store.AdjustMemberCountParams{ID: placeID, Delta: 1})
}

func (s *Service) removeMember(ctx context.Context, q *store.Queries, placeID, userID uuid.UUID) (bool, error) {
	n, err := q.RemoveMember(ctx, store.RemoveMemberParams{PlaceID: placeID, UserID: userID})
	if err != nil || n == 0 {
		return false, err
	}
	return true, q.AdjustMemberCount(ctx, store.AdjustMemberCountParams{ID: placeID, Delta: -1})
}

// JoinPlace joins a public place. Joining is idempotent.
func (s *Service) JoinPlace(ctx context.Context, p *Principal, ref string) (PlaceView, error) {
	var view PlaceView
	err := s.tx(ctx, func(q *store.Queries) error {
		place, acc, err := s.placeFor(ctx, q, p, ref)
		if err != nil {
			return err
		}
		if acc.IsMember {
			view = viewOf(place, acc)
			return nil
		}
		if place.Visibility != "public" {
			return apperr.Forbidden("this place requires an invite to join")
		}
		banned, err := q.IsBanned(ctx, store.IsBannedParams{PlaceID: place.ID, UserID: p.User.ID})
		if err != nil {
			return err
		}
		if banned {
			return apperr.Forbidden("you are banned from this place")
		}
		if err := s.addMember(ctx, q, place.ID, p.User.ID); err != nil {
			return err
		}
		place.MemberCount++
		acc, err = s.access(ctx, q, place, p.User.ID)
		view = viewOf(place, acc)
		return err
	})
	return view, err
}

func (s *Service) LeavePlace(ctx context.Context, p *Principal, ref string) error {
	return s.tx(ctx, func(q *store.Queries) error {
		place, acc, err := s.requireMember(ctx, q, p, ref)
		if err != nil {
			return err
		}
		if acc.Member.IsOwner {
			return apperr.Conflict("the owner cannot leave; transfer ownership or delete the place instead")
		}
		_, err = s.removeMember(ctx, q, place.ID, p.User.ID)
		return err
	})
}
