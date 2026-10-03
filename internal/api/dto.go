package api

import (
	"time"

	"github.com/google/uuid"

	"github.com/parkerbrown98/gotalk-server/internal/permissions"
	"github.com/parkerbrown98/gotalk-server/internal/service"
	"github.com/parkerbrown98/gotalk-server/internal/store"
)

type User struct {
	ID          string    `json:"id" format:"uuid"`
	Username    string    `json:"username"`
	DisplayName string    `json:"display_name"`
	Bio         string    `json:"bio"`
	Pronouns    string    `json:"pronouns"`
	AvatarURL   *string   `json:"avatar_url"`
	CreatedAt   time.Time `json:"created_at"`
}

// SelfUser is the authenticated user's own view, including private fields.
type SelfUser struct {
	User
	Email           string `json:"email" format:"email"`
	EmailVerified   bool   `json:"email_verified"`
	IsInstanceAdmin bool   `json:"is_instance_admin"`
}

func toUser(u store.User) User {
	return User{
		ID:          u.ID.String(),
		Username:    u.Username,
		DisplayName: u.DisplayName,
		Bio:         u.Bio,
		Pronouns:    u.Pronouns,
		AvatarURL:   u.AvatarUrl,
		CreatedAt:   u.CreatedAt,
	}
}

func toSelfUser(u store.User) SelfUser {
	return SelfUser{
		User:            toUser(u),
		Email:           u.Email,
		EmailVerified:   u.EmailVerifiedAt != nil,
		IsInstanceAdmin: u.IsInstanceAdmin,
	}
}

type Tokens struct {
	TokenType             string    `json:"token_type" example:"Bearer"`
	AccessToken           string    `json:"access_token"`
	ExpiresIn             int64     `json:"expires_in" doc:"Access token lifetime in seconds"`
	AccessTokenExpiresAt  time.Time `json:"access_token_expires_at"`
	RefreshToken          string    `json:"refresh_token" doc:"Single-use; each refresh returns a new one"`
	RefreshTokenExpiresAt time.Time `json:"refresh_token_expires_at"`
	SessionID             string    `json:"session_id" format:"uuid"`
	User                  SelfUser  `json:"user"`
}

func toTokens(r *service.AuthResult) Tokens {
	return Tokens{
		TokenType:             "Bearer",
		AccessToken:           r.AccessToken,
		ExpiresIn:             int64(time.Until(r.AccessExpiresAt).Round(time.Second).Seconds()),
		AccessTokenExpiresAt:  r.AccessExpiresAt,
		RefreshToken:          r.RefreshToken,
		RefreshTokenExpiresAt: r.RefreshExpiresAt,
		SessionID:             r.SessionID.String(),
		User:                  toSelfUser(r.User),
	}
}

type Session struct {
	ID         string    `json:"id" format:"uuid"`
	UserAgent  string    `json:"user_agent"`
	IPAddress  string    `json:"ip_address"`
	CreatedAt  time.Time `json:"created_at"`
	LastUsedAt time.Time `json:"last_used_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	Current    bool      `json:"current"`
}

func toSession(s store.Session, current uuid.UUID) Session {
	return Session{
		ID:         s.ID.String(),
		UserAgent:  s.UserAgent,
		IPAddress:  s.IpAddress,
		CreatedAt:  s.CreatedAt,
		LastUsedAt: s.LastUsedAt,
		ExpiresAt:  s.ExpiresAt,
		Current:    s.ID == current,
	}
}

type Place struct {
	ID          string    `json:"id" format:"uuid"`
	Slug        string    `json:"slug"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	IconURL     *string   `json:"icon_url"`
	BannerURL   *string   `json:"banner_url"`
	Visibility  string    `json:"visibility" enum:"public,invite_only,private"`
	IsNSFW      bool      `json:"is_nsfw"`
	Locale      string    `json:"locale"`
	OwnerID     string    `json:"owner_id" format:"uuid"`
	MemberCount int32     `json:"member_count"`
	CreatedAt   time.Time `json:"created_at"`
	// MyPermissions is present only when the caller is a member.
	MyPermissions *int64 `json:"my_permissions,omitempty" doc:"Caller's effective permission bits; present only for members"`
}

func toPlace(p store.Place) Place {
	return Place{
		ID:          p.ID.String(),
		Slug:        p.Slug,
		Name:        p.Name,
		Description: p.Description,
		IconURL:     p.IconUrl,
		BannerURL:   p.BannerUrl,
		Visibility:  p.Visibility,
		IsNSFW:      p.IsNsfw,
		Locale:      p.Locale,
		OwnerID:     p.OwnerID.String(),
		MemberCount: p.MemberCount,
		CreatedAt:   p.CreatedAt,
	}
}

