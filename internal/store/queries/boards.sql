-- name: CreateBoard :one
INSERT INTO boards (id, place_id, parent_id, kind, slug, name, description, position,
                    reply_mode, solutions_enabled, is_nsfw)
VALUES (@id, @place_id, @parent_id, @kind, @slug, @name, @description, @position,
        @reply_mode, @solutions_enabled, @is_nsfw)
RETURNING *;

-- name: GetBoard :one
SELECT * FROM boards WHERE id = @id;

-- name: ListBoards :many
SELECT * FROM boards WHERE place_id = @place_id ORDER BY position, created_at;

-- name: CountBoards :one
SELECT count(*) FROM boards WHERE place_id = @place_id;

-- name: NextBoardPosition :one
SELECT (COALESCE(MAX(position), -1) + 1)::integer FROM boards
WHERE place_id = @place_id AND parent_id IS NOT DISTINCT FROM sqlc.narg('parent_id')::uuid;

-- name: UpdateBoard :one
-- Nullable params leave the column unchanged; set_parent controls parent_id separately so
-- a board can be moved to the root.
UPDATE boards
SET name              = COALESCE(sqlc.narg('name')::text, name),
    slug              = COALESCE(sqlc.narg('slug')::text, slug),
    description       = COALESCE(sqlc.narg('description')::text, description),
    position          = COALESCE(sqlc.narg('position')::integer, position),
    reply_mode        = COALESCE(sqlc.narg('reply_mode')::text, reply_mode),
    solutions_enabled = COALESCE(sqlc.narg('solutions_enabled')::boolean, solutions_enabled),
    is_nsfw           = COALESCE(sqlc.narg('is_nsfw')::boolean, is_nsfw),
    parent_id         = CASE WHEN @set_parent::boolean THEN sqlc.narg('parent_id')::uuid ELSE parent_id END,
    updated_at        = now()
WHERE id = @id
RETURNING *;

-- name: DeleteBoard :exec
DELETE FROM boards WHERE id = @id;

-- name: CountLiveBoardTopics :one
SELECT count(*) FROM topics WHERE board_id = @board_id AND deleted_at IS NULL;

-- name: SetPublicBoards :exec
UPDATE boards SET is_public = (id = ANY(@public_ids::uuid[]))
WHERE place_id = @place_id AND is_public <> (id = ANY(@public_ids::uuid[]));

-- name: AdjustBoardCounts :exec
UPDATE boards
SET topic_count  = topic_count + @topics::integer,
    post_count   = post_count + @posts::integer,
    last_post_at = CASE WHEN @touch::boolean THEN now() ELSE last_post_at END
WHERE id = @id;

-- name: ListBoardOverwrites :many
SELECT * FROM board_overwrites WHERE place_id = @place_id;

-- name: UpsertBoardOverwrite :one
INSERT INTO board_overwrites (board_id, role_id, place_id, allow, deny)
VALUES (@board_id, @role_id, @place_id, @allow, @deny)
ON CONFLICT (board_id, role_id) DO UPDATE SET allow = EXCLUDED.allow, deny = EXCLUDED.deny
RETURNING *;

-- name: DeleteBoardOverwrite :execrows
DELETE FROM board_overwrites WHERE board_id = @board_id AND role_id = @role_id;
