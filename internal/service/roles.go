package service

import (
	"context"
	"math"
	"strings"

	"github.com/google/uuid"

	"github.com/parkerbrown98/gotalk-server/internal/apperr"
	"github.com/parkerbrown98/gotalk-server/internal/permissions"
	"github.com/parkerbrown98/gotalk-server/internal/store"
)

func (s *Service) ListRoles(ctx context.Context, p *Principal, ref string) ([]store.Role, error) {
	place, _, err := s.requireMember(ctx, s.q, p, ref)
	if err != nil {
		return nil, err
	}
	return s.q.ListRoles(ctx, place.ID)
}

// MyPermissions returns the caller's effective permissions and standing in a place.
func (s *Service) MyPermissions(ctx context.Context, p *Principal, ref string) (permissions.Member, error) {
	_, acc, err := s.requireMember(ctx, s.q, p, ref)
	return acc.Member, err
}

type RoleInput struct {
	Name        *string
	Color       *int32
	Permissions *int64
	Position    *int32
}

func validateRoleFields(in RoleInput, actor permissions.Member) error {
	if in.Name != nil {
		name := strings.TrimSpace(*in.Name)
		if name == "" || len(name) > 64 {
			return apperr.Invalid("role name must be 1-64 characters")
		}
		*in.Name = name
	}
	if in.Color != nil && (*in.Color < 0 || *in.Color > 0xFFFFFF) {
		return apperr.Invalid("color must be an RGB integer between 0 and 16777215")
	}
	if in.Permissions != nil {
		perms := permissions.Permission(*in.Permissions)
		if !permissions.Valid(perms) {
			return apperr.Invalid("permissions contains unknown bits")
		}
		if !actor.CanGrant(perms) {
			return apperr.Forbidden("you cannot grant permissions you do not have")
		}
	}
	return nil
}

// CreateRole creates a role directly above the default role, shifting existing roles up.
func (s *Service) CreateRole(ctx context.Context, p *Principal, ref string, in RoleInput) (store.Role, error) {
	if in.Name == nil {
		return store.Role{}, apperr.Invalid("role name is required")
	}
	var role store.Role
	err := s.tx(ctx, func(q *store.Queries) error {
		place, acc, err := s.requirePermission(ctx, q, p, ref, permissions.ManageRoles)
		if err != nil {
			return err
		}
		if err := validateRoleFields(in, acc.Member); err != nil {
			return err
		}
		if _, err := q.LockPlace(ctx, place.ID); err != nil {
			return err
		}
		if err := q.ShiftRolePositions(ctx, store.ShiftRolePositionsParams{
			PlaceID: place.ID, Delta: 1, FromPos: 1, ToPos: math.MaxInt32 - 1,
		}); err != nil {
			return err
		}
		id, err := uuid.NewV7()
		if err != nil {
			return err
		}
		params := store.CreateRoleParams{ID: id, PlaceID: place.ID, Name: *in.Name, Position: 1}
		if in.Color != nil {
			params.Color = *in.Color
		}
		if in.Permissions != nil {
			params.Permissions = *in.Permissions
		}
		role, err = q.CreateRole(ctx, params)
		if err != nil {
			return err
		}
		return s.audit(ctx, q, place.ID, p, "role.create", "role", &role.ID, "",
			map[string]any{"name": role.Name, "permissions": role.Permissions})
	})
	return role, err
}

