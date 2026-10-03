-- name: UpsertBan :exec
INSERT INTO place_bans (place_id, user_id, reason, banned_by)
VALUES (@place_id, @user_id, @reason, @banned_by)
ON CONFLICT (place_id, user_id) DO UPDATE
SET reason = EXCLUDED.reason, banned_by = EXCLUDED.banned_by, created_at = now();

-- name: IsBanned :one
SELECT EXISTS (SELECT 1 FROM place_bans WHERE place_id = @place_id AND user_id = @user_id);

-- name: ListBans :many
SELECT sqlc.embed(b), sqlc.embed(u)
FROM place_bans b
JOIN users u ON u.id = b.user_id
WHERE b.place_id = @place_id
ORDER BY b.created_at DESC
LIMIT @lim OFFSET @off;

-- name: DeleteBan :execrows
DELETE FROM place_bans WHERE place_id = @place_id AND user_id = @user_id;
