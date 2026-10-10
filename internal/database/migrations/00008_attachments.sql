-- +goose Up

-- Uploads now also hold files attached to messages and posts, and images cached for link
-- previews. filename is the name shown to people (attachments only).
ALTER TABLE uploads DROP CONSTRAINT uploads_purpose_check;
ALTER TABLE uploads ADD CONSTRAINT uploads_purpose_check
    CHECK (purpose IN ('avatar', 'place_icon', 'place_banner', 'instance_icon', 'attachment', 'embed'));
ALTER TABLE uploads ADD COLUMN filename TEXT NOT NULL DEFAULT '';

-- Files attached to a chat message, in display order. An upload belongs to one message or
-- post; deleting the message frees it for the cleanup sweep.
CREATE TABLE message_attachments (
    message_id UUID     NOT NULL REFERENCES messages (id) ON DELETE CASCADE,
    upload_id  UUID     NOT NULL UNIQUE REFERENCES uploads (id) ON DELETE CASCADE,
    position   SMALLINT NOT NULL,
    PRIMARY KEY (message_id, position)
);

CREATE TABLE post_attachments (
    post_id   UUID     NOT NULL REFERENCES posts (id) ON DELETE CASCADE,
    upload_id UUID     NOT NULL UNIQUE REFERENCES uploads (id) ON DELETE CASCADE,
    position  SMALLINT NOT NULL,
    PRIMARY KEY (post_id, position)
);

-- Metadata fetched for links in messages and posts (OpenGraph, Twitter cards, <title>).
-- Rows are keyed by the URL as written; failed fetches are remembered so they are not
-- retried on every view. The preview image is copied into storage (image_upload_id) so
-- clients never contact the linked site.
CREATE TABLE link_previews (
    url             TEXT        PRIMARY KEY,
    status          TEXT        NOT NULL CHECK (status IN ('ok', 'failed')),
    kind            TEXT        NOT NULL DEFAULT 'link' CHECK (kind IN ('link', 'image')),
    final_url       TEXT        NOT NULL DEFAULT '',
    site_name       TEXT        NOT NULL DEFAULT '',
    title           TEXT        NOT NULL DEFAULT '',
    description     TEXT        NOT NULL DEFAULT '',
    theme_color     TEXT        NOT NULL DEFAULT '',
    large_image     BOOLEAN     NOT NULL DEFAULT false,
    image_upload_id UUID        REFERENCES uploads (id) ON DELETE SET NULL,
    fetched_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE link_previews;
DROP TABLE post_attachments;
DROP TABLE message_attachments;
DELETE FROM uploads WHERE purpose IN ('attachment', 'embed');
ALTER TABLE uploads DROP COLUMN filename;
ALTER TABLE uploads DROP CONSTRAINT uploads_purpose_check;
ALTER TABLE uploads ADD CONSTRAINT uploads_purpose_check
    CHECK (purpose IN ('avatar', 'place_icon', 'place_banner', 'instance_icon'));
