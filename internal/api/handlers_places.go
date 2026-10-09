package api

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/parkerbrown98/gotalk-server/internal/permissions"
	"github.com/parkerbrown98/gotalk-server/internal/service"
)

const (
	tagPlaces  = "Places"
	tagMembers = "Members"
	tagRoles   = "Roles"
	tagInvites = "Invites"
)

type PlacePath struct {
	Place string `path:"place" maxLength:"64" doc:"Place ID or slug"`
}

type MemberPath struct {
	PlacePath
	UserID string `path:"userID" format:"uuid"`
}

type MemberRolePath struct {
	MemberPath
	RoleID string `path:"roleID" format:"uuid"`
}

type RolePath struct {
	PlacePath
	RoleID string `path:"roleID" format:"uuid"`
}

type InviteCodePath struct {
	Code string `path:"code" maxLength:"64"`
}

type PlaceInvitePath struct {
	PlacePath
	Code string `path:"code" maxLength:"64"`
}

type CreatePlaceRequest struct {
	Slug        string `json:"slug" minLength:"3" maxLength:"32" pattern:"^[a-z0-9][a-z0-9-]*[a-z0-9]$"`
	Name        string `json:"name" minLength:"1" maxLength:"100"`
	Description string `json:"description,omitempty" maxLength:"2000"`
	Visibility  string `json:"visibility,omitempty" enum:"public,invite_only,private" default:"public"`
	IsNSFW      bool   `json:"is_nsfw,omitempty"`
	Locale      string `json:"locale,omitempty" maxLength:"16" default:"en"`
}

type UpdatePlaceRequest struct {
	Slug        *string `json:"slug,omitempty" minLength:"3" maxLength:"32"`
	Name        *string `json:"name,omitempty" minLength:"1" maxLength:"100"`
	Description *string `json:"description,omitempty" maxLength:"2000"`
	Visibility  *string `json:"visibility,omitempty" enum:"public,invite_only,private"`
	IsNSFW      *bool   `json:"is_nsfw,omitempty"`
	Locale      *string `json:"locale,omitempty" maxLength:"16"`
	IconURL     *string `json:"icon_url,omitempty" maxLength:"2048" doc:"Empty string clears the icon"`
	BannerURL   *string `json:"banner_url,omitempty" maxLength:"2048" doc:"Empty string clears the banner"`
	// VotingEnabled is a place setting rather than a profile field.
	VotingEnabled *bool `json:"voting_enabled,omitempty" doc:"Turn topic voting on or off"`
}

type TransferOwnershipRequest struct {
	UserID string `json:"user_id" format:"uuid"`
}

type DiscoverInput struct {
	PageQuery
	Query string `query:"q" maxLength:"100" doc:"Case-insensitive match on name or description"`
}

type PlacePageInput struct {
	PlacePath
	PageQuery
}

type ListMembersInput struct {
	PlacePath
	PageQuery
	Query string `query:"q" maxLength:"32" doc:"Prefix of a username, display name or nickname (autocomplete)"`
}

// ReasonQuery carries an optional audit-log reason on DELETE requests.
type ReasonQuery struct {
	Reason string `query:"reason" maxLength:"512" doc:"Recorded in the audit log and shown to the affected user"`
}

type UpdateMemberRequest struct {
	Nickname string `json:"nickname" maxLength:"32" doc:"Empty string clears the nickname"`
}

type RoleRequest struct {
	Name        *string `json:"name,omitempty" minLength:"1" maxLength:"64"`
	Color       *int32  `json:"color,omitempty" minimum:"0" maximum:"16777215"`
	Permissions *int64  `json:"permissions,omitempty" minimum:"0"`
	Position    *int32  `json:"position,omitempty" minimum:"1" doc:"Higher positions outrank lower ones; ignored on create"`
}

type CreateInviteRequest struct {
	MaxUses int32  `json:"max_uses,omitempty" minimum:"0" maximum:"10000" doc:"0 means unlimited"`
	MaxAge  *int64 `json:"max_age,omitempty" minimum:"0" maximum:"2592000" doc:"Seconds until expiry; 0 never expires; defaults to 7 days"`
}

