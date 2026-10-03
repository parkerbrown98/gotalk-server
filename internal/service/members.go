package service

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/parkerbrown98/gotalk-server/internal/apperr"
	"github.com/parkerbrown98/gotalk-server/internal/permissions"
	"github.com/parkerbrown98/gotalk-server/internal/store"
)

type MemberView struct {
	Member  store.PlaceMember
	User    store.User
	RoleIDs []uuid.UUID
}

func (s *Service) attachRoles(ctx context.Context, q *store.Queries, placeID uuid.UUID, rows []MemberView) ([]MemberView, error) {
	if len(rows) == 0 {
		return rows, nil
	}
	ids := make([]uuid.UUID, len(rows))
	index := make(map[uuid.UUID]int, len(rows))
	for i, r := range rows {
		ids[i] = r.User.ID
		index[r.User.ID] = i
		rows[i].RoleIDs = []uuid.UUID{}
	}
	assigned, err := q.ListMemberRoleIDs(ctx, store.ListMemberRoleIDsParams{PlaceID: placeID, UserIds: ids})
	if err != nil {
		return nil, err
	}
	for _, a := range assigned {
		i := index[a.UserID]
		rows[i].RoleIDs = append(rows[i].RoleIDs, a.RoleID)
	}
	return rows, nil
}

func (s *Service) ListMembers(ctx context.Context, p *Principal, ref, query string, page Pagination) ([]MemberView, error) {
	place, _, err := s.requireMember(ctx, s.q, p, ref)
	if err != nil {
		return nil, err
	}
	page = page.normalized()
	var qp *string
	if query = strings.TrimPrefix(strings.TrimSpace(query), "@"); query != "" {
		escaped := likeEscaper.Replace(query)
		qp = &escaped
	}
	rows, err := s.q.ListMembers(ctx, store.ListMembersParams{PlaceID: place.ID, Query: qp, Lim: page.Limit, Off: page.Offset})
	if err != nil {
		return nil, err
	}
	views := make([]MemberView, len(rows))
	for i, r := range rows {
		views[i] = MemberView{Member: r.PlaceMember, User: r.User}
	}
	return s.attachRoles(ctx, s.q, place.ID, views)
}

func (s *Service) getMember(ctx context.Context, q *store.Queries, placeID, userID uuid.UUID) (MemberView, error) {
	row, err := q.GetMember(ctx, store.GetMemberParams{PlaceID: placeID, UserID: userID})
	if err != nil {
		return MemberView{}, notFound(err, "member not found")
	}
	views, err := s.attachRoles(ctx, q, placeID, []MemberView{{Member: row.PlaceMember, User: row.User}})
	if err != nil {
		return MemberView{}, err
	}
	return views[0], nil
}

func (s *Service) GetMember(ctx context.Context, p *Principal, ref string, userID uuid.UUID) (MemberView, error) {
	place, _, err := s.requireMember(ctx, s.q, p, ref)
	if err != nil {
		return MemberView{}, err
	}
	return s.getMember(ctx, s.q, place.ID, userID)
}

// targetStanding loads a target member's standing, failing with 404 if they are not a member.
func (s *Service) targetStanding(ctx context.Context, q *store.Queries, place store.Place, userID uuid.UUID) (permissions.Member, error) {
	isMember, err := q.IsMember(ctx, store.IsMemberParams{PlaceID: place.ID, UserID: userID})
	if err != nil {
		return permissions.Member{}, err
	}
	if !isMember {
		return permissions.Member{}, apperr.NotFound("member not found")
	}
	return s.memberStanding(ctx, q, place, userID)
}

