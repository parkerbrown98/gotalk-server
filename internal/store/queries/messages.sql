-- name: CreateMessage :one
INSERT INTO messages (id, channel_id, place_id, author_id, content, reply_to_id, mention_ids)
VALUES (@id, @channel_id, @place_id, @author_id, @content, @reply_to_id, @mention_ids)
RETURNING *;

-- name: GetMessage :one
SELECT * FROM messages WHERE id = @id;

-- name: ListMessagesBefore :many
-- Newest first; callers reverse the page into chronological order.
SELECT * FROM messages
WHERE channel_id = @channel_id AND (sqlc.narg('before')::uuid IS NULL OR id < sqlc.narg('before')::uuid)
ORDER BY id DESC
LIMIT @lim;

-- name: ListMessagesAfter :many
SELECT * FROM messages
WHERE channel_id = @channel_id AND id > @after
ORDER BY id
LIMIT @lim;

-- name: ListMessagesByIDs :many
SELECT * FROM messages WHERE id = ANY(@ids::uuid[]);

-- name: UpdateMessageContent :one
UPDATE messages
SET content     = @content,
    mention_ids = @mention_ids,
    edit_count  = edit_count + 1,
    edited_at   = now(),
    updated_at  = now()
WHERE id = @id
RETURNING *;

-- name: DeleteMessage :execrows
DELETE FROM messages WHERE id = @id;

-- name: SetMessagePinned :one
UPDATE messages
SET is_pinned  = @pinned::boolean,
    pinned_at  = CASE WHEN @pinned::boolean THEN now() END,
    pinned_by  = CASE WHEN @pinned::boolean THEN sqlc.narg('pinned_by')::uuid END,
    updated_at = now()
WHERE id = @id
RETURNING *;

-- name: CountPins :one
SELECT count(*) FROM messages WHERE channel_id = @channel_id AND is_pinned;

-- name: ListPins :many
SELECT * FROM messages WHERE channel_id = @channel_id AND is_pinned ORDER BY pinned_at DESC, id DESC;

-- name: CreateMessageRevision :exec
INSERT INTO message_revisions (id, message_id, content) VALUES (@id, @message_id, @content);

-- name: ListMessageRevisions :many
SELECT * FROM message_revisions WHERE message_id = @message_id ORDER BY created_at DESC, id DESC;

-- name: AddMessageReaction :execrows
INSERT INTO message_reactions (message_id, user_id, emoji) VALUES (@message_id, @user_id, @emoji)
ON CONFLICT DO NOTHING;

-- name: RemoveMessageReaction :execrows
DELETE FROM message_reactions WHERE message_id = @message_id AND user_id = @user_id AND emoji = @emoji;

-- name: MessageHasEmoji :one
SELECT EXISTS (SELECT 1 FROM message_reactions WHERE message_id = @message_id AND emoji = @emoji);

-- name: CountDistinctMessageReactions :one
SELECT count(DISTINCT emoji) FROM message_reactions WHERE message_id = @message_id;

-- name: MessageReactionSummaries :many
-- viewer_id may be uuid.Nil.
SELECT message_id, emoji, count(*)::integer AS count, bool_or(user_id = @viewer_id)::boolean AS me
FROM message_reactions
WHERE message_id = ANY(@message_ids::uuid[])
GROUP BY message_id, emoji
ORDER BY message_id, min(created_at), emoji;

-- name: ListMessageReactionUsers :many
SELECT sqlc.embed(u)
FROM message_reactions r
JOIN users u ON u.id = r.user_id
WHERE r.message_id = @message_id AND r.emoji = @emoji AND u.deleted_at IS NULL
ORDER BY r.created_at, u.id
LIMIT @lim OFFSET @off;
