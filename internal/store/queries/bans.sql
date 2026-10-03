-- name: UpsertBan :exec
INSERT INTO place_bans (place_id, user_id, reason, banned_by, expires_at)
VALUES (@place_id, @user_id, @reason, @banned_by, @expires_at)
ON CONFLICT (place_id, user_id) DO UPDATE
SET reason = EXCLUDED.reason, banned_by = EXCLUDED.banned_by, expires_at = EXCLUDED.expires_at, created_at = now();

-- name: IsBanned :one
-- Temporary bans stop applying once they expire.
SELECT EXISTS (
    SELECT 1 FROM place_bans
    WHERE place_id = @place_id AND user_id = @user_id AND (expires_at IS NULL OR expires_at > now())
);

-- name: ListBans :many
SELECT sqlc.embed(b), sqlc.embed(u)
FROM place_bans b
JOIN users u ON u.id = b.user_id
WHERE b.place_id = @place_id AND (b.expires_at IS NULL OR b.expires_at > now())
ORDER BY b.created_at DESC
LIMIT @lim OFFSET @off;

-- name: DeleteBan :execrows
-- Expired bans count as already lifted.
DELETE FROM place_bans
WHERE place_id = @place_id AND user_id = @user_id AND (expires_at IS NULL OR expires_at > now());
