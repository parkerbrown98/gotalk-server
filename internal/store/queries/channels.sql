-- name: CreateChannel :one
INSERT INTO channels (id, place_id, parent_id, kind, name, topic, position, is_nsfw, owner_id,
                      thread_message_id, dm_key, user_limit)
VALUES (@id, @place_id, @parent_id, @kind, @name, @topic, @position, @is_nsfw, @owner_id,
        @thread_message_id, @dm_key, @user_limit)
RETURNING *;

-- name: GetChannel :one
SELECT * FROM channels WHERE id = @id;

-- name: GetChannelForUpdate :one
SELECT * FROM channels WHERE id = @id FOR UPDATE;

-- name: GetDMChannelByKey :one
SELECT * FROM channels WHERE dm_key = @dm_key;

-- name: ListPlaceChannels :many
-- Categories, text and voice channels; threads are listed per parent channel.
SELECT * FROM channels
WHERE place_id = @place_id AND kind IN ('category', 'text', 'voice')
ORDER BY position, created_at;

-- name: NextChannelPosition :one
SELECT (COALESCE(MAX(position), -1) + 1)::integer FROM channels
WHERE place_id = @place_id AND kind IN ('category', 'text', 'voice')
  AND parent_id IS NOT DISTINCT FROM sqlc.narg('parent_id')::uuid;

-- name: UpdateChannel :one
-- Nullable params leave the column unchanged; set_parent controls parent_id separately so
-- a channel can be moved out of its category.
UPDATE channels
SET name        = COALESCE(sqlc.narg('name')::text, name),
    topic       = COALESCE(sqlc.narg('topic')::text, topic),
    position    = COALESCE(sqlc.narg('position')::integer, position),
    is_nsfw     = COALESCE(sqlc.narg('is_nsfw')::boolean, is_nsfw),
    is_archived = COALESCE(sqlc.narg('is_archived')::boolean, is_archived),
    user_limit  = COALESCE(sqlc.narg('user_limit')::integer, user_limit),
    owner_id    = COALESCE(sqlc.narg('owner_id')::uuid, owner_id),
    parent_id   = CASE WHEN @set_parent::boolean THEN sqlc.narg('parent_id')::uuid ELSE parent_id END,
    updated_at  = now()
WHERE id = @id
RETURNING *;

-- name: MoveChildChannelsToRoot :exec
UPDATE channels SET parent_id = NULL, updated_at = now() WHERE parent_id = @parent_id AND kind IN ('text', 'voice');

-- name: DeleteChannel :exec
DELETE FROM channels WHERE id = @id;

-- name: RecordChannelMessage :exec
UPDATE channels
SET last_message_id = @message_id,
    last_message_at = @created_at,
    message_count   = message_count + 1,
    is_archived     = false
WHERE id = @id;

-- name: RefreshChannelAfterDelete :exec
UPDATE channels
SET message_count   = GREATEST(message_count - 1, 0),
    last_message_id = (SELECT m.id FROM messages m WHERE m.channel_id = @id ORDER BY m.id DESC LIMIT 1),
    last_message_at = (SELECT m.created_at FROM messages m WHERE m.channel_id = @id ORDER BY m.id DESC LIMIT 1)
WHERE channels.id = @id;

-- name: ListThreads :many
SELECT * FROM channels
WHERE parent_id = @parent_id AND kind = 'thread' AND is_archived = @archived
ORDER BY last_message_at DESC NULLS LAST, id DESC
LIMIT @lim OFFSET @off;

-- name: ListThreadsByMessage :many
SELECT * FROM channels WHERE thread_message_id = ANY(@message_ids::uuid[]);

-- name: ListChannelOverwrites :many
SELECT * FROM channel_overwrites WHERE place_id = @place_id;

-- name: UpsertChannelOverwrite :one
INSERT INTO channel_overwrites (channel_id, role_id, place_id, allow, deny)
VALUES (@channel_id, @role_id, @place_id, @allow, @deny)
ON CONFLICT (channel_id, role_id) DO UPDATE SET allow = EXCLUDED.allow, deny = EXCLUDED.deny
RETURNING *;

-- name: DeleteChannelOverwrite :execrows
DELETE FROM channel_overwrites WHERE channel_id = @channel_id AND role_id = @role_id;

-- name: AddChannelRecipient :execrows
INSERT INTO channel_recipients (channel_id, user_id) VALUES (@channel_id, @user_id)
ON CONFLICT DO NOTHING;

-- name: RemoveChannelRecipient :execrows
DELETE FROM channel_recipients WHERE channel_id = @channel_id AND user_id = @user_id;

-- name: IsChannelRecipient :one
SELECT EXISTS (SELECT 1 FROM channel_recipients WHERE channel_id = @channel_id AND user_id = @user_id);

-- name: ListChannelRecipients :many
SELECT * FROM channel_recipients
WHERE channel_id = ANY(@channel_ids::uuid[])
ORDER BY joined_at, user_id;

