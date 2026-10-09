-- name: RefreshTopicRanks :exec
-- Recomputes score, hot_rank and controversy for the given topics, or every topic of a
-- place. Score is up minus down votes, or the reactions on the opening post when the place has
-- voting off. hot_rank is sign(s) times log10 of max(abs(s), 1) plus age in units of 45000
-- seconds, where s is score plus twice the square root of the reply count: ten times the
-- engagement is worth 12.5 hours of age. Keep in sync with 00006_feeds.sql.
UPDATE topics t
SET score       = s.score,
    hot_rank    = sign(s.score + 2 * sqrt(GREATEST(t.post_count - 1, 0)))
                      * log(GREATEST(abs(s.score + 2 * sqrt(GREATEST(t.post_count - 1, 0))), 1))
                  + (extract(epoch FROM t.created_at)::float8 - 1704067200) / 45000,
    controversy = CASE
        WHEN s.voting_enabled AND t.upvotes > 0 AND t.downvotes > 0
            THEN power((t.upvotes + t.downvotes)::float8,
                       LEAST(t.upvotes, t.downvotes)::float8 / GREATEST(t.upvotes, t.downvotes)::float8)
        ELSE 0 END
FROM (
    SELECT tt.id,
           pl.voting_enabled,
           (CASE WHEN pl.voting_enabled THEN tt.upvotes - tt.downvotes
                 ELSE COALESCE(op.reaction_count, 0) END)::integer AS score
    FROM topics tt
    JOIN places pl ON pl.id = tt.place_id
    LEFT JOIN posts op ON op.topic_id = tt.id AND op.post_number = 1
    WHERE tt.id = ANY(@topic_ids::uuid[]) OR tt.place_id = sqlc.narg('place_id')::uuid
) s
WHERE t.id = s.id;

-- name: GetTopicVote :one
SELECT value FROM topic_votes WHERE topic_id = @topic_id AND user_id = @user_id;

-- name: UpsertTopicVote :exec
INSERT INTO topic_votes (topic_id, user_id, value)
VALUES (@topic_id, @user_id, @value)
ON CONFLICT (topic_id, user_id) DO UPDATE SET value = EXCLUDED.value, updated_at = now();

-- name: DeleteTopicVote :one
DELETE FROM topic_votes WHERE topic_id = @topic_id AND user_id = @user_id RETURNING value;

-- name: AdjustTopicVotes :exec
UPDATE topics
SET upvotes   = upvotes + @up_delta::integer,
    downvotes = downvotes + @down_delta::integer
WHERE id = @id;

-- name: ListTopicVotes :many
SELECT topic_id, value FROM topic_votes WHERE user_id = @user_id AND topic_id = ANY(@topic_ids::uuid[]);

-- name: RemoveUserTopicVotes :many
-- Erases a user's votes and takes them off the topic counts; returns the affected topics
-- so their ranks can be refreshed.
WITH removed AS (
    DELETE FROM topic_votes WHERE user_id = @user_id RETURNING topic_id, value
)
UPDATE topics
SET upvotes   = topics.upvotes - r.up,
    downvotes = topics.downvotes - r.down
FROM (
    SELECT topic_id,
           (count(*) FILTER (WHERE value = 1))::integer  AS up,
           (count(*) FILTER (WHERE value = -1))::integer AS down
    FROM removed GROUP BY topic_id
) r
WHERE topics.id = r.topic_id
RETURNING topics.id;

-- name: MarkTopicUnread :execrows
-- Clears the "opened" state but keeps the read position.
UPDATE topic_reads SET opened_at = NULL, updated_at = now()
WHERE user_id = @user_id AND topic_id = @topic_id AND opened_at IS NOT NULL;

-- name: MarkTopicsOpened :many
-- Records opens of several (already authorized) topics at once.
INSERT INTO topic_reads (user_id, topic_id, last_read_post_number, opened_at, first_opened_at, seen_post_number)
SELECT @user_id, t.id, 1, now(), now(), t.last_post_number
FROM topics t
WHERE t.id = ANY(@topic_ids::uuid[]) AND t.deleted_at IS NULL
ON CONFLICT (user_id, topic_id) DO UPDATE
SET opened_at        = now(),
    first_opened_at  = COALESCE(topic_reads.first_opened_at, now()),
    seen_post_number = GREATEST(topic_reads.seen_post_number, EXCLUDED.seen_post_number),
    updated_at       = now()
