-- name: CreateUser :one
INSERT INTO users (id, username, email, password_hash, display_name, is_instance_admin)
VALUES (@id, @username, @email, @password_hash, @display_name, @is_instance_admin)
RETURNING *;

-- name: GetUserByID :one
SELECT * FROM users WHERE id = @id AND deleted_at IS NULL;

-- name: GetUserByUsername :one
SELECT * FROM users WHERE lower(username) = lower(@username) AND deleted_at IS NULL;

-- name: GetUserByLogin :one
SELECT * FROM users
WHERE (lower(username) = lower(@login) OR lower(email) = lower(@login))
  AND deleted_at IS NULL
LIMIT 1;

-- name: UpdateUserProfile :one
-- Nullable params leave the column unchanged; an empty avatar_url clears it.
UPDATE users
SET display_name = COALESCE(sqlc.narg('display_name')::text, display_name),
    bio          = COALESCE(sqlc.narg('bio')::text, bio),
    pronouns     = COALESCE(sqlc.narg('pronouns')::text, pronouns),
    avatar_url   = CASE WHEN sqlc.narg('avatar_url')::text IS NULL THEN avatar_url
                        ELSE NULLIF(sqlc.narg('avatar_url')::text, '') END,
    updated_at   = now()
WHERE id = @id AND deleted_at IS NULL
RETURNING *;

-- name: UpdateUserPassword :exec
UPDATE users SET password_hash = @password_hash, updated_at = now() WHERE id = @id;

-- name: SoftDeleteUser :exec
UPDATE users
SET deleted_at    = now(),
    email         = 'deleted-' || id::text || '@invalid',
    password_hash = '',
    display_name  = '',
    bio           = '',
    pronouns      = '',
    avatar_url    = NULL,
    updated_at    = now()
WHERE id = @id;

-- name: CountOwnedPlaces :one
SELECT count(*) FROM places WHERE owner_id = @owner_id AND deleted_at IS NULL;

-- name: CountInstanceAdmins :one
SELECT count(*) FROM users WHERE is_instance_admin AND deleted_at IS NULL;