-- name: ListUserDMChannels :many
-- Most recently active first.
SELECT c.* FROM channels c
JOIN channel_recipients r ON r.channel_id = c.id
WHERE r.user_id = @user_id
ORDER BY COALESCE(c.last_message_at, c.created_at) DESC, c.id DESC
LIMIT @lim OFFSET @off;

-- name: ListDMPartnerIDs :many
SELECT DISTINCT r2.user_id
FROM channel_recipients r1
JOIN channel_recipients r2 ON r2.channel_id = r1.channel_id
WHERE r1.user_id = @user_id AND r2.user_id <> @user_id;

-- name: SharesPlace :one
SELECT EXISTS (
    SELECT 1 FROM place_members a
    JOIN place_members b ON b.place_id = a.place_id
    JOIN places p ON p.id = a.place_id
    WHERE a.user_id = @user_a AND b.user_id = @user_b AND p.deleted_at IS NULL
);

-- name: FilterPresenceAudience :many
-- Returns the subset of user_ids whose presence the viewer may see: themselves, people
-- sharing a place with them, and their DM partners.
SELECT u.id FROM users u
WHERE u.id = ANY(@user_ids::uuid[])
  AND (u.id = @viewer_id
       OR EXISTS (SELECT 1 FROM place_members a
                  JOIN place_members b ON b.place_id = a.place_id
                  JOIN places p ON p.id = a.place_id
                  WHERE a.user_id = @viewer_id AND b.user_id = u.id AND p.deleted_at IS NULL)
       OR EXISTS (SELECT 1 FROM channel_recipients r1
                  JOIN channel_recipients r2 ON r2.channel_id = r1.channel_id
                  WHERE r1.user_id = @viewer_id AND r2.user_id = u.id));

-- name: ListUserPlaceIDs :many
SELECT m.place_id FROM place_members m
JOIN places p ON p.id = m.place_id
WHERE m.user_id = @user_id AND p.deleted_at IS NULL;

-- name: ListPlaceMemberIDs :many
SELECT user_id FROM place_members WHERE place_id = @place_id;

-- name: ReassignGroupDMOwnership :exec
-- Hands group DMs owned by a departing user to the longest-standing other recipient.
UPDATE channels
SET owner_id = (SELECT cr.user_id FROM channel_recipients cr
                WHERE cr.channel_id = channels.id AND cr.user_id <> @user_id
                ORDER BY cr.joined_at, cr.user_id LIMIT 1),
    updated_at = now()
WHERE kind = 'group_dm' AND owner_id = @user_id;

-- name: LeaveAllGroupDMs :many
DELETE FROM channel_recipients r
USING channels c
WHERE r.channel_id = c.id AND c.kind = 'group_dm' AND r.user_id = @user_id
RETURNING r.channel_id;

-- name: ListChannelReads :many
SELECT * FROM channel_reads WHERE user_id = @user_id AND channel_id = ANY(@channel_ids::uuid[]);

-- name: AckChannel :exec
-- Read positions never move backwards.
INSERT INTO channel_reads (user_id, channel_id, last_read_message_id, mention_count)
VALUES (@user_id, @channel_id, @message_id, 0)
ON CONFLICT (user_id, channel_id) DO UPDATE
SET last_read_message_id = GREATEST(channel_reads.last_read_message_id, EXCLUDED.last_read_message_id),
    updated_at           = now();

-- name: RecountMentions :one
-- Counts unread messages that notified the user: every message from someone else in a
-- DM, mentions and replies elsewhere.
UPDATE channel_reads r
SET mention_count = (
    SELECT count(*)::integer FROM messages m
    WHERE m.channel_id = r.channel_id
      AND m.id > r.last_read_message_id
      AND CASE WHEN @is_dm::boolean THEN m.author_id IS DISTINCT FROM r.user_id
               ELSE r.user_id = ANY(m.mention_ids) END
)
WHERE r.user_id = @user_id AND r.channel_id = @channel_id
RETURNING *;

-- name: AddMentions :exec
-- Counts message_id as an unread mention for each user who has not read past it.
INSERT INTO channel_reads (user_id, channel_id, mention_count)
SELECT unnest(@user_ids::uuid[]), @channel_id::uuid, 1
ON CONFLICT (user_id, channel_id) DO UPDATE SET mention_count = channel_reads.mention_count + 1
WHERE channel_reads.last_read_message_id IS NULL OR channel_reads.last_read_message_id < @message_id::uuid;

-- name: ListChannelReceipts :many
SELECT * FROM channel_reads
WHERE channel_id = @channel_id AND last_read_message_id IS NOT NULL
ORDER BY user_id;

-- name: DeleteChannelReadsFor :exec
DELETE FROM channel_reads WHERE channel_id = @channel_id AND user_id = @user_id;
