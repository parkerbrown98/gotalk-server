-- name: SetPlaceInIndex :exec
UPDATE places SET in_index = @in_index WHERE id = @id AND in_index <> @in_index;

-- name: SetBoardsInIndex :exec
UPDATE boards SET in_index = (id = ANY(@ids::uuid[]))
WHERE place_id = @place_id AND in_index <> (id = ANY(@ids::uuid[]));

-- name: ClearBoardsInIndex :exec
UPDATE boards SET in_index = false WHERE place_id = @place_id AND in_index;

-- name: ListIndexCandidatePlaceIDs :many
-- Places whose index state can change when directory listing is switched on or off.
SELECT id FROM places WHERE deleted_at IS NULL AND (in_index OR indexable) ORDER BY id;

-- name: CountPlacesInIndex :one
SELECT count(*) FROM places WHERE in_index AND deleted_at IS NULL;

-- name: EmitIndexChange :exec
-- A later event for the same object replaces the earlier one and is sequenced again.
INSERT INTO index_changes (object_type, object_id, type, place_id, board_id, topic_id, user_id)
VALUES (@object_type, @object_id, @type, sqlc.narg('place_id'), sqlc.narg('board_id'),
        sqlc.narg('topic_id'), sqlc.narg('user_id'))
ON CONFLICT (object_type, object_id) DO UPDATE
SET type     = EXCLUDED.type,
    place_id = EXCLUDED.place_id,
    board_id = EXCLUDED.board_id,
    topic_id = EXCLUDED.topic_id,
    user_id  = EXCLUDED.user_id,
    at       = now(),
    ord      = clock_timestamp(),
    seq      = NULL;

-- name: EmitUserTopicDeletes :execrows
-- topic.delete for every indexed topic the user opened (their opening post leaves the index).
INSERT INTO index_changes (object_type, object_id, type, place_id, board_id, topic_id)
SELECT 'topic', t.id, 'topic.delete', t.place_id, t.board_id, t.id
FROM topics t
JOIN boards b ON b.id = t.board_id
WHERE t.author_id = @user_id AND t.deleted_at IS NULL AND b.in_index
ON CONFLICT (object_type, object_id) DO UPDATE
SET type = EXCLUDED.type, place_id = EXCLUDED.place_id, board_id = EXCLUDED.board_id,
    topic_id = EXCLUDED.topic_id, user_id = NULL, at = now(),
    ord = clock_timestamp(), seq = NULL;

-- name: EmitUserTopicUpserts :execrows
-- topic.upsert for every indexable topic with a live post by the user.
INSERT INTO index_changes (object_type, object_id, type, place_id, board_id, topic_id)
SELECT DISTINCT 'topic', t.id, 'topic.upsert', t.place_id, t.board_id, t.id
FROM posts p
JOIN topics t ON t.id = p.topic_id
JOIN boards b ON b.id = t.board_id
JOIN posts op ON op.topic_id = t.id AND op.post_number = 1
JOIN users ou ON ou.id = op.author_id
WHERE p.author_id = @user_id AND p.deleted_at IS NULL AND t.deleted_at IS NULL AND b.in_index
  AND op.deleted_at IS NULL AND ou.deleted_at IS NULL AND NOT ou.exclude_from_index
ON CONFLICT (object_type, object_id) DO UPDATE
SET type = EXCLUDED.type, place_id = EXCLUDED.place_id, board_id = EXCLUDED.board_id,
    topic_id = EXCLUDED.topic_id, user_id = NULL, at = now(),
    ord = clock_timestamp(), seq = NULL;

-- name: TouchUserTopics :exec
UPDATE topics SET index_updated_at = clock_timestamp()
WHERE id IN (SELECT p.topic_id FROM posts p WHERE p.author_id = @user_id AND p.deleted_at IS NULL)
  AND deleted_at IS NULL;

-- name: UserHasIndexedPosts :one
SELECT EXISTS (
    SELECT 1 FROM posts p JOIN boards b ON b.id = p.board_id
    WHERE p.author_id = @user_id AND p.deleted_at IS NULL AND b.in_index
)::boolean;

-- name: TouchIndexTopic :one
-- Bumps the topic's index timestamp and reports whether it is indexable now.
UPDATE topics t SET index_updated_at = clock_timestamp()
WHERE t.id = @id
RETURNING t.id, t.place_id, t.board_id, (t.deleted_at IS NOT NULL)::boolean AS deleted,
    COALESCE((SELECT b.in_index FROM boards b WHERE b.id = t.board_id), false)::boolean AS board_in_index,
    EXISTS (
        SELECT 1 FROM posts op JOIN users u ON u.id = op.author_id
        WHERE op.topic_id = t.id AND op.post_number = 1 AND op.deleted_at IS NULL
          AND u.deleted_at IS NULL AND NOT u.exclude_from_index
    )::boolean AS opening_visible;

-- name: BoardInIndex :one
SELECT in_index FROM boards WHERE id = @id;

-- name: LockIndexFeed :one
-- Serializes sequencing and returns the feed's high-water marks.
SELECT index_feed_seq, index_feed_pruned_seq FROM instance_settings WHERE id = 1 FOR UPDATE;

-- name: SequenceIndexChanges :execrows
UPDATE index_changes c SET seq = p.n + sqlc.arg('base')::bigint
FROM (
    SELECT object_type, object_id, row_number() OVER (ORDER BY ord, object_type, object_id) AS n
    FROM index_changes WHERE seq IS NULL
) p
WHERE c.object_type = p.object_type AND c.object_id = p.object_id;

-- name: AdvanceIndexFeed :exec
UPDATE instance_settings SET index_feed_seq = index_feed_seq + @n::bigint WHERE id = 1;