func toPlaceView(v service.PlaceView) Place {
	out := toPlace(v.Place)
	if v.Permissions != nil {
		perms := int64(*v.Permissions)
		out.MyPermissions = &perms
	}
	return out
}

type Member struct {
	User         User       `json:"user"`
	Nickname     *string    `json:"nickname"`
	RoleIDs      []string   `json:"role_ids" doc:"Explicitly assigned roles; the @everyone role is implicit"`
	JoinedAt     time.Time  `json:"joined_at"`
	TimeoutUntil *time.Time `json:"timeout_until" doc:"While in the future, the member cannot post, reply or react"`
}

func toMember(m service.MemberView) Member {
	ids := make([]string, len(m.RoleIDs))
	for i, id := range m.RoleIDs {
		ids[i] = id.String()
	}
	out := Member{User: toUser(m.User), Nickname: m.Member.Nickname, RoleIDs: ids, JoinedAt: m.Member.JoinedAt}
	if t := m.Member.TimeoutUntil; t != nil && t.After(time.Now()) {
		out.TimeoutUntil = t
	}
	return out
}

type Role struct {
	ID              string    `json:"id" format:"uuid"`
	Name            string    `json:"name"`
	Color           int32     `json:"color"`
	Position        int32     `json:"position"`
	Permissions     int64     `json:"permissions"`
	PermissionNames []string  `json:"permission_names"`
	IsDefault       bool      `json:"is_default"`
	CreatedAt       time.Time `json:"created_at"`
}

func toRole(r store.Role) Role {
	return Role{
		ID:              r.ID.String(),
		Name:            r.Name,
		Color:           r.Color,
		Position:        r.Position,
		Permissions:     r.Permissions,
		PermissionNames: permissions.Names(permissions.Permission(r.Permissions)),
		IsDefault:       r.IsDefault,
		CreatedAt:       r.CreatedAt,
	}
}

type Invite struct {
	Code      string     `json:"code"`
	PlaceID   string     `json:"place_id" format:"uuid"`
	CreatedBy *string    `json:"created_by" format:"uuid"`
	MaxUses   *int32     `json:"max_uses" doc:"null means unlimited"`
	Uses      int32      `json:"uses"`
	ExpiresAt *time.Time `json:"expires_at" doc:"null means the invite never expires"`
	CreatedAt time.Time  `json:"created_at"`
}

func toInvite(i store.Invite) Invite {
	var createdBy *string
	if i.CreatedBy != nil {
		s := i.CreatedBy.String()
		createdBy = &s
	}
	return Invite{
		Code:      i.Code,
		PlaceID:   i.PlaceID.String(),
		CreatedBy: createdBy,
		MaxUses:   i.MaxUses,
		Uses:      i.Uses,
		ExpiresAt: i.ExpiresAt,
		CreatedAt: i.CreatedAt,
	}
}

type Ban struct {
	User      User       `json:"user"`
	Reason    string     `json:"reason"`
	BannedBy  *string    `json:"banned_by" format:"uuid"`
	ExpiresAt *time.Time `json:"expires_at" doc:"null for permanent bans"`
	CreatedAt time.Time  `json:"created_at"`
}

func toBan(b store.ListBansRow) Ban {
	return Ban{
		User: toUser(b.User), Reason: b.PlaceBan.Reason, BannedBy: idString(b.PlaceBan.BannedBy),
		ExpiresAt: b.PlaceBan.ExpiresAt, CreatedAt: b.PlaceBan.CreatedAt,
	}
}

// Page is a paginated list. NextOffset is present when more results may exist.
type Page[T any] struct {
	Items      []T    `json:"items"`
	NextOffset *int32 `json:"next_offset,omitempty"`
}

func pageOf[S, T any](items []S, conv func(S) T, p service.Pagination) Page[T] {
	out := Page[T]{Items: make([]T, len(items))}
	for i, it := range items {
		out.Items[i] = conv(it)
	}
	if p.Limit > 0 && len(items) == int(p.Limit) {
		next := p.Offset + p.Limit
		out.NextOffset = &next
	}
	return out
}

func mapSlice[S, T any](items []S, conv func(S) T) []T {
	out := make([]T, len(items))
	for i, it := range items {
		out[i] = conv(it)
	}
	return out
}

// PageQuery is embedded in list inputs.
type PageQuery struct {
	Limit  int32 `query:"limit" minimum:"1" maximum:"100" default:"50"`
	Offset int32 `query:"offset" minimum:"0" default:"0"`
}

func (q PageQuery) pagination() service.Pagination {
	return service.Pagination{Limit: q.Limit, Offset: q.Offset}
}

type Body[T any] struct {
	Body T
}

func ok[T any](v T) (*Body[T], error) { return &Body[T]{Body: v}, nil }
