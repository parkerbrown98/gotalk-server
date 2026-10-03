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

-- name: ListUsersByIDs :many
SELECT * FROM users WHERE id = ANY(@ids::uuid[]) AND deleted_at IS NULL;

-- name: ListUsersByUsernames :many
SELECT * FROM users WHERE lower(username) = ANY(@usernames::text[]) AND deleted_at IS NULL;

-- name: DeleteUserNotifications :exec
DELETE FROM notifications WHERE user_id = @user_id;

-- name: DeleteUserSubscriptions :exec
DELETE FROM subscriptions WHERE user_id = @user_id;

-- name: DeleteUserDrafts :exec
DELETE FROM drafts WHERE user_id = @user_id;

-- name: DeleteUserTopicReads :exec
DELETE FROM topic_reads WHERE user_id = @user_id;

-- name: RemoveUserReactions :exec
WITH removed AS (
    DELETE FROM post_reactions WHERE user_id = @user_id RETURNING post_id
)
UPDATE posts SET reaction_count = posts.reaction_count - r.n
FROM (SELECT post_id, count(*)::integer AS n FROM removed GROUP BY post_id) r
WHERE posts.id = r.post_id;
