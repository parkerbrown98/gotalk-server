package permissions

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

// Bit values are a public contract with clients; changing one is a breaking change.
func TestBitValuesAreStable(t *testing.T) {
	want := map[Permission]int64{
		Administrator: 1, ManagePlace: 2, ManageRoles: 4, CreateInvites: 8, ManageInvites: 16,
		KickMembers: 32, BanMembers: 64, ChangeNickname: 128, ManageNicknames: 256,
		ViewAuditLog: 512, ModerateMembers: 1024, ManageReports: 2048,
		ViewBoards: 1 << 16, ManagePosts: 1 << 22, ViewChannels: 1 << 24, ManageChannels: 1 << 27,
		ConnectVoice: 1 << 32, MoveMembers: 1 << 36,
	}
	for p, v := range want {
		assert.EqualValues(t, v, p, names[p])
	}
	assert.Less(t, int64(All), int64(1)<<53, "must stay JSON-number safe")
}

func TestDefinitionsAreSortedAndComplete(t *testing.T) {
	defs := Definitions()
	assert.Len(t, defs, len(names))
	for i := 1; i < len(defs); i++ {
		assert.Less(t, defs[i-1].Value, defs[i].Value)
	}
}

func TestValidAndNames(t *testing.T) {
	assert.True(t, Valid(KickMembers|BanMembers))
	assert.False(t, Valid(1<<62))
	assert.Equal(t, []string{"KICK_MEMBERS", "BAN_MEMBERS"}, Names(BanMembers|KickMembers))
	assert.True(t, Valid(Default))
}

func TestEffectivePermissions(t *testing.T) {
	member := Member{Raw: KickMembers}
	assert.True(t, member.Has(KickMembers))
	assert.False(t, member.Has(BanMembers))
	assert.False(t, member.Has(KickMembers|BanMembers), "Has requires every bit")

	assert.Equal(t, All, Member{Raw: Administrator}.Effective())
	assert.Equal(t, All, Member{IsOwner: true}.Effective())
}

func TestHierarchy(t *testing.T) {
	owner := Member{IsOwner: true}
	admin := Member{Raw: Administrator, TopPosition: 5}
	mod := Member{Raw: KickMembers, TopPosition: 3}
	peer := Member{Raw: KickMembers, TopPosition: 3}
	member := Member{}

	assert.True(t, owner.Outranks(admin))
	assert.False(t, admin.Outranks(owner), "nobody outranks the owner")
	assert.False(t, owner.Outranks(owner))
	assert.True(t, admin.Outranks(mod))
	assert.True(t, mod.Outranks(member))
	assert.False(t, mod.Outranks(peer), "equal rank cannot act on each other")

	assert.True(t, mod.CanManageRoleAt(2))
	assert.False(t, mod.CanManageRoleAt(3))
	assert.True(t, owner.CanManageRoleAt(1000))
}

func TestCanGrant(t *testing.T) {
	mod := Member{Raw: KickMembers | ManageRoles, TopPosition: 3}
	assert.True(t, mod.CanGrant(KickMembers))
	assert.False(t, mod.CanGrant(BanMembers))
	assert.False(t, mod.CanGrant(Administrator))
	assert.True(t, Member{Raw: Administrator}.CanGrant(Administrator|BanMembers))
}

func TestTimeoutAndGuestMasks(t *testing.T) {
	timedOut := Member{Raw: Default | KickMembers, TimedOut: true}
	assert.True(t, timedOut.Has(ViewBoards|KickMembers))
	assert.False(t, timedOut.Has(ReplyToTopics))
	assert.False(t, timedOut.Has(AddReactions))
	assert.Equal(t, All, Member{IsOwner: true, TimedOut: true}.Effective())

	guest := Member{Raw: Default, Guest: true}
	assert.Equal(t, ViewBoards, guest.Effective())
	assert.Equal(t, Permission(0), Member{Raw: Administrator, Guest: true}.Effective(),
		"guests never inherit administrator")
}

func TestBoardOverwrites(t *testing.T) {
	everyone, mods, vip := uuid.New(), uuid.New(), uuid.New()
	member := Member{Raw: Default, DefaultRoleID: everyone, RoleIDs: []uuid.UUID{everyone}}
	mod := Member{Raw: Default, DefaultRoleID: everyone, RoleIDs: []uuid.UUID{everyone, mods}}

	staffOnly := []Overwrite{{RoleID: everyone, Deny: ViewBoards}, {RoleID: mods, Allow: ViewBoards | ManagePosts}}
	assert.Zero(t, member.InScope(staffOnly)&ViewBoards)
	assert.Equal(t, ViewBoards|ManagePosts, mod.InScope(staffOnly)&(ViewBoards|ManagePosts),
		"a role allow beats an @everyone deny")

	// Role allows and denies at the same level: deny is applied, then allow wins.
	both := Member{Raw: Default, DefaultRoleID: everyone, RoleIDs: []uuid.UUID{everyone, mods, vip}}
	mixed := []Overwrite{{RoleID: mods, Deny: CreateTopics}, {RoleID: vip, Allow: CreateTopics}}
	assert.NotZero(t, both.InScope(mixed)&CreateTopics)

	// Inheritance: a child level can re-open what a parent closed, and vice versa.
	announcements := []Overwrite{{RoleID: everyone, Deny: CreateTopics}}
	reopened := []Overwrite{{RoleID: everyone, Allow: CreateTopics}}
	assert.Zero(t, member.InScope(announcements)&CreateTopics)
	assert.NotZero(t, member.InScope(announcements, reopened)&CreateTopics)
	assert.Zero(t, member.InScope(reopened, announcements)&CreateTopics)

	assert.Equal(t, All, Member{IsOwner: true}.InScope(staffOnly))
	guest := Member{Raw: Default, Guest: true, DefaultRoleID: everyone, RoleIDs: []uuid.UUID{everyone}}
	assert.Equal(t, ViewBoards, guest.InScope(nil))
	assert.Zero(t, guest.InScope(staffOnly))
	assert.Zero(t, Member{Raw: Default, TimedOut: true}.InScope()&ReplyToTopics)
}