func (s *Service) UpdateRole(ctx context.Context, p *Principal, ref string, roleID uuid.UUID, in RoleInput) (store.Role, error) {
	var role store.Role
	err := s.tx(ctx, func(q *store.Queries) error {
		place, acc, err := s.requirePermission(ctx, q, p, ref, permissions.ManageRoles)
		if err != nil {
			return err
		}
		if _, err := q.LockPlace(ctx, place.ID); err != nil {
			return err
		}
		current, err := q.GetRole(ctx, store.GetRoleParams{ID: roleID, PlaceID: place.ID})
		if err != nil {
			return notFound(err, "role not found")
		}
		if !acc.Member.CanManageRoleAt(current.Position) {
			return apperr.Forbidden("you can only edit roles ranked below your highest role")
		}
		if current.IsDefault && (in.Name != nil || in.Position != nil) {
			return apperr.Invalid("the %s role cannot be renamed or moved", DefaultRoleName)
		}
		if err := validateRoleFields(in, acc.Member); err != nil {
			return err
		}

		if in.Position != nil && *in.Position != current.Position {
			top, err := q.MaxRolePosition(ctx, place.ID)
			if err != nil {
				return err
			}
			target := min(max(*in.Position, 1), top)
			if !acc.Member.CanManageRoleAt(target) {
				return apperr.Forbidden("you can only move roles below your highest role")
			}
			// Close the gap at the old position and open one at the target.
			if target > current.Position {
				err = q.ShiftRolePositions(ctx, store.ShiftRolePositionsParams{
					PlaceID: place.ID, Delta: -1, FromPos: current.Position + 1, ToPos: target,
				})
			} else {
				err = q.ShiftRolePositions(ctx, store.ShiftRolePositionsParams{
					PlaceID: place.ID, Delta: 1, FromPos: target, ToPos: current.Position - 1,
				})
			}
			if err != nil {
				return err
			}
			in.Position = &target
		}

		role, err = q.UpdateRole(ctx, store.UpdateRoleParams{
			ID:          current.ID,
			PlaceID:     place.ID,
			Name:        in.Name,
			Color:       in.Color,
			Permissions: in.Permissions,
			Position:    in.Position,
		})
		if err != nil {
			return err
		}
		// @everyone permissions decide what signed-out visitors can read.
		if role.IsDefault && role.Permissions != current.Permissions {
			if err := s.refreshPublicBoards(ctx, q, place.ID); err != nil {
				return err
			}
		}
		meta := map[string]any{}
		setIf(meta, "name", in.Name)
		setIf(meta, "color", in.Color)
		setIf(meta, "permissions", in.Permissions)
		setIf(meta, "position", in.Position)
		if in.Permissions != nil || in.Position != nil {
			s.emitPermissionsChanged(ctx, q, place.ID)
		}
		return s.audit(ctx, q, place.ID, p, "role.update", "role", &role.ID, "", meta)
	})
	return role, err
}

func (s *Service) DeleteRole(ctx context.Context, p *Principal, ref string, roleID uuid.UUID) error {
	return s.tx(ctx, func(q *store.Queries) error {
		place, acc, err := s.requirePermission(ctx, q, p, ref, permissions.ManageRoles)
		if err != nil {
			return err
		}
		if _, err := q.LockPlace(ctx, place.ID); err != nil {
			return err
		}
		role, err := q.GetRole(ctx, store.GetRoleParams{ID: roleID, PlaceID: place.ID})
		if err != nil {
			return notFound(err, "role not found")
		}
		if role.IsDefault {
			return apperr.Invalid("the %s role cannot be deleted", DefaultRoleName)
		}
		if !acc.Member.CanManageRoleAt(role.Position) {
			return apperr.Forbidden("you can only delete roles ranked below your highest role")
		}
		if err := q.DeleteRole(ctx, store.DeleteRoleParams{ID: role.ID, PlaceID: place.ID}); err != nil {
			return err
		}
		if err := s.audit(ctx, q, place.ID, p, "role.delete", "role", &role.ID, "", map[string]any{"name": role.Name}); err != nil {
			return err
		}
		s.emitPermissionsChanged(ctx, q, place.ID)
		return q.ShiftRolePositions(ctx, store.ShiftRolePositionsParams{
			PlaceID: place.ID, Delta: -1, FromPos: role.Position + 1, ToPos: math.MaxInt32,
		})
	})
}
