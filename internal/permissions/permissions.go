// Package permissions defines place-level permission bits and hierarchy rules.
//
// Bit values are part of the public API and must never be renumbered. All bits stay
// below 2^53 so they round-trip safely through JSON numbers in JavaScript clients.
package permissions

import (
	"slices"
	"sort"

	"github.com/google/uuid"
)

type Permission int64

const (
	Administrator   Permission = 1 << 0
	ManagePlace     Permission = 1 << 1
	ManageRoles     Permission = 1 << 2
	CreateInvites   Permission = 1 << 3
	ManageInvites   Permission = 1 << 4
	KickMembers     Permission = 1 << 5
	BanMembers      Permission = 1 << 6
	ChangeNickname  Permission = 1 << 7
	ManageNicknames Permission = 1 << 8
	ViewAuditLog    Permission = 1 << 9
	ModerateMembers Permission = 1 << 10
	ManageReports   Permission = 1 << 11

	// Forum permissions (phase 2).
	ViewBoards    Permission = 1 << 16
	CreateTopics  Permission = 1 << 17
	ReplyToTopics Permission = 1 << 18
	AddReactions  Permission = 1 << 19
	AttachFiles   Permission = 1 << 20
	ManageBoards  Permission = 1 << 21
	ManagePosts   Permission = 1 << 22

	// Chat permissions (phase 3).
	ViewChannels   Permission = 1 << 24
	SendMessages   Permission = 1 << 25
	ManageMessages Permission = 1 << 26
	ManageChannels Permission = 1 << 27

	// Voice permissions (phase 4).
	ConnectVoice Permission = 1 << 32
	Speak        Permission = 1 << 33
	ShareScreen  Permission = 1 << 34
	MuteMembers  Permission = 1 << 35
	MoveMembers  Permission = 1 << 36
)

var names = map[Permission]string{
	Administrator:   "ADMINISTRATOR",
	ManagePlace:     "MANAGE_PLACE",
	ManageRoles:     "MANAGE_ROLES",
	CreateInvites:   "CREATE_INVITES",
	ManageInvites:   "MANAGE_INVITES",
	KickMembers:     "KICK_MEMBERS",
	BanMembers:      "BAN_MEMBERS",
	ChangeNickname:  "CHANGE_NICKNAME",
	ManageNicknames: "MANAGE_NICKNAMES",
	ViewAuditLog:    "VIEW_AUDIT_LOG",
	ModerateMembers: "MODERATE_MEMBERS",
	ManageReports:   "MANAGE_REPORTS",
	ViewBoards:      "VIEW_BOARDS",
	CreateTopics:    "CREATE_TOPICS",
	ReplyToTopics:   "REPLY_TO_TOPICS",
	AddReactions:    "ADD_REACTIONS",
	AttachFiles:     "ATTACH_FILES",
	ManageBoards:    "MANAGE_BOARDS",
	ManagePosts:     "MANAGE_POSTS",
	ViewChannels:    "VIEW_CHANNELS",
	SendMessages:    "SEND_MESSAGES",
	ManageMessages:  "MANAGE_MESSAGES",
	ManageChannels:  "MANAGE_CHANNELS",
	ConnectVoice:    "CONNECT_VOICE",
	Speak:           "SPEAK",
	ShareScreen:     "SHARE_SCREEN",
	MuteMembers:     "MUTE_MEMBERS",
	MoveMembers:     "MOVE_MEMBERS",
}

// All is the union of every defined permission.
var All = func() Permission {
	var p Permission
	for bit := range names {
		p |= bit
	}
	return p
}()

// Default is granted to the @everyone role of newly created places.
const Default = CreateInvites | ChangeNickname |
	ViewBoards | CreateTopics | ReplyToTopics | AddReactions | AttachFiles |
	ViewChannels | SendMessages |
	ConnectVoice | Speak | ShareScreen

// Forum is the set of permissions that board overwrites may allow or deny.
const Forum = ViewBoards | CreateTopics | ReplyToTopics | AddReactions | AttachFiles | ManageBoards | ManagePosts

