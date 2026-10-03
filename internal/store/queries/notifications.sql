-- name: CreateNotification :batchone
-- Duplicate reaction and direct-message notifications are silently skipped (no row is
-- returned).
INSERT INTO notifications (id, user_id, kind, place_id, topic_id, post_id, channel_id, message_id, actor_id, data)
VALUES (@id, @user_id, @kind, @place_id, @topic_id, @post_id, @channel_id, @message_id, @actor_id, @data)
ON CONFLICT DO NOTHING
RETURNING *;

-- name: MarkChannelNotificationsRead :exec
-- Reading a channel up to a message also reads the notifications it caused.
UPDATE notifications SET read_at = now()
WHERE user_id = @user_id AND channel_id = @channel_id AND read_at IS NULL
  AND (message_id IS NULL OR message_id <= @message_id);

-- name: ListNotifications :many
SELECT * FROM notifications
WHERE user_id = @user_id AND (NOT @unread_only::boolean OR read_at IS NULL)
ORDER BY id DESC
LIMIT @lim OFFSET @off;

-- name: CountUnreadNotifications :one
SELECT count(*) FROM notifications WHERE user_id = @user_id AND read_at IS NULL;

-- name: MarkNotificationRead :execrows
UPDATE notifications SET read_at = COALESCE(read_at, now()) WHERE id = @id AND user_id = @user_id;

-- name: MarkAllNotificationsRead :exec
UPDATE notifications SET read_at = now() WHERE user_id = @user_id AND read_at IS NULL;

-- name: DeleteNotification :execrows
DELETE FROM notifications WHERE id = @id AND user_id = @user_id;

-- name: UpsertSubscription :exec
INSERT INTO subscriptions (user_id, target_type, target_id, place_id, level)
VALUES (@user_id, @target_type, @target_id, @place_id, @level)
ON CONFLICT (user_id, target_type, target_id) DO UPDATE SET level = EXCLUDED.level, updated_at = now();

-- name: EnsureSubscription :exec
-- Adds a subscription without overriding an explicit choice.
INSERT INTO subscriptions (user_id, target_type, target_id, place_id, level)
VALUES (@user_id, @target_type, @target_id, @place_id, @level)
ON CONFLICT DO NOTHING;

-- name: DeleteSubscription :exec
DELETE FROM subscriptions WHERE user_id = @user_id AND target_type = @target_type AND target_id = @target_id;

-- name: DeleteTargetSubscriptions :exec
DELETE FROM subscriptions WHERE target_type = @target_type AND target_id = @target_id;

-- name: ListUserSubscriptions :many
SELECT * FROM subscriptions WHERE user_id = @user_id AND target_id = ANY(@target_ids::uuid[]);

-- name: ListSubscriptionsFor :many
SELECT * FROM subscriptions WHERE user_id = ANY(@user_ids::uuid[]) AND target_id = ANY(@target_ids::uuid[]);

-- name: ListWatchers :many
SELECT DISTINCT user_id FROM subscriptions WHERE level = 'watching' AND target_id = ANY(@target_ids::uuid[]);

-- name: UpsertDraft :exec
INSERT INTO drafts (user_id, key, data) VALUES (@user_id, @key, @data)
ON CONFLICT (user_id, key) DO UPDATE SET data = EXCLUDED.data, updated_at = now();

-- name: GetDraft :one
SELECT * FROM drafts WHERE user_id = @user_id AND key = @key;

-- name: ListDrafts :many
SELECT * FROM drafts WHERE user_id = @user_id ORDER BY updated_at DESC;

-- name: CountDrafts :one
SELECT count(*) FROM drafts WHERE user_id = @user_id;

-- name: DeleteDraft :execrows
DELETE FROM drafts WHERE user_id = @user_id AND key = @key;