// SetNickname changes a member's nickname. An empty nickname clears it. Members need
// CHANGE_NICKNAME for themselves, or MANAGE_NICKNAMES and a higher rank for others.
func (s *Service) SetNickname(ctx context.Context, p *Principal, ref string, userID uuid.UUID, nickname string) (MemberView, error) {
	nickname = strings.TrimSpace(nickname)
	if len(nickname) > 32 {
		return MemberView{}, apperr.Invalid("nickname must be at most 32 characters")
	}
	var view MemberView
	err := s.tx(ctx, func(q *store.Queries) error {
		place, acc, err := s.requireMember(ctx, q, p, ref)
		if err != nil {
			return err
		}
		if userID == p.User.ID {
			if !acc.Member.Has(permissions.ChangeNickname) && !acc.Member.Has(permissions.ManageNicknames) {
				return apperr.Forbidden("missing permission: CHANGE_NICKNAME")
			}
		} else {
			if !acc.Member.Has(permissions.ManageNicknames) {
				return apperr.Forbidden("missing permission: MANAGE_NICKNAMES")
			}
			target, err := s.targetStanding(ctx, q, place, userID)
			if err != nil {
				return err
			}
			if !acc.Member.Outranks(target) {
				return apperr.Forbidden("you can only change nicknames of members ranked below you")
			}
		}
		if err := q.UpdateMemberNickname(ctx, store.UpdateMemberNicknameParams{PlaceID: place.ID, UserID: userID, Nickname: nickname}); err != nil {
			return err
		}
		if userID != p.User.ID {
			if err := s.audit(ctx, q, place.ID, p, "member.nickname", "user", &userID, "", map[string]any{"nickname": nickname}); err != nil {
				return err
			}
		}
		view, err = s.getMember(ctx, q, place.ID, userID)
		return err
	})
	return view, err
}

func (s *Service) KickMember(ctx context.Context, p *Principal, ref string, userID uuid.UUID, reason string) error {
	if userID == p.User.ID {
		return apperr.Invalid("use the leave endpoint to leave a place")
	}
	if len([]rune(reason)) > maxReasonLen {
		return apperr.Invalid("reason must be at most %d characters", maxReasonLen)
	}
	return s.tx(ctx, func(q *store.Queries) error {
		place, acc, err := s.requirePermission(ctx, q, p, ref, permissions.KickMembers)
		if err != nil {
			return err
		}
		target, err := s.targetStanding(ctx, q, place, userID)
		if err != nil {
			return err
		}
		if !acc.Member.Outranks(target) {
			return apperr.Forbidden("you can only kick members ranked below you")
		}
		if _, err = s.removeMember(ctx, q, place.ID, userID); err != nil {
			return err
		}
		return s.audit(ctx, q, place.ID, p, "member.kick", "user", &userID, reason, nil)
	})
}

// roleChangeGuard enforces that a role can be (un)assigned by the caller.
func (s *Service) roleChangeGuard(ctx context.Context, q *store.Queries, p *Principal, ref string, userID, roleID uuid.UUID) (store.Place, store.Role, error) {
	place, acc, err := s.requirePermission(ctx, q, p, ref, permissions.ManageRoles)
	if err != nil {
		return place, store.Role{}, err
	}
	role, err := q.GetRole(ctx, store.GetRoleParams{ID: roleID, PlaceID: place.ID})
	if err != nil {
		return place, role, notFound(err, "role not found")
	}
	if role.IsDefault {
		return place, role, apperr.Invalid("the %s role is implicit and cannot be assigned", DefaultRoleName)
	}
	if !acc.Member.CanManageRoleAt(role.Position) {
		return place, role, apperr.Forbidden("you can only assign roles ranked below your highest role")
	}
	if _, err := s.targetStanding(ctx, q, place, userID); err != nil {
		return place, role, err
	}
	return place, role, nil
}

func (s *Service) AssignRole(ctx context.Context, p *Principal, ref string, userID, roleID uuid.UUID) (MemberView, error) {
	var view MemberView
	err := s.tx(ctx, func(q *store.Queries) error {
		place, role, err := s.roleChangeGuard(ctx, q, p, ref, userID, roleID)
		if err != nil {
			return err
		}
		if err := q.AddMemberRole(ctx, store.AddMemberRoleParams{PlaceID: place.ID, UserID: userID, RoleID: role.ID}); err != nil {
			return err
		}
		s.emitPermissionsChanged(ctx, q, place.ID, userID)
		if err := s.audit(ctx, q, place.ID, p, "member.role_add", "user", &userID, "",
			map[string]any{"role_id": role.ID, "role_name": role.Name}); err != nil {
			return err
		}
		view, err = s.getMember(ctx, q, place.ID, userID)
		return err
	})
	return view, err
}

