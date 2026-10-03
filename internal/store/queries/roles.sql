-- name: CreateRole :one
INSERT INTO roles (id, place_id, name, color, position, permissions, is_default)
VALUES (@id, @place_id, @name, @color, @position, @permissions, @is_default)
RETURNING *;

-- name: GetRole :one
SELECT * FROM roles WHERE id = @id AND place_id = @place_id;

-- name: GetDefaultRole :one
SELECT * FROM roles WHERE place_id = @place_id AND is_default;

-- name: ListRoles :many
SELECT * FROM roles WHERE place_id = @place_id ORDER BY position DESC, created_at;

-- name: MaxRolePosition :one
SELECT COALESCE(MAX(position), 0)::integer FROM roles WHERE place_id = @place_id;

-- name: ShiftRolePositions :exec
-- Shifts non-default roles whose position lies within [from_pos, to_pos] by delta.
UPDATE roles SET position = position + @delta::integer, updated_at = now()
WHERE place_id = @place_id
  AND NOT is_default
  AND position BETWEEN @from_pos::integer AND @to_pos::integer;

-- name: UpdateRole :one
UPDATE roles
SET name        = COALESCE(sqlc.narg('name')::text, name),
    color       = COALESCE(sqlc.narg('color')::integer, color),
    permissions = COALESCE(sqlc.narg('permissions')::bigint, permissions),
    position    = COALESCE(sqlc.narg('position')::integer, position),
    updated_at  = now()
WHERE id = @id AND place_id = @place_id
RETURNING *;

-- name: DeleteRole :exec
DELETE FROM roles WHERE id = @id AND place_id = @place_id AND NOT is_default;