// Guest is the most a non-member may hold: read access to public places.
const Guest = ViewBoards

// Participation is withheld from members while they are timed out.
const Participation = CreateInvites | ChangeNickname |
	CreateTopics | ReplyToTopics | AddReactions | AttachFiles |
	SendMessages | Speak | ShareScreen

type Definition struct {
	Name  string `json:"name"`
	Value int64  `json:"value"`
}

// Definitions lists every permission ordered by bit value, for client discovery.
func Definitions() []Definition {
	defs := make([]Definition, 0, len(names))
	for bit, name := range names {
		defs = append(defs, Definition{Name: name, Value: int64(bit)})
	}
	sort.Slice(defs, func(i, j int) bool { return defs[i].Value < defs[j].Value })
	return defs
}

// Valid reports whether p contains only defined bits.
func Valid(p Permission) bool { return p&^All == 0 }

// Names returns the names of the bits set in p.
func Names(p Permission) []string {
	out := []string{}
	for _, d := range Definitions() {
		if p&Permission(d.Value) != 0 {
			out = append(out, d.Name)
		}
	}
	return out
}

// Member is a member's effective standing within a place. Non-members reading a public
// place are represented with Guest set.
type Member struct {
	IsOwner bool
	// Raw is the union of the default role and every assigned role.
	Raw Permission
	// TopPosition is the highest position among the member's roles (0 = default role only).
	TopPosition int32
	// DefaultRoleID is the place's @everyone role; RoleIDs lists every role the member
	// holds, including the default role. Both are used to apply board overwrites.
	DefaultRoleID uuid.UUID
	RoleIDs       []uuid.UUID
	// TimedOut withholds Participation permissions.
	TimedOut bool
	// Guest caps permissions at Guest.
	Guest bool
}

// Effective returns the permissions the member actually holds.
func (m Member) Effective() Permission {
	if m.Guest {
		return m.Raw & Guest
	}
	if m.IsOwner || m.Raw&Administrator != 0 {
		return All
	}
	if m.TimedOut {
		return m.Raw &^ Participation
	}
	return m.Raw
}

// Has reports whether the member holds every permission in p.
func (m Member) Has(p Permission) bool { return m.Effective()&p == p }

// Overwrite adjusts a role's permissions within one board.
type Overwrite struct {
	RoleID uuid.UUID
	Allow  Permission
	Deny   Permission
}

// InScope returns the member's permissions after applying overwrite levels in order,
// outermost (root board) first. Within a level the @everyone overwrite applies first, then
// the combined overwrites of the member's other roles, so a role allow beats an @everyone
// deny. Owners and administrators are unaffected.
func (m Member) InScope(levels ...[]Overwrite) Permission {
	if !m.Guest && (m.IsOwner || m.Raw&Administrator != 0) {
		return All
	}
	p := m.Raw
	for _, level := range levels {
		var allow, deny Permission
		for _, ow := range level {
			switch {
			case ow.RoleID == m.DefaultRoleID:
				p = (p &^ ow.Deny) | ow.Allow
			case slices.Contains(m.RoleIDs, ow.RoleID):
				allow |= ow.Allow
				deny |= ow.Deny
			}
		}
		p = (p &^ deny) | allow
	}
	if m.Guest {
		p &= Guest
	}
	if m.TimedOut {
		p &^= Participation
	}
	return p
}

// Outranks reports whether m sits strictly above other in the role hierarchy. Owners
// outrank everyone and cannot be outranked.
func (m Member) Outranks(other Member) bool {
	if other.IsOwner {
		return false
	}
	if m.IsOwner {
		return true
	}
	return m.TopPosition > other.TopPosition
}

// CanManageRoleAt reports whether m may edit, assign or delete a role at position.
func (m Member) CanManageRoleAt(position int32) bool {
	return m.IsOwner || m.TopPosition > position
}

// CanGrant reports whether m may put permissions p on a role. Members can only grant
// permissions they hold themselves.
func (m Member) CanGrant(p Permission) bool {
	return p&^m.Effective() == 0
}