type BanRequest struct {
	Reason   string `json:"reason,omitempty" maxLength:"512"`
	Duration int64  `json:"duration,omitempty" minimum:"0" maximum:"31536000" doc:"Seconds until the ban lifts; 0 or omitted bans permanently"`
}

type MyPermissions struct {
	IsOwner         bool     `json:"is_owner"`
	Permissions     int64    `json:"permissions"`
	PermissionNames []string `json:"permission_names"`
	TopPosition     int32    `json:"top_position"`
}

type InvitePreview struct {
	Code      string     `json:"code"`
	ExpiresAt *time.Time `json:"expires_at"`
	Place     Place      `json:"place"`
}

func (s *Server) registerPlaces() {
	huma.Register(s.api, withStatus(withAuth(operation("create-place", http.MethodPost, "/places",
		"Create a place", tagPlaces)), http.StatusCreated),
		handle(s, func(ctx context.Context, in *Body[CreatePlaceRequest]) (*Body[Place], error) {
			b := in.Body
			v, err := s.Service.CreatePlace(ctx, mustPrincipal(ctx), service.CreatePlaceInput{
				Slug: b.Slug, Name: b.Name, Description: b.Description,
				Visibility: b.Visibility, IsNSFW: b.IsNSFW, Locale: b.Locale,
			})
			if err != nil {
				return nil, err
			}
			return ok(toPlaceView(v))
		}))

	huma.Register(s.api, operation("discover-places", http.MethodGet, "/places",
		"Discover public and invite-only places, most members first", tagPlaces),
		handle(s, func(ctx context.Context, in *DiscoverInput) (*Body[Page[Place]], error) {
			page := in.pagination()
			places, err := s.Service.DiscoverPlaces(ctx, in.Query, page)
			if err != nil {
				return nil, err
			}
			return ok(pageOf(places, toPlace, page))
		}))

	huma.Register(s.api, withOptionalAuth(operation("get-place", http.MethodGet, "/places/{place}",
		"Get a place by ID or slug", tagPlaces)),
		handle(s, func(ctx context.Context, in *PlacePath) (*Body[Place], error) {
			v, err := s.Service.GetPlace(ctx, principalFrom(ctx), in.Place)
			if err != nil {
				return nil, err
			}
			return ok(toPlaceView(v))
		}))

	huma.Register(s.api, withAuth(operation("update-place", http.MethodPatch, "/places/{place}",
		"Update a place (MANAGE_PLACE)", tagPlaces)),
		handle(s, func(ctx context.Context, in *struct {
			PlacePath
			Body UpdatePlaceRequest
		}) (*Body[Place], error) {
			b := in.Body
			v, err := s.Service.UpdatePlace(ctx, mustPrincipal(ctx), in.Place, service.PlaceUpdate{
				Name: b.Name, Description: b.Description, Slug: b.Slug, Visibility: b.Visibility,
				IsNSFW: b.IsNSFW, Locale: b.Locale, IconURL: b.IconURL, BannerURL: b.BannerURL,
				VotingEnabled: b.VotingEnabled,
			})
			if err != nil {
				return nil, err
			}
			return ok(toPlaceView(v))
		}))

	huma.Register(s.api, withAuth(operation("delete-place", http.MethodDelete, "/places/{place}",
		"Delete a place (owner only)", tagPlaces)),
		handle(s, func(ctx context.Context, in *PlacePath) (*struct{}, error) {
			return nil, s.Service.DeletePlace(ctx, mustPrincipal(ctx), in.Place)
		}))

	huma.Register(s.api, withAuth(operation("transfer-place", http.MethodPost, "/places/{place}/transfer",
		"Transfer ownership to another member (owner only)", tagPlaces)),
		handle(s, func(ctx context.Context, in *struct {
			PlacePath
			Body TransferOwnershipRequest
		}) (*Body[Place], error) {
			id, err := parseID("user_id", in.Body.UserID)
			if err != nil {
				return nil, err
			}
			v, err := s.Service.TransferOwnership(ctx, mustPrincipal(ctx), in.Place, id)
			if err != nil {
				return nil, err
			}
			return ok(toPlaceView(v))
		}))

	huma.Register(s.api, withAuth(operation("join-place", http.MethodPost, "/places/{place}/join",
		"Join a public place", tagPlaces)),
		handle(s, func(ctx context.Context, in *PlacePath) (*Body[Place], error) {
			v, err := s.Service.JoinPlace(ctx, mustPrincipal(ctx), in.Place)
			if err != nil {
				return nil, err
			}
			return ok(toPlaceView(v))
		}))

	huma.Register(s.api, withStatus(withAuth(operation("leave-place", http.MethodPost, "/places/{place}/leave",
		"Leave a place", tagPlaces)), http.StatusNoContent),
		handle(s, func(ctx context.Context, in *PlacePath) (*struct{}, error) {
			return nil, s.Service.LeavePlace(ctx, mustPrincipal(ctx), in.Place)
		}))

	huma.Register(s.api, withAuth(operation("get-my-permissions", http.MethodGet, "/places/{place}/permissions/@me",
		"Get the caller's effective permissions in a place", tagPlaces)),
		handle(s, func(ctx context.Context, in *PlacePath) (*Body[MyPermissions], error) {
			m, err := s.Service.MyPermissions(ctx, mustPrincipal(ctx), in.Place)
			if err != nil {
				return nil, err
			}
			eff := m.Effective()
			return ok(MyPermissions{
				IsOwner:         m.IsOwner,
				Permissions:     int64(eff),
				PermissionNames: permissions.Names(eff),
				TopPosition:     m.TopPosition,
			})
		}))
}

