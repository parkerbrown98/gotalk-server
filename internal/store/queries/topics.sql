-- name: CreateTopic :one
INSERT INTO topics (id, place_id, board_id, author_id, title, slug, tags, last_poster_id)
VALUES (@id, @place_id, @board_id, @author_id, @title, @slug, @tags, @author_id)
RETURNING *;

-- name: GetTopic :one
SELECT * FROM topics WHERE id = @id;

-- name: GetTopicForUpdate :one
SELECT * FROM topics WHERE id = @id FOR UPDATE;

-- name: ListTopicsByIDs :many
SELECT * FROM topics WHERE id = ANY(@ids::uuid[]);

-- name: ListBoardTopics :many
-- Pinned topics first, then most recently active. Archived topics are excluded unless asked for.
SELECT * FROM topics
WHERE board_id = @board_id
  AND deleted_at IS NULL
  AND (@include_archived::boolean OR NOT is_archived)
  AND (sqlc.narg('tag')::text IS NULL OR sqlc.narg('tag')::text = ANY(tags))
ORDER BY is_pinned DESC, last_post_at DESC, id DESC
LIMIT @lim OFFSET @off;

-- name: ListPlaceTopics :many
-- Latest activity across the given (visible) boards of a place.
SELECT * FROM topics
WHERE place_id = @place_id
  AND board_id = ANY(@board_ids::uuid[])
  AND deleted_at IS NULL
  AND NOT is_archived
  AND (sqlc.narg('tag')::text IS NULL OR sqlc.narg('tag')::text = ANY(tags))
ORDER BY last_post_at DESC, id DESC
LIMIT @lim OFFSET @off;

-- name: ClaimPostNumber :one
-- Reserves the next post number and records the activity on the (now locked) topic row.
UPDATE topics
SET last_post_number = last_post_number + 1,
    post_count       = post_count + 1,
    last_post_at     = now(),
    last_poster_id   = @poster_id
WHERE id = @id
RETURNING last_post_number;

-- name: UpdateTopic :one
UPDATE topics
SET title       = COALESCE(sqlc.narg('title')::text, title),
    slug        = COALESCE(sqlc.narg('slug')::text, slug),
    tags        = COALESCE(sqlc.narg('tags')::text[], tags),
    is_pinned   = COALESCE(sqlc.narg('is_pinned')::boolean, is_pinned),
    is_locked   = COALESCE(sqlc.narg('is_locked')::boolean, is_locked),
    is_archived = COALESCE(sqlc.narg('is_archived')::boolean, is_archived),
    board_id    = COALESCE(sqlc.narg('board_id')::uuid, board_id),
    updated_at  = now()
WHERE id = @id
RETURNING *;

-- name: SetTopicSolution :one
UPDATE topics SET solution_post_id = sqlc.narg('post_id')::uuid, updated_at = now()
WHERE id = @id
RETURNING *;

-- name: SoftDeleteTopic :exec
UPDATE topics SET deleted_at = now(), deleted_by = @deleted_by, updated_at = now() WHERE id = @id;

-- name: AdjustTopicPostCount :exec
UPDATE topics SET post_count = post_count + @delta::integer WHERE id = @id;

-- name: MoveTopicPosts :exec
UPDATE posts SET board_id = @board_id WHERE topic_id = @topic_id;

-- name: ListPlaceTags :many
-- Tags used by live topics in the given boards, most used first. prefix must not contain
-- LIKE wildcards (tags are restricted to [a-z0-9-]).
SELECT tag::text AS tag, count(*)::bigint AS topic_count
FROM (
    SELECT unnest(t.tags) AS tag FROM topics t
    WHERE t.place_id = @place_id AND t.deleted_at IS NULL AND t.board_id = ANY(@board_ids::uuid[])
) s
WHERE sqlc.narg('prefix')::text IS NULL OR tag LIKE sqlc.narg('prefix')::text || '%'
GROUP BY tag
ORDER BY count(*) DESC, tag
LIMIT @lim;

-- name: MarkTopicRead :one
-- Records an open of the topic and advances the read position (never backwards).
-- seen_post_number is the topic's last post number at this open.
INSERT INTO topic_reads (user_id, topic_id, last_read_post_number, opened_at, first_opened_at, seen_post_number)
VALUES (@user_id, @topic_id, @post_number, now(), now(), @seen_post_number)
ON CONFLICT (user_id, topic_id) DO UPDATE
SET last_read_post_number = GREATEST(topic_reads.last_read_post_number, EXCLUDED.last_read_post_number),
    opened_at             = now(),
    first_opened_at       = COALESCE(topic_reads.first_opened_at, now()),
    seen_post_number      = GREATEST(topic_reads.seen_post_number, EXCLUDED.seen_post_number),
    updated_at            = now()
RETURNING *;

-- name: ListTopicReads :many
SELECT topic_id, last_read_post_number, opened_at, seen_post_number FROM topic_reads
WHERE user_id = @user_id AND topic_id = ANY(@topic_ids::uuid[]);
