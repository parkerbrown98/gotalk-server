-- name: CreateBotUser :one
INSERT INTO users (id, username, email, password_hash, display_name, is_bot)
VALUES (@id, @username, @email, '', @display_name, true)
RETURNING *;

-- name: CreateApplication :one
INSERT INTO applications (id, owner_id, bot_user_id, name, description, icon_url, is_public)
VALUES (@id, @owner_id, @bot_user_id, @name, @description, @icon_url, @is_public)
RETURNING *;

-- name: GetApplication :one
SELECT * FROM applications WHERE id = @id;

-- name: GetApplicationByBot :one
SELECT * FROM applications WHERE bot_user_id = @bot_user_id;

-- name: ListOwnedApplications :many
SELECT * FROM applications WHERE owner_id = @owner_id ORDER BY created_at;

-- name: CountOwnedApplications :one
SELECT count(*) FROM applications WHERE owner_id = @owner_id;

-- name: UpdateApplication :one
-- Nullable params leave the column unchanged; an empty icon_url clears it.
UPDATE applications
SET name        = COALESCE(sqlc.narg('name')::text, name),
    description = COALESCE(sqlc.narg('description')::text, description),
    icon_url    = CASE WHEN sqlc.narg('icon_url')::text IS NULL THEN icon_url
                       ELSE NULLIF(sqlc.narg('icon_url')::text, '') END,
    is_public   = COALESCE(sqlc.narg('is_public')::boolean, is_public),
    updated_at  = now()
WHERE id = @id
RETURNING *;

-- name: DeleteApplication :exec
DELETE FROM applications WHERE id = @id;

-- name: ListPlaceApplications :many
-- Applications whose bot is a member of the place.
SELECT a.*
FROM applications a
JOIN place_members m ON m.user_id = a.bot_user_id
WHERE m.place_id = @place_id
ORDER BY a.name, a.id;

-- name: ListApplicationCommands :many
SELECT * FROM application_commands WHERE application_id = @application_id ORDER BY name;

-- name: ListCommandsForApplications :many
SELECT * FROM application_commands WHERE application_id = ANY(@application_ids::uuid[]) ORDER BY name, application_id;

-- name: GetApplicationCommand :one
SELECT * FROM application_commands WHERE application_id = @application_id AND name = @name;

-- name: DeleteApplicationCommands :exec
DELETE FROM application_commands WHERE application_id = @application_id;

-- name: CreateApplicationCommand :one
INSERT INTO application_commands (id, application_id, name, description, options)
VALUES (@id, @application_id, @name, @description, @options)
RETURNING *;
