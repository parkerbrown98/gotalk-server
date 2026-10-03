-- name: CreatePost :one
INSERT INTO posts (id, topic_id, place_id, board_id, author_id, parent_id, post_number, content)
VALUES (@id, @topic_id, @place_id, @board_id, @author_id, @parent_id, @post_number, @content)
RETURNING *;

-- name: GetPost :one
SELECT * FROM posts WHERE id = @id;

-- name: GetPostByNumber :one
SELECT * FROM posts WHERE topic_id = @topic_id AND post_number = @post_number;

-- name: ListTopicPosts :many
SELECT * FROM posts WHERE topic_id = @topic_id ORDER BY post_number LIMIT @lim OFFSET @off;

-- name: ListTopicPostsThreaded :many
-- Depth-first order: each reply directly follows its parent, siblings oldest first.
WITH RECURSIVE tree AS (
    SELECT r.id, ARRAY[r.post_number] AS path
    FROM posts r
    WHERE r.topic_id = @topic_id AND r.parent_id IS NULL
    UNION ALL
    SELECT c.id, tree.path || c.post_number
    FROM posts c
    JOIN tree ON c.parent_id = tree.id
)
SELECT sqlc.embed(p), (cardinality(tree.path) - 1)::integer AS depth
FROM tree
JOIN posts p ON p.id = tree.id
ORDER BY tree.path
LIMIT @lim OFFSET @off;

-- name: UpdatePostContent :one
UPDATE posts
SET content    = @content,
    edit_count = edit_count + 1,
    edited_at  = now(),
    updated_at = now()
WHERE id = @id
RETURNING *;

-- name: SoftDeletePost :execrows
UPDATE posts SET deleted_at = now(), deleted_by = @deleted_by, updated_at = now()
WHERE id = @id AND deleted_at IS NULL;

-- name: CreatePostRevision :exec
INSERT INTO post_revisions (id, post_id, editor_id, content) VALUES (@id, @post_id, @editor_id, @content);

-- name: ListPostRevisions :many
SELECT * FROM post_revisions WHERE post_id = @post_id ORDER BY created_at DESC, id DESC;

-- name: AddReaction :execrows
INSERT INTO post_reactions (post_id, user_id, emoji) VALUES (@post_id, @user_id, @emoji)
ON CONFLICT DO NOTHING;

-- name: RemoveReaction :execrows
DELETE FROM post_reactions WHERE post_id = @post_id AND user_id = @user_id AND emoji = @emoji;

-- name: AdjustReactionCount :exec
UPDATE posts SET reaction_count = reaction_count + @delta::integer WHERE id = @id;

-- name: PostHasEmoji :one
SELECT EXISTS (SELECT 1 FROM post_reactions WHERE post_id = @post_id AND emoji = @emoji);

-- name: CountDistinctReactions :one
SELECT count(DISTINCT emoji) FROM post_reactions WHERE post_id = @post_id;

-- name: ReactionSummaries :many
-- viewer_id may be uuid.Nil for anonymous callers.
SELECT post_id, emoji, count(*)::integer AS count, bool_or(user_id = @viewer_id)::boolean AS me
FROM post_reactions
WHERE post_id = ANY(@post_ids::uuid[])
GROUP BY post_id, emoji
ORDER BY post_id, min(created_at), emoji;

-- name: ListReactionUsers :many
SELECT sqlc.embed(u)
FROM post_reactions r
JOIN users u ON u.id = r.user_id
WHERE r.post_id = @post_id AND r.emoji = @emoji AND u.deleted_at IS NULL
ORDER BY r.created_at, u.id
LIMIT @lim OFFSET @off;

-- name: UpsertSearchDocument :exec
-- The opening post's document also carries the topic title; pass '' for replies.
INSERT INTO search_documents (post_id, vector)
VALUES (@post_id, setweight(to_tsvector('simple', @title::text), 'A') ||
                  setweight(to_tsvector('simple', @content::text), 'B'))
ON CONFLICT (post_id) DO UPDATE SET vector = EXCLUDED.vector, indexed_at = now();

-- name: DeleteSearchDocument :exec
DELETE FROM search_documents WHERE post_id = @post_id;