RETURNING topic_id;

-- name: MarkFeedRead :execrows
-- Marks the most recently active topics in the given boards fully read, up to before.
INSERT INTO topic_reads (user_id, topic_id, last_read_post_number, opened_at, first_opened_at, seen_post_number)
SELECT @user_id, t.id, t.last_post_number, now(), now(), t.last_post_number
FROM topics t
WHERE t.board_id = ANY(@board_ids::uuid[])
  AND t.deleted_at IS NULL
  AND t.last_post_at <= @before::timestamptz
ORDER BY t.last_post_at DESC
LIMIT @lim
ON CONFLICT (user_id, topic_id) DO UPDATE
SET last_read_post_number = GREATEST(topic_reads.last_read_post_number, EXCLUDED.last_read_post_number),
    opened_at             = now(),
    first_opened_at       = COALESCE(topic_reads.first_opened_at, now()),
    seen_post_number      = GREATEST(topic_reads.seen_post_number, EXCLUDED.seen_post_number),
    updated_at            = now();

-- name: ListOpeningPosts :many
SELECT topic_id, content FROM posts WHERE topic_id = ANY(@topic_ids::uuid[]) AND post_number = 1;

-- name: ListBoardsForPlaces :many
SELECT * FROM boards WHERE place_id = ANY(@place_ids::uuid[]) ORDER BY place_id, position, created_at;

-- name: ListBoardOverwritesForPlaces :many
SELECT * FROM board_overwrites WHERE place_id = ANY(@place_ids::uuid[]);

-- name: ListMemberRolesForPlaces :many
-- The default role of each place plus every role the user holds there.
SELECT r.* FROM roles r
WHERE r.place_id = ANY(@place_ids::uuid[])
  AND (r.is_default
       OR r.id IN (SELECT mr.role_id FROM member_roles mr
                   WHERE mr.user_id = @user_id AND mr.place_id = ANY(@place_ids::uuid[])));

-- name: ListForumMutes :many
-- A user's place and board level notification preferences in the given places.
SELECT * FROM subscriptions
WHERE user_id = @user_id AND place_id = ANY(@place_ids::uuid[]) AND target_type IN ('place', 'board');

-- name: ListPublicFeedBoards :many
-- Boards signed-out visitors can read, across every live public place. is_nsfw also counts
-- the place and the board's ancestors (boards nest at most three levels).
SELECT sqlc.embed(b),
       (b.is_nsfw OR pl.is_nsfw OR COALESCE(p1.is_nsfw, false) OR COALESCE(p2.is_nsfw, false))::boolean AS nsfw
FROM boards b
JOIN places pl ON pl.id = b.place_id
LEFT JOIN boards p1 ON p1.id = b.parent_id
LEFT JOIN boards p2 ON p2.id = p1.parent_id
WHERE b.is_public AND b.kind = 'board' AND pl.deleted_at IS NULL AND pl.visibility = 'public';

-- name: ListFeedPinned :many
-- Pinned topics for the top of a place feed, most recently active first.
SELECT t.* FROM topics t
WHERE t.board_id = ANY(@board_ids::uuid[])
  AND t.deleted_at IS NULL
  AND (sqlc.narg('place_id')::uuid IS NULL OR t.place_id = sqlc.narg('place_id')::uuid)
  AND (@include_archived::boolean OR NOT t.is_archived)
  AND NOT (@exclude_pinned::boolean AND t.is_pinned)
  AND (sqlc.narg('tag')::text IS NULL OR sqlc.narg('tag')::text = ANY(t.tags))
  AND (sqlc.narg('solved')::boolean IS NULL
       OR (t.solution_post_id IS NOT NULL) = sqlc.narg('solved')::boolean)
  AND (sqlc.narg('since')::timestamptz IS NULL OR t.created_at >= sqlc.narg('since')::timestamptz)
  AND NOT (@hide_read::boolean AND EXISTS (
        SELECT 1 FROM topic_reads r
        WHERE r.user_id = @viewer_id::uuid AND r.topic_id = t.id
          AND r.opened_at IS NOT NULL AND r.seen_post_number >= t.last_post_number))
  AND NOT COALESCE(
        (SELECT s.level = 'muted' FROM subscriptions s
         WHERE @apply_mutes::boolean AND s.user_id = @viewer_id::uuid
           AND s.target_type = 'topic' AND s.target_id = t.id),
        t.board_id = ANY(@muted_board_ids::uuid[]))
  AND t.is_pinned