-- name: ListIndexChanges :many
SELECT * FROM index_changes WHERE seq > @after::bigint ORDER BY seq LIMIT @lim;

-- name: PruneIndexChanges :one
-- Removes sequenced events older than the retention window and returns the highest
-- sequence number removed (0 when none).
WITH gone AS (
    DELETE FROM index_changes WHERE seq IS NOT NULL AND at < @before RETURNING seq
)
SELECT COALESCE(max(seq), 0)::bigint FROM gone;

-- name: SetIndexFeedPruned :exec
UPDATE instance_settings SET index_feed_pruned_seq = GREATEST(index_feed_pruned_seq, @seq::bigint) WHERE id = 1;

-- name: ListIndexPlaces :many
-- The index read queries apply the topic and post rules of the index protocol
-- (docs/api-index.md); internal/service/index.go applies the place and board rules and
-- re-checks the place conditions filtered here.
SELECT * FROM places
WHERE deleted_at IS NULL AND visibility = 'public' AND indexable AND NOT is_nsfw
  AND (sqlc.narg('after')::uuid IS NULL OR id > sqlc.narg('after')::uuid)
ORDER BY id
LIMIT @lim;

-- name: ListIndexBoardTopics :many
SELECT sqlc.embed(t), u.id AS author_id, u.username AS author_username, u.display_name AS author_display_name,
    (SELECT count(*) FROM posts p JOIN users pu ON pu.id = p.author_id
     WHERE p.topic_id = t.id AND p.post_number > 1 AND p.deleted_at IS NULL
       AND pu.deleted_at IS NULL AND NOT pu.exclude_from_index)::integer AS reply_count
FROM topics t
JOIN posts op ON op.topic_id = t.id AND op.post_number = 1
JOIN users u ON u.id = op.author_id
WHERE t.board_id = @board_id AND t.deleted_at IS NULL
  AND op.deleted_at IS NULL AND u.deleted_at IS NULL AND NOT u.exclude_from_index
  AND (sqlc.narg('since')::timestamptz IS NULL OR t.index_updated_at >= sqlc.narg('since')::timestamptz)
  AND (sqlc.narg('after_at')::timestamptz IS NULL
       OR (t.index_updated_at, t.id) > (sqlc.narg('after_at')::timestamptz, sqlc.narg('after_id')::uuid))
ORDER BY t.index_updated_at, t.id
LIMIT @lim;

-- name: GetIndexTopic :one
SELECT sqlc.embed(t), u.id AS author_id, u.username AS author_username, u.display_name AS author_display_name,
    (SELECT count(*) FROM posts p JOIN users pu ON pu.id = p.author_id
     WHERE p.topic_id = t.id AND p.post_number > 1 AND p.deleted_at IS NULL
       AND pu.deleted_at IS NULL AND NOT pu.exclude_from_index)::integer AS reply_count
FROM topics t
JOIN posts op ON op.topic_id = t.id AND op.post_number = 1
JOIN users u ON u.id = op.author_id
WHERE t.id = @id AND t.deleted_at IS NULL
  AND op.deleted_at IS NULL AND u.deleted_at IS NULL AND NOT u.exclude_from_index;

-- name: ListIndexPosts :many
SELECT p.id, p.post_number, p.parent_id, p.content, p.created_at, p.edited_at,
    u.id AS author_id, u.username AS author_username, u.display_name AS author_display_name
FROM posts p
JOIN users u ON u.id = p.author_id
WHERE p.topic_id = @topic_id AND p.deleted_at IS NULL AND u.deleted_at IS NULL AND NOT u.exclude_from_index
  AND p.post_number > @after::integer
ORDER BY p.post_number
LIMIT @lim;

-- name: SetUserExcludeFromIndex :one
UPDATE users SET exclude_from_index = @exclude, updated_at = now()
WHERE id = @id AND deleted_at IS NULL
RETURNING *;

-- name: GetDirectoryKeys :one
SELECT directory_signing_key, directory_previous_key FROM instance_settings WHERE id = 1;

-- name: EnsureDirectorySigningKey :one
UPDATE instance_settings SET directory_signing_key = COALESCE(directory_signing_key, @key::bytea)
WHERE id = 1
RETURNING directory_signing_key;

-- name: RotateDirectorySigningKey :exec
UPDATE instance_settings SET directory_previous_key = @previous, directory_signing_key = @key WHERE id = 1;

-- name: RevokeDirectoryPreviousKey :execrows
UPDATE instance_settings SET directory_previous_key = NULL WHERE id = 1 AND directory_previous_key IS NOT NULL;

-- name: ClaimIndexPing :one
-- Records a ping unless one was sent within the interval; returns the previous ping time
-- (NULL for the first). No row means another ping is too recent.
UPDATE instance_settings s SET index_last_ping_at = now()
FROM (SELECT index_last_ping_at AS prev FROM instance_settings WHERE id = 1 FOR UPDATE) old
WHERE s.id = 1
  AND (old.prev IS NULL OR old.prev <= now() - make_interval(secs => @interval_secs::float8))
RETURNING old.prev;

-- name: GetLastIndexPing :one
SELECT index_last_ping_at FROM instance_settings WHERE id = 1;

-- name: CreateInstanceAuditEntry :exec
INSERT INTO instance_audit_log (id, actor_id, action, target_type, target_id, metadata)
VALUES (@id, @actor_id, @action, @target_type, @target_id, @metadata);

-- name: ListInstanceAuditLog :many
SELECT * FROM instance_audit_log
WHERE (sqlc.narg('before')::uuid IS NULL OR id < sqlc.narg('before')::uuid)
ORDER BY id DESC
LIMIT @lim;
