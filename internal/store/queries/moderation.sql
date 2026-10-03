-- name: CreateReport :one
INSERT INTO reports (id, place_id, reporter_id, target_type, post_id, target_user_id, reason, details, content_snapshot)
VALUES (@id, @place_id, @reporter_id, @target_type, @post_id, @target_user_id, @reason, @details, @content_snapshot)
RETURNING *;

-- name: GetReport :one
SELECT * FROM reports WHERE id = @id AND place_id = @place_id;

-- name: ListReports :many
SELECT * FROM reports
WHERE place_id = @place_id AND (sqlc.narg('status')::text IS NULL OR status = sqlc.narg('status')::text)
ORDER BY id DESC
LIMIT @lim OFFSET @off;

-- name: ResolveReport :one
UPDATE reports
SET status = @status, resolved_by = @resolved_by, resolved_at = now(), resolution_note = @resolution_note
WHERE id = @id AND place_id = @place_id
RETURNING *;

-- name: CreateAuditEntry :exec
INSERT INTO audit_log (id, place_id, actor_id, action, target_type, target_id, reason, metadata)
VALUES (@id, @place_id, @actor_id, @action, @target_type, @target_id, @reason, @metadata);

-- name: ListAuditLog :many
SELECT * FROM audit_log
WHERE place_id = @place_id
  AND (sqlc.narg('action')::text IS NULL OR action = sqlc.narg('action')::text
       OR action LIKE sqlc.narg('action')::text || '.%')
  AND (sqlc.narg('actor_id')::uuid IS NULL OR actor_id = sqlc.narg('actor_id')::uuid)
  AND (sqlc.narg('target_id')::uuid IS NULL OR target_id = sqlc.narg('target_id')::uuid)
ORDER BY id DESC
LIMIT @lim OFFSET @off;