func (s *Service) UnassignRole(ctx context.Context, p *Principal, ref string, userID, roleID uuid.UUID) (MemberView, error) {
	var view MemberView
	err := s.tx(ctx, func(q *store.Queries) error {
		place, role, err := s.roleChangeGuard(ctx, q, p, ref, userID, roleID)
		if err != nil {
			return err
		}
		if _, err := q.RemoveMemberRole(ctx, store.RemoveMemberRoleParams{PlaceID: place.ID, UserID: userID, RoleID: role.ID}); err != nil {
			return err
		}
		s.emitPermissionsChanged(ctx, q, place.ID, userID)
		if err := s.audit(ctx, q, place.ID, p, "member.role_remove", "user", &userID, "",
			map[string]any{"role_id": role.ID, "role_name": role.Name}); err != nil {
			return err
		}
		view, err = s.getMember(ctx, q, place.ID, userID)
		return err
	})
	return view, err
}

func (s *Service) ListBans(ctx context.Context, p *Principal, ref string, page Pagination) ([]store.ListBansRow, error) {
	place, _, err := s.requirePermission(ctx, s.q, p, ref, permissions.BanMembers)
	if err != nil {
		return nil, err
	}
	page = page.normalized()
	return s.q.ListBans(ctx, store.ListBansParams{PlaceID: place.ID, Lim: page.Limit, Off: page.Offset})
}

// BanUser bans a user (member or not) and removes their membership. A positive duration
// makes the ban temporary; zero bans permanently.
func (s *Service) BanUser(ctx context.Context, p *Principal, ref string, userID uuid.UUID, reason string, duration time.Duration) error {
	if userID == p.User.ID {
		return apperr.Invalid("you cannot ban yourself")
	}
	if len(reason) > maxReasonLen {
		return apperr.Invalid("reason must be at most %d characters", maxReasonLen)
	}
	if duration < 0 || duration > maxBanDuration {
		return apperr.Invalid("duration must be between 0 (permanent) and %d seconds", int(maxBanDuration.Seconds()))
	}
	return s.tx(ctx, func(q *store.Queries) error {
		place, acc, err := s.requirePermission(ctx, q, p, ref, permissions.BanMembers)
		if err != nil {
			return err
		}
		if _, err := q.GetUserByID(ctx, userID); err != nil {
			return notFound(err, "user not found")
		}
		isMember, err := q.IsMember(ctx, store.IsMemberParams{PlaceID: place.ID, UserID: userID})
		if err != nil {
			return err
		}
		if isMember {
			target, err := s.memberStanding(ctx, q, place, userID)
			if err != nil {
				return err
			}
			if !acc.Member.Outranks(target) {
				return apperr.Forbidden("you can only ban members ranked below you")
			}
		}
		actor := p.User.ID
		var expires *time.Time
		meta := map[string]any{}
		if duration > 0 {
			exp := time.Now().Add(duration).UTC().Truncate(time.Second)
			expires = &exp
			meta["expires_at"] = exp
		}
		if err := q.UpsertBan(ctx, store.UpsertBanParams{PlaceID: place.ID, UserID: userID, Reason: reason, BannedBy: &actor, ExpiresAt: expires}); err != nil {
			return err
		}
		if _, err := s.removeMember(ctx, q, place.ID, userID); err != nil {
			return err
		}
		return s.audit(ctx, q, place.ID, p, "member.ban", "user", &userID, reason, meta)
	})
}

func (s *Service) UnbanUser(ctx context.Context, p *Principal, ref string, userID uuid.UUID) error {
	return s.tx(ctx, func(q *store.Queries) error {
		place, _, err := s.requirePermission(ctx, q, p, ref, permissions.BanMembers)
		if err != nil {
			return err
		}
		n, err := q.DeleteBan(ctx, store.DeleteBanParams{PlaceID: place.ID, UserID: userID})
		if err != nil {
			return err
		}
		if n == 0 {
			return apperr.NotFound("ban not found")
		}
		return s.audit(ctx, q, place.ID, p, "member.unban", "user", &userID, "", nil)
	})
}
