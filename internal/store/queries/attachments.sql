-- name: ListUploadsByIDs :many
SELECT * FROM uploads WHERE id = ANY(@ids::uuid[]);

-- name: ListAttachedUploads :many
-- The given uploads that already belong to a message or post.
SELECT upload_id FROM message_attachments WHERE upload_id = ANY(@ids::uuid[])
UNION ALL
SELECT upload_id FROM post_attachments WHERE upload_id = ANY(@ids::uuid[]);

-- name: AddMessageAttachments :exec
INSERT INTO message_attachments (message_id, upload_id, position)
SELECT @message_id::uuid, a.id, (a.ord - 1)::smallint
FROM unnest(@upload_ids::uuid[]) WITH ORDINALITY AS a(id, ord);

-- name: ListMessageAttachments :many
SELECT ma.message_id, sqlc.embed(u)
FROM message_attachments ma
JOIN uploads u ON u.id = ma.upload_id
WHERE ma.message_id = ANY(@message_ids::uuid[])
ORDER BY ma.message_id, ma.position;

-- name: AddPostAttachments :exec
INSERT INTO post_attachments (post_id, upload_id, position)
SELECT @post_id::uuid, a.id, (a.ord - 1)::smallint
FROM unnest(@upload_ids::uuid[]) WITH ORDINALITY AS a(id, ord);

-- name: ListPostAttachments :many
SELECT pa.post_id, sqlc.embed(u)
FROM post_attachments pa
JOIN uploads u ON u.id = pa.upload_id
WHERE pa.post_id = ANY(@post_ids::uuid[])
ORDER BY pa.post_id, pa.position;

-- name: ListLinkPreviews :many
SELECT lp.url, lp.status, lp.kind, lp.final_url, lp.site_name, lp.title, lp.description, lp.theme_color,
       lp.large_image, lp.fetched_at, u.url AS image_url, u.width AS image_width, u.height AS image_height
FROM link_previews lp
LEFT JOIN uploads u ON u.id = lp.image_upload_id
WHERE lp.url = ANY(@urls::text[]);

-- name: UpsertLinkPreview :exec
INSERT INTO link_previews (url, status, kind, final_url, site_name, title, description, theme_color, large_image,
                           image_upload_id, fetched_at)
VALUES (@url, @status, @kind, @final_url, @site_name, @title, @description, @theme_color, @large_image,
        @image_upload_id, now())
ON CONFLICT (url) DO UPDATE
SET status          = EXCLUDED.status,
    kind            = EXCLUDED.kind,
    final_url       = EXCLUDED.final_url,
    site_name       = EXCLUDED.site_name,
    title           = EXCLUDED.title,
    description     = EXCLUDED.description,
    theme_color     = EXCLUDED.theme_color,
    large_image     = EXCLUDED.large_image,
    image_upload_id = EXCLUDED.image_upload_id,
    fetched_at      = now();