ORDER BY t.last_post_at DESC, t.id DESC
LIMIT @lim;

-- name: ListFeedHot :many
-- Highest hot_rank first.
SELECT t.* FROM topics t
WHERE t.board_id = ANY(@board_ids::uuid[])
  AND t.deleted_at IS NULL
  AND (sqlc.narg('place_id')::uuid IS NULL OR t.place_id = sqlc.narg('place_id')::uuid)
  AND (@include_archived::boolean OR NOT t.is_archived)
  AND NOT (@exclude_pinned::boolean AND t.is_pinned)
  AND (sqlc.narg('tag')::text IS NULL OR sqlc.narg('tag')::text = ANY(t.tags))
  AND (sqlc.narg('solved')::boolean IS NULL
       OR (t.solution_post_id IS NOT NULL) = sqlc.narg('solved')::boolean)
  AND (sqlc.narg('since')::timestamptz IS NULL OR t.created_at >= sqlc.narg('since')::timestamptz)
  AND NOT (@hide_read::boolean AND EXISTS (
        SELECT 1 FROM topic_reads r
        WHERE r.user_id = @viewer_id::uuid AND r.topic_id = t.id
          AND r.opened_at IS NOT NULL AND r.seen_post_number >= t.last_post_number))
  AND NOT COALESCE(
        (SELECT s.level = 'muted' FROM subscriptions s
         WHERE @apply_mutes::boolean AND s.user_id = @viewer_id::uuid
           AND s.target_type = 'topic' AND s.target_id = t.id),
        t.board_id = ANY(@muted_board_ids::uuid[]))
  AND (sqlc.narg('after_id')::uuid IS NULL
       OR (t.hot_rank, t.id) < (sqlc.narg('after_rank')::float8, sqlc.narg('after_id')::uuid))
ORDER BY t.hot_rank DESC, t.id DESC
LIMIT @lim;

-- name: ListFeedNew :many
-- Newest topics first.
SELECT t.* FROM topics t
WHERE t.board_id = ANY(@board_ids::uuid[])
  AND t.deleted_at IS NULL
  AND (sqlc.narg('place_id')::uuid IS NULL OR t.place_id = sqlc.narg('place_id')::uuid)
  AND (@include_archived::boolean OR NOT t.is_archived)
  AND NOT (@exclude_pinned::boolean AND t.is_pinned)
  AND (sqlc.narg('tag')::text IS NULL OR sqlc.narg('tag')::text = ANY(t.tags))
  AND (sqlc.narg('solved')::boolean IS NULL
       OR (t.solution_post_id IS NOT NULL) = sqlc.narg('solved')::boolean)
  AND (sqlc.narg('since')::timestamptz IS NULL OR t.created_at >= sqlc.narg('since')::timestamptz)
  AND NOT (@hide_read::boolean AND EXISTS (
        SELECT 1 FROM topic_reads r
        WHERE r.user_id = @viewer_id::uuid AND r.topic_id = t.id
          AND r.opened_at IS NOT NULL AND r.seen_post_number >= t.last_post_number))
  AND NOT COALESCE(
        (SELECT s.level = 'muted' FROM subscriptions s
         WHERE @apply_mutes::boolean AND s.user_id = @viewer_id::uuid
           AND s.target_type = 'topic' AND s.target_id = t.id),
        t.board_id = ANY(@muted_board_ids::uuid[]))
  AND (sqlc.narg('after_id')::uuid IS NULL
       OR (t.created_at, t.id) < (sqlc.narg('after_time')::timestamptz, sqlc.narg('after_id')::uuid))
