-- name: AddMember :execrows
INSERT INTO place_members (place_id, user_id)
VALUES (@place_id, @user_id)
ON CONFLICT DO NOTHING;

-- name: GetMember :one
SELECT sqlc.embed(m), sqlc.embed(u)
FROM place_members m
JOIN users u ON u.id = m.user_id
WHERE m.place_id = @place_id AND m.user_id = @user_id;

-- name: IsMember :one
SELECT EXISTS (SELECT 1 FROM place_members WHERE place_id = @place_id AND user_id = @user_id);

-- name: ListMembers :many
-- The optional query is a case-insensitive prefix of the username, display name or nickname
-- (LIKE wildcards must already be escaped).
SELECT sqlc.embed(m), sqlc.embed(u)
FROM place_members m
JOIN users u ON u.id = m.user_id
WHERE m.place_id = @place_id
  AND (sqlc.narg('query')::text IS NULL
       OR u.username ILIKE sqlc.narg('query')::text || '%'
       OR u.display_name ILIKE sqlc.narg('query')::text || '%'
       OR m.nickname ILIKE sqlc.narg('query')::text || '%')
ORDER BY m.joined_at, m.user_id
LIMIT @lim OFFSET @off;

-- name: ListMembersAmong :many
SELECT user_id, timeout_until FROM place_members
WHERE place_id = @place_id AND user_id = ANY(@user_ids::uuid[]);

-- name: GetMemberTimeout :one
SELECT timeout_until FROM place_members WHERE place_id = @place_id AND user_id = @user_id;

-- name: SetMemberTimeout :exec
UPDATE place_members SET timeout_until = sqlc.narg('timeout_until')::timestamptz
WHERE place_id = @place_id AND user_id = @user_id;

-- name: RemoveMember :execrows
DELETE FROM place_members WHERE place_id = @place_id AND user_id = @user_id;

-- name: RemoveAllUserMemberships :exec
DELETE FROM place_members WHERE user_id = @user_id;

-- name: UpdateMemberNickname :exec
UPDATE place_members SET nickname = NULLIF(@nickname::text, '')
WHERE place_id = @place_id AND user_id = @user_id;

-- name: ListMemberRoleIDs :many
SELECT user_id, role_id FROM member_roles
WHERE place_id = @place_id AND user_id = ANY(@user_ids::uuid[]);

-- name: AddMemberRole :exec
INSERT INTO member_roles (place_id, user_id, role_id)
VALUES (@place_id, @user_id, @role_id)
ON CONFLICT DO NOTHING;

-- name: RemoveMemberRole :execrows
DELETE FROM member_roles WHERE place_id = @place_id AND user_id = @user_id AND role_id = @role_id;

-- name: GetMemberRolesForPermissions :many
-- Returns the default role plus every role assigned to the member.
SELECT r.id, r.permissions, r.position, r.is_default
FROM roles r
WHERE r.place_id = @place_id
  AND (r.is_default
       OR r.id IN (SELECT mr.role_id FROM member_roles mr
                   WHERE mr.place_id = @place_id AND mr.user_id = @user_id));