func (s *Server) registerMembers() {
	huma.Register(s.api, withAuth(operation("list-members", http.MethodGet, "/places/{place}/members",
		"List members", tagMembers)),
		handle(s, func(ctx context.Context, in *ListMembersInput) (*Body[Page[Member]], error) {
			page := in.pagination()
			members, err := s.Service.ListMembers(ctx, mustPrincipal(ctx), in.Place, in.Query, page)
			if err != nil {
				return nil, err
			}
			return ok(pageOf(members, toMember, page))
		}))

	huma.Register(s.api, withAuth(operation("get-member", http.MethodGet, "/places/{place}/members/{userID}",
		"Get a member", tagMembers)),
		handle(s, func(ctx context.Context, in *MemberPath) (*Body[Member], error) {
			id, err := parseID("userID", in.UserID)
			if err != nil {
				return nil, err
			}
			m, err := s.Service.GetMember(ctx, mustPrincipal(ctx), in.Place, id)
			if err != nil {
				return nil, err
			}
			return ok(toMember(m))
		}))

	huma.Register(s.api, withAuth(operation("update-member", http.MethodPatch, "/places/{place}/members/{userID}",
		"Set a member's nickname (CHANGE_NICKNAME for yourself, MANAGE_NICKNAMES for others)", tagMembers)),
		handle(s, func(ctx context.Context, in *struct {
			MemberPath
			Body UpdateMemberRequest
		}) (*Body[Member], error) {
			id, err := parseID("userID", in.UserID)
			if err != nil {
				return nil, err
			}
			m, err := s.Service.SetNickname(ctx, mustPrincipal(ctx), in.Place, id, in.Body.Nickname)
			if err != nil {
				return nil, err
			}
			return ok(toMember(m))
		}))

	huma.Register(s.api, withAuth(operation("kick-member", http.MethodDelete, "/places/{place}/members/{userID}",
		"Kick a member (KICK_MEMBERS)", tagMembers)),
		handle(s, func(ctx context.Context, in *struct {
			MemberPath
			ReasonQuery
		}) (*struct{}, error) {
			id, err := parseID("userID", in.UserID)
			if err != nil {
				return nil, err
			}
			return nil, s.Service.KickMember(ctx, mustPrincipal(ctx), in.Place, id, in.Reason)
		}))

	huma.Register(s.api, withAuth(operation("assign-role", http.MethodPut, "/places/{place}/members/{userID}/roles/{roleID}",
		"Assign a role to a member (MANAGE_ROLES)", tagMembers)),
		handle(s, func(ctx context.Context, in *MemberRolePath) (*Body[Member], error) {
			userID, err := parseID("userID", in.UserID)
			if err != nil {
				return nil, err
			}
			roleID, err := parseID("roleID", in.RoleID)
			if err != nil {
				return nil, err
			}
			m, err := s.Service.AssignRole(ctx, mustPrincipal(ctx), in.Place, userID, roleID)
			if err != nil {
				return nil, err
			}
			return ok(toMember(m))
		}))

	huma.Register(s.api, withStatus(withAuth(operation("unassign-role", http.MethodDelete,
		"/places/{place}/members/{userID}/roles/{roleID}", "Remove a role from a member (MANAGE_ROLES)", tagMembers)), http.StatusOK),
		handle(s, func(ctx context.Context, in *MemberRolePath) (*Body[Member], error) {
			userID, err := parseID("userID", in.UserID)
			if err != nil {
				return nil, err
			}
			roleID, err := parseID("roleID", in.RoleID)
			if err != nil {
				return nil, err
			}
			m, err := s.Service.UnassignRole(ctx, mustPrincipal(ctx), in.Place, userID, roleID)
			if err != nil {
				return nil, err
			}
			return ok(toMember(m))
		}))

	huma.Register(s.api, withAuth(operation("list-bans", http.MethodGet, "/places/{place}/bans",
		"List bans (BAN_MEMBERS)", tagMembers)),
		handle(s, func(ctx context.Context, in *PlacePageInput) (*Body[Page[Ban]], error) {
			page := in.pagination()
			bans, err := s.Service.ListBans(ctx, mustPrincipal(ctx), in.Place, page)
			if err != nil {
				return nil, err
			}
			return ok(pageOf(bans, toBan, page))
		}))

	huma.Register(s.api, withStatus(withAuth(operation("ban-user", http.MethodPut, "/places/{place}/bans/{userID}",
		"Ban a user and remove them from the place (BAN_MEMBERS)", tagMembers)), http.StatusNoContent),
		handle(s, func(ctx context.Context, in *struct {
			MemberPath
			Body *BanRequest
		}) (*struct{}, error) {
			id, err := parseID("userID", in.UserID)
			if err != nil {
				return nil, err
			}
			var (
				reason   string
				duration time.Duration
			)
			if in.Body != nil {
				reason = in.Body.Reason
				duration = time.Duration(in.Body.Duration) * time.Second
			}
			return nil, s.Service.BanUser(ctx, mustPrincipal(ctx), in.Place, id, reason, duration)
		}))

	huma.Register(s.api, withAuth(operation("unban-user", http.MethodDelete, "/places/{place}/bans/{userID}",
		"Lift a ban (BAN_MEMBERS)", tagMembers)),
		handle(s, func(ctx context.Context, in *MemberPath) (*struct{}, error) {
			id, err := parseID("userID", in.UserID)
			if err != nil {
				return nil, err
			}
			return nil, s.Service.UnbanUser(ctx, mustPrincipal(ctx), in.Place, id)
		}))
}