ORDER BY t.created_at DESC, t.id DESC
LIMIT @lim;

-- name: ListFeedActive :many
-- Latest reply first.
SELECT t.* FROM topics t
WHERE t.board_id = ANY(@board_ids::uuid[])
  AND t.deleted_at IS NULL
  AND (sqlc.narg('place_id')::uuid IS NULL OR t.place_id = sqlc.narg('place_id')::uuid)
  AND (@include_archived::boolean OR NOT t.is_archived)
  AND NOT (@exclude_pinned::boolean AND t.is_pinned)
  AND (sqlc.narg('tag')::text IS NULL OR sqlc.narg('tag')::text = ANY(t.tags))
  AND (sqlc.narg('solved')::boolean IS NULL
       OR (t.solution_post_id IS NOT NULL) = sqlc.narg('solved')::boolean)
  AND (sqlc.narg('since')::timestamptz IS NULL OR t.created_at >= sqlc.narg('since')::timestamptz)
  AND NOT (@hide_read::boolean AND EXISTS (
        SELECT 1 FROM topic_reads r
        WHERE r.user_id = @viewer_id::uuid AND r.topic_id = t.id
          AND r.opened_at IS NOT NULL AND r.seen_post_number >= t.last_post_number))
  AND NOT COALESCE(
        (SELECT s.level = 'muted' FROM subscriptions s
         WHERE @apply_mutes::boolean AND s.user_id = @viewer_id::uuid
           AND s.target_type = 'topic' AND s.target_id = t.id),
        t.board_id = ANY(@muted_board_ids::uuid[]))
  AND (sqlc.narg('after_id')::uuid IS NULL
       OR (t.last_post_at, t.id) < (sqlc.narg('after_time')::timestamptz, sqlc.narg('after_id')::uuid))
ORDER BY t.last_post_at DESC, t.id DESC
LIMIT @lim;

-- name: ListFeedTop :many
-- Highest score first; since bounds the window.
SELECT t.* FROM topics t
WHERE t.board_id = ANY(@board_ids::uuid[])
  AND t.deleted_at IS NULL
  AND (sqlc.narg('place_id')::uuid IS NULL OR t.place_id = sqlc.narg('place_id')::uuid)
  AND (@include_archived::boolean OR NOT t.is_archived)
  AND NOT (@exclude_pinned::boolean AND t.is_pinned)
  AND (sqlc.narg('tag')::text IS NULL OR sqlc.narg('tag')::text = ANY(t.tags))
  AND (sqlc.narg('solved')::boolean IS NULL
       OR (t.solution_post_id IS NOT NULL) = sqlc.narg('solved')::boolean)
  AND (sqlc.narg('since')::timestamptz IS NULL OR t.created_at >= sqlc.narg('since')::timestamptz)
  AND NOT (@hide_read::boolean AND EXISTS (
        SELECT 1 FROM topic_reads r
        WHERE r.user_id = @viewer_id::uuid AND r.topic_id = t.id
          AND r.opened_at IS NOT NULL AND r.seen_post_number >= t.last_post_number))
  AND NOT COALESCE(
        (SELECT s.level = 'muted' FROM subscriptions s
         WHERE @apply_mutes::boolean AND s.user_id = @viewer_id::uuid
           AND s.target_type = 'topic' AND s.target_id = t.id),
        t.board_id = ANY(@muted_board_ids::uuid[]))
  AND (sqlc.narg('after_id')::uuid IS NULL
       OR (t.score, t.id) < (sqlc.narg('after_score')::integer, sqlc.narg('after_id')::uuid))
ORDER BY t.score DESC, t.id DESC
LIMIT @lim;

