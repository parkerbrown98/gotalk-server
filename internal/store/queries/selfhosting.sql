-- name: ListInstanceConfig :many
SELECT * FROM instance_config ORDER BY section;

-- name: UpsertInstanceConfig :one
INSERT INTO instance_config (section, settings, updated_by, updated_at)
VALUES (@section, @settings, @updated_by, now())
ON CONFLICT (section) DO UPDATE
SET settings = EXCLUDED.settings, updated_by = EXCLUDED.updated_by, updated_at = now()
RETURNING *;

-- name: DeleteInstanceConfig :execrows
DELETE FROM instance_config WHERE section = @section;

-- name: DeleteAllInstanceConfig :execrows
DELETE FROM instance_config;

-- name: BumpConfigRevision :one
UPDATE instance_settings SET config_revision = config_revision + 1, updated_at = now()
WHERE id = 1
RETURNING config_revision;

-- name: GetConfigRevision :one
SELECT config_revision FROM instance_settings WHERE id = 1;

-- name: CreateUpload :one
INSERT INTO uploads (id, storage_key, url, purpose, uploader_id, content_type, size_bytes, width, height)
VALUES (@id, @storage_key, @url, @purpose, @uploader_id, @content_type, @size_bytes, @width, @height)
RETURNING *;

-- name: GetUploadByKey :one
SELECT * FROM uploads WHERE storage_key = @storage_key;

-- name: GetUploadByURL :one
SELECT * FROM uploads WHERE url = @url ORDER BY created_at DESC LIMIT 1;

-- name: DeleteUpload :exec
DELETE FROM uploads WHERE id = @id;

-- name: ListUnreferencedUploads :many
-- Uploads older than @before whose URL no entity uses any more (replaced or cleared
-- images, deleted accounts). One anti-join scans each referencing table once.
SELECT u.* FROM uploads u
WHERE u.created_at < @before
  AND NOT EXISTS (
      SELECT 1 FROM (
          SELECT avatar_url AS url FROM users WHERE avatar_url IS NOT NULL
          UNION ALL SELECT icon_url FROM places WHERE icon_url IS NOT NULL
          UNION ALL SELECT banner_url FROM places WHERE banner_url IS NOT NULL
          UNION ALL SELECT icon_url FROM instance_settings WHERE icon_url IS NOT NULL
          UNION ALL SELECT icon_url FROM applications WHERE icon_url IS NOT NULL
      ) refs
      WHERE refs.url = u.url
  )
ORDER BY u.created_at
LIMIT @lim;

-- name: UploadReferenced :one
SELECT (EXISTS (SELECT 1 FROM users WHERE avatar_url = @url::text)
    OR EXISTS (SELECT 1 FROM places WHERE icon_url = @url::text OR banner_url = @url::text)
    OR EXISTS (SELECT 1 FROM instance_settings WHERE icon_url = @url::text)
    OR EXISTS (SELECT 1 FROM applications WHERE icon_url = @url::text))::boolean AS referenced;

-- name: CountUploads :one
SELECT count(*) AS uploads, COALESCE(sum(size_bytes), 0)::bigint AS bytes FROM uploads;