func (s *Server) registerRoles() {
	huma.Register(s.api, withAuth(operation("list-roles", http.MethodGet, "/places/{place}/roles",
		"List roles, highest first", tagRoles)),
		handle(s, func(ctx context.Context, in *PlacePath) (*Body[[]Role], error) {
			roles, err := s.Service.ListRoles(ctx, mustPrincipal(ctx), in.Place)
			if err != nil {
				return nil, err
			}
			return ok(mapSlice(roles, toRole))
		}))

	huma.Register(s.api, withStatus(withAuth(operation("create-role", http.MethodPost, "/places/{place}/roles",
		"Create a role just above @everyone (MANAGE_ROLES)", tagRoles)), http.StatusCreated),
		handle(s, func(ctx context.Context, in *struct {
			PlacePath
			Body RoleRequest
		}) (*Body[Role], error) {
			role, err := s.Service.CreateRole(ctx, mustPrincipal(ctx), in.Place, roleInput(in.Body))
			if err != nil {
				return nil, err
			}
			return ok(toRole(role))
		}))

	huma.Register(s.api, withAuth(operation("update-role", http.MethodPatch, "/places/{place}/roles/{roleID}",
		"Update or reorder a role (MANAGE_ROLES)", tagRoles)),
		handle(s, func(ctx context.Context, in *struct {
			RolePath
			Body RoleRequest
		}) (*Body[Role], error) {
			id, err := parseID("roleID", in.RoleID)
			if err != nil {
				return nil, err
			}
			role, err := s.Service.UpdateRole(ctx, mustPrincipal(ctx), in.Place, id, roleInput(in.Body))
			if err != nil {
				return nil, err
			}
			return ok(toRole(role))
		}))

	huma.Register(s.api, withAuth(operation("delete-role", http.MethodDelete, "/places/{place}/roles/{roleID}",
		"Delete a role (MANAGE_ROLES)", tagRoles)),
		handle(s, func(ctx context.Context, in *RolePath) (*struct{}, error) {
			id, err := parseID("roleID", in.RoleID)
			if err != nil {
				return nil, err
			}
			return nil, s.Service.DeleteRole(ctx, mustPrincipal(ctx), in.Place, id)
		}))
}

