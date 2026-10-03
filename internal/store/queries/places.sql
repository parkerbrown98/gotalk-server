-- name: CreatePlace :one
INSERT INTO places (id, slug, name, description, visibility, is_nsfw, locale, owner_id, member_count)
VALUES (@id, @slug, @name, @description, @visibility, @is_nsfw, @locale, @owner_id, 0)
RETURNING *;

-- name: GetPlaceByID :one
SELECT * FROM places WHERE id = @id AND deleted_at IS NULL;

-- name: GetPlaceBySlug :one
SELECT * FROM places WHERE lower(slug) = lower(@slug) AND deleted_at IS NULL;

-- name: LockPlace :one
SELECT id FROM places WHERE id = @id AND deleted_at IS NULL FOR UPDATE;

-- name: ListDiscoverablePlaces :many
SELECT * FROM places
WHERE deleted_at IS NULL
  AND visibility <> 'private'
  AND (sqlc.narg('query')::text IS NULL
       OR name ILIKE '%' || sqlc.narg('query')::text || '%'
       OR description ILIKE '%' || sqlc.narg('query')::text || '%')
ORDER BY member_count DESC, id
LIMIT @lim OFFSET @off;

-- name: ListUserPlaces :many
SELECT p.* FROM places p
JOIN place_members m ON m.place_id = p.id
WHERE m.user_id = @user_id AND p.deleted_at IS NULL
ORDER BY m.joined_at;

-- name: UpdatePlace :one
-- Nullable params leave the column unchanged; empty icon/banner URLs clear them.
UPDATE places
SET name        = COALESCE(sqlc.narg('name')::text, name),
    description = COALESCE(sqlc.narg('description')::text, description),
    slug        = COALESCE(sqlc.narg('slug')::text, slug),
    visibility  = COALESCE(sqlc.narg('visibility')::text, visibility),
    is_nsfw     = COALESCE(sqlc.narg('is_nsfw')::boolean, is_nsfw),
    locale      = COALESCE(sqlc.narg('locale')::text, locale),
    icon_url    = CASE WHEN sqlc.narg('icon_url')::text IS NULL THEN icon_url
                       ELSE NULLIF(sqlc.narg('icon_url')::text, '') END,
    banner_url  = CASE WHEN sqlc.narg('banner_url')::text IS NULL THEN banner_url
                       ELSE NULLIF(sqlc.narg('banner_url')::text, '') END,
    updated_at  = now()
WHERE id = @id AND deleted_at IS NULL
RETURNING *;

-- name: TransferPlaceOwnership :exec
UPDATE places SET owner_id = @owner_id, updated_at = now() WHERE id = @id;

-- name: SoftDeletePlace :exec
UPDATE places SET deleted_at = now(), updated_at = now() WHERE id = @id;

-- name: AdjustMemberCount :exec
UPDATE places SET member_count = member_count + @delta WHERE id = @id;

-- name: DecrementMemberCountsForUser :exec
UPDATE places SET member_count = member_count - 1
WHERE id IN (SELECT place_id FROM place_members WHERE user_id = @user_id);
