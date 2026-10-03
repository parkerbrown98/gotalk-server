-- name: CreateAPIToken :one
INSERT INTO api_tokens (id, user_id, application_id, kind, name, token_hash, token_hint, scopes, expires_at)
VALUES (@id, @user_id, @application_id, @kind, @name, @token_hash, @token_hint, @scopes, @expires_at)
RETURNING *;

-- name: GetAPITokenUser :one
SELECT sqlc.embed(t), sqlc.embed(u)
FROM api_tokens t
JOIN users u ON u.id = t.user_id
WHERE t.token_hash = @token_hash
  AND t.revoked_at IS NULL
  AND (t.expires_at IS NULL OR t.expires_at > now())
  AND u.deleted_at IS NULL;

-- name: TouchAPIToken :exec
-- Usage is recorded at most once a minute to keep authentication cheap.
UPDATE api_tokens SET last_used_at = now()
WHERE id = @id AND (last_used_at IS NULL OR last_used_at < now() - interval '1 minute');

-- name: ListPersonalTokens :many
SELECT * FROM api_tokens
WHERE user_id = @user_id AND kind = 'personal' AND revoked_at IS NULL
ORDER BY created_at DESC;

-- name: CountActivePersonalTokens :one
SELECT count(*) FROM api_tokens
WHERE user_id = @user_id AND kind = 'personal' AND revoked_at IS NULL
  AND (expires_at IS NULL OR expires_at > now());

-- name: RevokePersonalToken :execrows
UPDATE api_tokens SET revoked_at = now()
WHERE id = @id AND user_id = @user_id AND kind = 'personal' AND revoked_at IS NULL;

-- name: RevokeUserTokens :many
UPDATE api_tokens SET revoked_at = now()
WHERE user_id = @user_id AND revoked_at IS NULL
RETURNING id;

-- name: RevokePersonalTokens :many
UPDATE api_tokens SET revoked_at = now()
WHERE user_id = @user_id AND kind = 'personal' AND revoked_at IS NULL
RETURNING id;

-- name: RevokeApplicationTokens :many
UPDATE api_tokens SET revoked_at = now()
WHERE application_id = @application_id AND revoked_at IS NULL
RETURNING id;
