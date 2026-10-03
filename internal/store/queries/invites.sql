-- name: CreateInvite :one
INSERT INTO invites (code, place_id, created_by, max_uses, expires_at)
VALUES (@code, @place_id, @created_by, @max_uses, @expires_at)
RETURNING *;

-- name: GetInvite :one
SELECT * FROM invites WHERE code = @code;

-- name: ListInvites :many
SELECT * FROM invites WHERE place_id = @place_id ORDER BY created_at DESC;

-- name: DeleteInvite :execrows
DELETE FROM invites WHERE code = @code AND place_id = @place_id;

-- name: ConsumeInvite :one
-- Atomically records one use of a still-valid invite.
UPDATE invites
SET uses = uses + 1
WHERE code = @code
  AND (max_uses IS NULL OR uses < max_uses)
  AND (expires_at IS NULL OR expires_at > now())
RETURNING *;
