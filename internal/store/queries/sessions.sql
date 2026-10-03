-- name: CreateSession :one
INSERT INTO sessions (id, user_id, refresh_token_hash, user_agent, ip_address, expires_at)
VALUES (@id, @user_id, @refresh_token_hash, @user_agent, @ip_address, @expires_at)
RETURNING *;

-- name: GetActiveSessionUser :one
SELECT sqlc.embed(u)
FROM sessions s
JOIN users u ON u.id = s.user_id
WHERE s.id = @id
  AND s.revoked_at IS NULL
  AND s.expires_at > now()
  AND u.deleted_at IS NULL;

-- name: GetSessionForUpdate :one
SELECT * FROM sessions WHERE id = @id FOR UPDATE;

-- name: RotateSession :one
UPDATE sessions
SET refresh_token_hash = @refresh_token_hash,
    expires_at         = @expires_at,
    user_agent         = @user_agent,
    ip_address         = @ip_address,
    last_used_at       = now()
WHERE id = @id AND revoked_at IS NULL
RETURNING *;

-- name: ListActiveSessions :many
SELECT * FROM sessions
WHERE user_id = @user_id AND revoked_at IS NULL AND expires_at > now()
ORDER BY last_used_at DESC;

-- name: RevokeSession :execrows
UPDATE sessions SET revoked_at = now()
WHERE id = @id AND user_id = @user_id AND revoked_at IS NULL;

-- name: RevokeSessionByID :exec
UPDATE sessions SET revoked_at = now() WHERE id = @id AND revoked_at IS NULL;

-- name: RevokeAllUserSessions :exec
UPDATE sessions SET revoked_at = now() WHERE user_id = @user_id AND revoked_at IS NULL;

-- name: RevokeOtherUserSessions :exec
UPDATE sessions SET revoked_at = now()
WHERE user_id = @user_id AND id <> @keep_session_id AND revoked_at IS NULL;