-- name: ListFeedControversial :many
-- Many votes split between up and down; since bounds the window.
SELECT t.* FROM topics t
WHERE t.board_id = ANY(@board_ids::uuid[])
  AND t.deleted_at IS NULL
  AND (sqlc.narg('place_id')::uuid IS NULL OR t.place_id = sqlc.narg('place_id')::uuid)
  AND (@include_archived::boolean OR NOT t.is_archived)
  AND NOT (@exclude_pinned::boolean AND t.is_pinned)
  AND (sqlc.narg('tag')::text IS NULL OR sqlc.narg('tag')::text = ANY(t.tags))
  AND (sqlc.narg('solved')::boolean IS NULL
       OR (t.solution_post_id IS NOT NULL) = sqlc.narg('solved')::boolean)
  AND (sqlc.narg('since')::timestamptz IS NULL OR t.created_at >= sqlc.narg('since')::timestamptz)
  AND NOT (@hide_read::boolean AND EXISTS (
        SELECT 1 FROM topic_reads r
        WHERE r.user_id = @viewer_id::uuid AND r.topic_id = t.id
          AND r.opened_at IS NOT NULL AND r.seen_post_number >= t.last_post_number))
  AND NOT COALESCE(
        (SELECT s.level = 'muted' FROM subscriptions s
         WHERE @apply_mutes::boolean AND s.user_id = @viewer_id::uuid
           AND s.target_type = 'topic' AND s.target_id = t.id),
        t.board_id = ANY(@muted_board_ids::uuid[]))
  AND t.controversy > 0
  AND (sqlc.narg('after_id')::uuid IS NULL
       OR (t.controversy, t.id) < (sqlc.narg('after_rank')::float8, sqlc.narg('after_id')::uuid))
ORDER BY t.controversy DESC, t.id DESC
LIMIT @lim;

-- name: ListFeedRising :many
-- Recent topics (since bounds the candidates to the last 48 hours) gaining votes and replies
-- fastest relative to their age at as_of. as_of is fixed by the first page so pages agree.
WITH c AS (
    SELECT t.id,
           ((GREATEST(t.score, 0) + GREATEST(t.post_count - 1, 0) + 1)::float8
               / power(GREATEST(extract(epoch FROM (sqlc.arg('as_of')::timestamptz - t.created_at))::float8 / 3600, 0) + 2, 1.5)
           )::float8 AS rank
    FROM topics t
    WHERE t.board_id = ANY(@board_ids::uuid[])
      AND t.deleted_at IS NULL
      AND (sqlc.narg('place_id')::uuid IS NULL OR t.place_id = sqlc.narg('place_id')::uuid)
      AND (@include_archived::boolean OR NOT t.is_archived)
      AND NOT (@exclude_pinned::boolean AND t.is_pinned)
      AND (sqlc.narg('tag')::text IS NULL OR sqlc.narg('tag')::text = ANY(t.tags))
      AND (sqlc.narg('solved')::boolean IS NULL
           OR (t.solution_post_id IS NOT NULL) = sqlc.narg('solved')::boolean)
      AND (sqlc.narg('since')::timestamptz IS NULL OR t.created_at >= sqlc.narg('since')::timestamptz)
      AND NOT (@hide_read::boolean AND EXISTS (
            SELECT 1 FROM topic_reads r
            WHERE r.user_id = @viewer_id::uuid AND r.topic_id = t.id
              AND r.opened_at IS NOT NULL AND r.seen_post_number >= t.last_post_number))
      AND NOT COALESCE(
            (SELECT s.level = 'muted' FROM subscriptions s
             WHERE @apply_mutes::boolean AND s.user_id = @viewer_id::uuid
               AND s.target_type = 'topic' AND s.target_id = t.id),
            t.board_id = ANY(@muted_board_ids::uuid[]))
      AND t.created_at <= @as_of::timestamptz
)
SELECT sqlc.embed(t), c.rank
FROM c
JOIN topics t ON t.id = c.id
WHERE sqlc.narg('after_id')::uuid IS NULL
   OR (c.rank, c.id) < (sqlc.narg('after_rank')::float8, sqlc.narg('after_id')::uuid)
ORDER BY c.rank DESC, c.id DESC
LIMIT @lim;
