-- name: GetUserByEmail :one
SELECT * FROM users WHERE lower(email) = lower(@email) AND deleted_at IS NULL;

-- name: CreateEmailToken :exec
INSERT INTO email_tokens (token_hash, user_id, purpose, email, expires_at)
VALUES (@token_hash, @user_id, @purpose, @email, @expires_at);

-- name: LatestEmailTokenAt :one
SELECT created_at FROM email_tokens
WHERE user_id = @user_id AND purpose = @purpose
ORDER BY created_at DESC
LIMIT 1;

-- name: ConsumeEmailToken :one
-- Marks a live token used. Concurrent consumers race on the row; only one gets it back.
UPDATE email_tokens SET used_at = now()
WHERE token_hash = @token_hash AND purpose = @purpose AND used_at IS NULL AND expires_at > now()
RETURNING *;

-- name: InvalidateEmailTokens :exec
UPDATE email_tokens SET used_at = now()
WHERE user_id = @user_id AND purpose = @purpose AND used_at IS NULL;

-- name: DeleteExpiredEmailTokens :execrows
DELETE FROM email_tokens WHERE expires_at < @before;

-- name: MarkEmailVerified :one
-- Only verifies the address the token was sent to, in case the account's email changed.
UPDATE users SET email_verified_at = COALESCE(email_verified_at, now()), updated_at = now()
WHERE id = @id AND lower(email) = lower(@email) AND deleted_at IS NULL
RETURNING *;

-- name: SetInstanceAdmin :exec
UPDATE users SET is_instance_admin = true, updated_at = now() WHERE id = @id;

-- name: EnqueueMail :one
INSERT INTO mail_outbox (id, kind, to_address, subject, text_body, html_body, expires_at)
VALUES (@id, @kind, @to_address, @subject, @text_body, @html_body, @expires_at)
RETURNING *;

-- name: ClaimMail :many
-- Locks due messages for this replica; SKIP LOCKED lets every replica drain the queue
-- without sending anything twice.
UPDATE mail_outbox
SET locked_until = now() + make_interval(secs => @lock_seconds::integer),
    attempts     = attempts + 1
WHERE id IN (
    SELECT id FROM mail_outbox
    WHERE status = 'pending' AND next_attempt_at <= now() AND expires_at > now()
      AND (locked_until IS NULL OR locked_until < now())
    ORDER BY next_attempt_at
    LIMIT @lim
    FOR UPDATE SKIP LOCKED
)
RETURNING *;

-- name: FinishMail :exec
UPDATE mail_outbox
SET status          = @status,
    next_attempt_at = @next_attempt_at,
    last_error      = @last_error,
    locked_until    = NULL,
    completed_at    = CASE WHEN @status::text = 'pending' THEN NULL ELSE now() END,
    text_body       = CASE WHEN @status::text = 'pending' THEN text_body ELSE '' END,
    html_body       = CASE WHEN @status::text = 'pending' THEN html_body ELSE '' END
WHERE id = @id;

-- name: ExpireMail :execrows
UPDATE mail_outbox
SET status = 'failed', last_error = 'expired before it could be sent', completed_at = now(),
    text_body = '', html_body = '', locked_until = NULL
WHERE status = 'pending' AND expires_at <= now();

-- name: DeleteOldMail :execrows
DELETE FROM mail_outbox WHERE completed_at < @before;

-- name: ListMailOutbox :many
SELECT * FROM mail_outbox ORDER BY created_at DESC LIMIT @lim;
