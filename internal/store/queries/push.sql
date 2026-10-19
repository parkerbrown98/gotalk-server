-- name: GetPushGateway :one
SELECT * FROM push_gateway WHERE id = 1;

-- name: ReplacePushSigningKey :exec
UPDATE push_gateway SET signing_key = @key::bytea, updated_at = now() WHERE id = 1;

-- name: CountAllPushSubscriptions :one
SELECT count(*) FROM push_subscriptions;

-- name: EnsurePushSigningKey :one
-- Stores a generated signing key unless another replica stored one first.
UPDATE push_gateway SET signing_key = COALESCE(signing_key, @key::bytea), updated_at = now()
WHERE id = 1
RETURNING signing_key;

-- name: SavePushRegistration :exec
UPDATE push_gateway
SET gateway_url = @gateway_url, public_key = @public_key, instance_id = @instance_id, key_id = @key_id,
    status = @status, detail = @detail, updated_at = now()
WHERE id = 1;

-- name: SetPushGatewayStatus :exec
UPDATE push_gateway
SET status      = @status,
    detail      = @detail,
    verified_at = CASE WHEN @status::text = 'active' THEN now() ELSE verified_at END,
    updated_at  = now()
WHERE id = 1;

-- name: ClearPushRegistration :exec
UPDATE push_gateway
SET gateway_url = NULL, public_key = NULL, instance_id = NULL, key_id = NULL, verified_at = NULL,
    status = @status, detail = @detail, updated_at = now()
WHERE id = 1;

-- name: ClaimPushGatewayCheck :one
-- Lets one replica sync the registration per interval.
UPDATE push_gateway SET checked_at = now()
WHERE id = 1 AND (checked_at IS NULL OR checked_at < now() - make_interval(secs => @interval_seconds::float8))
RETURNING checked_at;

-- name: GetOldestInstanceAdmin :one
SELECT * FROM users WHERE is_instance_admin AND deleted_at IS NULL ORDER BY created_at LIMIT 1;

-- name: UpsertPushSubscription :one
INSERT INTO push_subscriptions (id, user_id, session_id, gateway_url, subscription_id, app_id, platform, p256dh, auth, mode, device_name)
VALUES (@id, @user_id, @session_id, @gateway_url, @subscription_id, @app_id, @platform, @p256dh, @auth, @mode, @device_name)
ON CONFLICT (user_id, gateway_url, subscription_id) DO UPDATE
SET session_id  = EXCLUDED.session_id,
    app_id      = EXCLUDED.app_id,
    platform    = EXCLUDED.platform,
    p256dh      = EXCLUDED.p256dh,
    auth        = EXCLUDED.auth,
    mode        = EXCLUDED.mode,
    device_name = EXCLUDED.device_name,
    updated_at  = now()
RETURNING *;

-- name: CountPushSubscriptions :one
SELECT count(*) FROM push_subscriptions WHERE user_id = @user_id;

-- name: ListPushSubscriptions :many
SELECT * FROM push_subscriptions WHERE user_id = @user_id ORDER BY created_at, id;

-- name: DeletePushSubscription :one
DELETE FROM push_subscriptions WHERE id = @id AND user_id = @user_id RETURNING *;

-- name: CountPushSubscriptionRefs :one
SELECT count(*) FROM push_subscriptions WHERE gateway_url = @gateway_url AND subscription_id = @subscription_id;

-- name: DeletePushSubscriptionsByGatewayID :execrows
-- The gateway reported the device's subscription dead; it is dead for every account.
DELETE FROM push_subscriptions WHERE gateway_url = @gateway_url AND subscription_id = @subscription_id;

-- name: ListDeliverableSubscriptions :many
-- The person's subscriptions on the current gateway whose session is still signed in,
-- optionally only some of them (after a partial failure).
SELECT ps.* FROM push_subscriptions ps
JOIN sessions s ON s.id = ps.session_id
WHERE ps.user_id = @user_id AND ps.gateway_url = @gateway_url
  AND s.revoked_at IS NULL AND s.expires_at > now()
  AND (sqlc.narg('only')::uuid[] IS NULL OR ps.id = ANY(sqlc.narg('only')::uuid[]))
ORDER BY ps.created_at, ps.id;

-- name: TouchPushSubscriptions :exec
UPDATE push_subscriptions SET last_push_at = now() WHERE id = ANY(@ids::uuid[]);

-- name: DeleteSignedOutPushSubscriptions :many
-- Subscriptions whose session was signed out or expired never receive pushes again.
DELETE FROM push_subscriptions ps
USING sessions s
WHERE s.id = ps.session_id AND (s.revoked_at IS NOT NULL OR s.expires_at <= now())
RETURNING ps.gateway_url, ps.subscription_id;

-- name: DeleteSessionPushSubscriptions :many
DELETE FROM push_subscriptions WHERE session_id = ANY(@session_ids::uuid[])
RETURNING gateway_url, subscription_id;

-- name: GetPushPreferences :one
SELECT * FROM push_preferences WHERE user_id = @user_id;

-- name: UpsertPushPreferences :one
INSERT INTO push_preferences (user_id, disabled_categories, quiet_hours_start, quiet_hours_end, timezone, paused_until)
VALUES (@user_id, @disabled_categories, @quiet_hours_start, @quiet_hours_end, @timezone, @paused_until)
ON CONFLICT (user_id) DO UPDATE
SET disabled_categories = EXCLUDED.disabled_categories,
    quiet_hours_start   = EXCLUDED.quiet_hours_start,
    quiet_hours_end     = EXCLUDED.quiet_hours_end,
    timezone            = EXCLUDED.timezone,
    paused_until        = EXCLUDED.paused_until,
    updated_at          = now()
RETURNING *;

-- name: EnqueuePushes :execrows
-- Queues the given notifications for people who have at least one subscription.
INSERT INTO push_outbox (notification_id, user_id)
SELECT n.id, n.user_id FROM notifications n
WHERE n.id = ANY(@ids::uuid[])
  AND EXISTS (SELECT 1 FROM push_subscriptions ps WHERE ps.user_id = n.user_id)
ON CONFLICT DO NOTHING;

-- name: ClaimPushOutbox :many
-- Locks due pushes for this replica; SKIP LOCKED lets every replica drain the queue without
-- pushing anything twice.
UPDATE push_outbox
SET locked_until = now() + make_interval(secs => @lock_seconds::integer),
    attempts     = attempts + 1
WHERE notification_id IN (
    SELECT notification_id FROM push_outbox
    WHERE next_attempt_at <= now() AND (locked_until IS NULL OR locked_until < now())
    ORDER BY next_attempt_at
    LIMIT @lim
    FOR UPDATE SKIP LOCKED
)
RETURNING *;

-- name: FinishPush :exec
DELETE FROM push_outbox WHERE notification_id = @notification_id;

-- name: RetryPush :exec
UPDATE push_outbox SET next_attempt_at = @next_attempt_at, pending = @pending, locked_until = NULL
WHERE notification_id = @notification_id;

-- name: DeleteStalePushes :execrows
DELETE FROM push_outbox WHERE created_at < @before;

-- name: GetNotification :one
SELECT * FROM notifications WHERE id = @id;