func roleInput(r RoleRequest) service.RoleInput {
	return service.RoleInput{Name: r.Name, Color: r.Color, Permissions: r.Permissions, Position: r.Position}
}

func (s *Server) registerInvites() {
	huma.Register(s.api, withStatus(withAuth(operation("create-invite", http.MethodPost, "/places/{place}/invites",
		"Create an invite (CREATE_INVITES)", tagInvites)), http.StatusCreated),
		handle(s, func(ctx context.Context, in *struct {
			PlacePath
			Body *CreateInviteRequest
		}) (*Body[Invite], error) {
			var input service.InviteInput
			if b := in.Body; b != nil {
				input.MaxUses = b.MaxUses
				if b.MaxAge != nil {
					d := time.Duration(*b.MaxAge) * time.Second
					input.MaxAge = &d
				}
			}
			invite, err := s.Service.CreateInvite(ctx, mustPrincipal(ctx), in.Place, input)
			if err != nil {
				return nil, err
			}
			return ok(toInvite(invite))
		}))

	huma.Register(s.api, withAuth(operation("list-invites", http.MethodGet, "/places/{place}/invites",
		"List a place's invites (MANAGE_INVITES)", tagInvites)),
		handle(s, func(ctx context.Context, in *PlacePath) (*Body[[]Invite], error) {
			invites, err := s.Service.ListInvites(ctx, mustPrincipal(ctx), in.Place)
			if err != nil {
				return nil, err
			}
			return ok(mapSlice(invites, toInvite))
		}))

	huma.Register(s.api, withAuth(operation("delete-invite", http.MethodDelete, "/places/{place}/invites/{code}",
		"Revoke an invite (creator or MANAGE_INVITES)", tagInvites)),
		handle(s, func(ctx context.Context, in *PlaceInvitePath) (*struct{}, error) {
			return nil, s.Service.DeleteInvite(ctx, mustPrincipal(ctx), in.Place, in.Code)
		}))

	huma.Register(s.api, operation("preview-invite", http.MethodGet, "/invites/{code}",
		"Preview the place an invite leads to", tagInvites),
		handle(s, func(ctx context.Context, in *InviteCodePath) (*Body[InvitePreview], error) {
			p, err := s.Service.PreviewInvite(ctx, in.Code)
			if err != nil {
				return nil, err
			}
			return ok(InvitePreview{Code: p.Invite.Code, ExpiresAt: p.Invite.ExpiresAt, Place: toPlace(p.Place)})
		}))

	huma.Register(s.api, withAuth(operation("accept-invite", http.MethodPost, "/invites/{code}",
		"Accept an invite and join its place", tagInvites)),
		handle(s, func(ctx context.Context, in *InviteCodePath) (*Body[Place], error) {
			v, err := s.Service.AcceptInvite(ctx, mustPrincipal(ctx), in.Code)
			if err != nil {
				return nil, err
			}
			return ok(toPlaceView(v))
		}))
}
