-- name: CreateWebhook :one
INSERT INTO webhooks (id, place_id, name, url, secret, events, is_active, created_by)
VALUES (@id, @place_id, @name, @url, @secret, @events, @is_active, @created_by)
RETURNING *;

-- name: GetWebhook :one
SELECT * FROM webhooks WHERE id = @id;

-- name: ListWebhooks :many
SELECT * FROM webhooks WHERE place_id = @place_id ORDER BY created_at, id;

-- name: CountWebhooks :one
SELECT count(*) FROM webhooks WHERE place_id = @place_id;

-- name: UpdateWebhook :one
-- Nullable params leave the column unchanged. Re-activating clears the failure streak.
UPDATE webhooks
SET name                 = COALESCE(sqlc.narg('name')::text, name),
    url                  = COALESCE(sqlc.narg('url')::text, url),
    events               = COALESCE(sqlc.narg('events')::text[], events),
    is_active            = COALESCE(sqlc.narg('is_active')::boolean, is_active),
    consecutive_failures = CASE WHEN sqlc.narg('is_active')::boolean THEN 0 ELSE consecutive_failures END,
    disabled_reason      = CASE WHEN sqlc.narg('is_active')::boolean IS NULL THEN disabled_reason
                                WHEN sqlc.narg('is_active')::boolean THEN ''
                                ELSE 'disabled by a moderator' END,
    updated_at           = now()
WHERE id = @id
RETURNING *;

-- name: RotateWebhookSecret :one
UPDATE webhooks SET secret = @secret, updated_at = now() WHERE id = @id RETURNING *;

-- name: DeleteWebhook :exec
DELETE FROM webhooks WHERE id = @id;

-- name: ListWebhooksForEvent :many
SELECT * FROM webhooks WHERE place_id = @place_id AND is_active AND @event::text = ANY(events);

-- name: CreateWebhookDelivery :one
INSERT INTO webhook_deliveries (id, webhook_id, event, payload)
VALUES (@id, @webhook_id, @event, @payload)
RETURNING *;

-- name: ClaimWebhookDeliveries :many
-- Locks due deliveries of active webhooks for this replica. SKIP LOCKED lets several
-- replicas drain the queue without delivering anything twice.
UPDATE webhook_deliveries
SET locked_until = now() + make_interval(secs => @lock_seconds::integer),
    attempts     = attempts + 1
WHERE id IN (
    SELECT d.id
    FROM webhook_deliveries d
    JOIN webhooks w ON w.id = d.webhook_id
    WHERE d.status = 'pending' AND w.is_active
      AND d.next_attempt_at <= now()
      AND (d.locked_until IS NULL OR d.locked_until < now())
    ORDER BY d.next_attempt_at
    LIMIT @lim
    FOR UPDATE OF d SKIP LOCKED
)
RETURNING *;

-- name: FinishWebhookAttempt :one
UPDATE webhook_deliveries
SET status          = @status,
    next_attempt_at = @next_attempt_at,
    locked_until    = NULL,
    response_status = @response_status,
    last_error      = @last_error,
    duration_ms     = @duration_ms,
    completed_at    = CASE WHEN @status::text = 'pending' THEN NULL ELSE now() END
WHERE id = @id
RETURNING *;

-- name: RecordWebhookSuccess :exec
UPDATE webhooks
SET consecutive_failures = 0, last_delivery_at = now(), last_success_at = now()
WHERE id = @id;

-- name: RecordWebhookAttemptFailure :exec
UPDATE webhooks SET last_delivery_at = now() WHERE id = @id;

-- name: RecordWebhookFailure :one
UPDATE webhooks
SET consecutive_failures = consecutive_failures + 1, last_delivery_at = now()
WHERE id = @id
RETURNING consecutive_failures;

-- name: DisableWebhook :execrows
UPDATE webhooks
SET is_active = false, disabled_reason = @disabled_reason, updated_at = now()
WHERE id = @id AND is_active;

-- name: CancelPendingDeliveries :exec
UPDATE webhook_deliveries
SET status = 'failed', last_error = @reason, completed_at = now(), locked_until = NULL
WHERE webhook_id = @webhook_id AND status = 'pending';

-- name: GetWebhookDelivery :one
SELECT * FROM webhook_deliveries WHERE id = @id AND webhook_id = @webhook_id;

-- name: ListWebhookDeliveries :many
SELECT * FROM webhook_deliveries
WHERE webhook_id = @webhook_id
  AND (sqlc.narg('status')::text IS NULL OR status = sqlc.narg('status')::text)
ORDER BY id DESC
LIMIT @lim OFFSET @off;

-- name: DeleteOldWebhookDeliveries :execrows
DELETE FROM webhook_deliveries WHERE completed_at < @before;
