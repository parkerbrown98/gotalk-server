-- name: EnsureInstanceSettings :exec
INSERT INTO instance_settings (id, jwt_secret, setup_token)
VALUES (1, @jwt_secret, @setup_token)
ON CONFLICT (id) DO NOTHING;

-- name: GetInstanceSettings :one
SELECT * FROM instance_settings WHERE id = 1;

-- name: CompleteSetup :one
UPDATE instance_settings
SET name               = @name,
    description        = @description,
    registration_mode  = @registration_mode,
    setup_completed_at = now(),
    setup_token        = NULL,
    updated_at         = now()
WHERE id = 1 AND setup_completed_at IS NULL
RETURNING *;

-- name: UpdateInstanceSettings :one
-- Nullable params leave the column unchanged; an empty icon_url clears it.
UPDATE instance_settings
SET name              = COALESCE(sqlc.narg('name')::text, name),
    description       = COALESCE(sqlc.narg('description')::text, description),
    icon_url          = CASE WHEN sqlc.narg('icon_url')::text IS NULL THEN icon_url
                             ELSE NULLIF(sqlc.narg('icon_url')::text, '') END,
    registration_mode = COALESCE(sqlc.narg('registration_mode')::text, registration_mode),
    updated_at        = now()
WHERE id = 1
RETURNING *;

-- name: CountUsers :one
SELECT count(*) FROM users WHERE deleted_at IS NULL AND NOT is_bot;

-- name: CountPlaces :one
SELECT count(*) FROM places WHERE deleted_at IS NULL;
