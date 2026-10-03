-- name: CreatePolicyDocument :one
INSERT INTO policy_documents (id, kind, version, title, content, summary, requires_consent, effective_at, published_by)
VALUES (@id, @kind,
        (SELECT COALESCE(max(version), 0) + 1 FROM policy_documents WHERE kind = @kind),
        @title, @content, @summary, @requires_consent, @effective_at, @published_by)
RETURNING *;

-- name: ListCurrentPolicies :many
-- The newest version of each policy that is already in effect.
SELECT DISTINCT ON (kind) *
FROM policy_documents
WHERE effective_at <= now()
ORDER BY kind, version DESC;

-- name: GetCurrentPolicy :one
SELECT * FROM policy_documents
WHERE kind = @kind AND effective_at <= now()
ORDER BY version DESC
LIMIT 1;

-- name: ListPolicyVersions :many
SELECT * FROM policy_documents WHERE kind = @kind ORDER BY version DESC;

-- name: GetPolicyVersion :one
SELECT * FROM policy_documents WHERE kind = @kind AND version = @version;

-- name: CreateConsentRecord :one
INSERT INTO consent_records (id, user_id, purpose, policy_version, granted, ip_address, user_agent)
VALUES (@id, @user_id, @purpose, @policy_version, @granted, @ip_address, @user_agent)
RETURNING *;

-- name: ListLatestConsents :many
SELECT DISTINCT ON (purpose) *
FROM consent_records
WHERE user_id = @user_id
ORDER BY purpose, created_at DESC, id DESC;

-- name: ListConsentHistory :many
SELECT * FROM consent_records
WHERE user_id = @user_id
ORDER BY created_at DESC, id DESC
LIMIT @lim OFFSET @off;

-- name: ScrubUserConsents :exec
-- Consent records outlive account deletion as proof, without the network details.
UPDATE consent_records SET ip_address = '', user_agent = '' WHERE user_id = @user_id;

-- name: ReportCountsByReason :many
SELECT reason, count(*) AS total
FROM reports
WHERE (sqlc.narg('place_id')::uuid IS NULL OR place_id = sqlc.narg('place_id')::uuid)
  AND created_at >= @since AND created_at < @until
GROUP BY reason;

-- name: ReportCountsByStatus :many
SELECT status, count(*) AS total
FROM reports
WHERE (sqlc.narg('place_id')::uuid IS NULL OR place_id = sqlc.narg('place_id')::uuid)
  AND created_at >= @since AND created_at < @until
GROUP BY status;

-- name: AuditActionCounts :many
SELECT action, count(*) AS total
FROM audit_log
WHERE (sqlc.narg('place_id')::uuid IS NULL OR place_id = sqlc.narg('place_id')::uuid)
  AND action = ANY(@actions::text[])
  AND created_at >= @since AND created_at < @until
GROUP BY action;
