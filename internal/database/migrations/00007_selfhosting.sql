-- +goose Up

-- Provider settings saved through the setup wizard or the instance settings API. A section
-- whose enabling key (storage.driver, mail.driver, voice.livekit_url,
-- server.cors_allowed_origins) is set in the config file or environment ignores its row.
-- Secrets are stored as entered, so protect database backups accordingly.
CREATE TABLE instance_config (
    section    TEXT        PRIMARY KEY CHECK (section IN ('storage', 'mail', 'voice', 'cors')),
    settings   JSONB       NOT NULL,
    updated_by UUID        REFERENCES users (id) ON DELETE SET NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Bumped on every instance_config change so every replica reloads its providers.
ALTER TABLE instance_settings ADD COLUMN config_revision BIGINT NOT NULL DEFAULT 0;

-- Files uploaded to the storage backend. url is the exact value stored on the entity that
-- uses the file (users.avatar_url, places.icon_url, ...); uploads no longer referenced are
-- deleted by the maintenance loop.
CREATE TABLE uploads (
    id           UUID        PRIMARY KEY,
    storage_key  TEXT        NOT NULL UNIQUE,
    url          TEXT        NOT NULL,
    purpose      TEXT        NOT NULL CHECK (purpose IN ('avatar', 'place_icon', 'place_banner', 'instance_icon')),
    uploader_id  UUID        REFERENCES users (id) ON DELETE SET NULL,
    content_type TEXT        NOT NULL,
    size_bytes   BIGINT      NOT NULL,
    width        INTEGER     NOT NULL,
    height       INTEGER     NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX uploads_url_idx ON uploads (url);
CREATE INDEX uploads_created_at_idx ON uploads (created_at);

-- Single-use tokens sent by email. Only a SHA-256 hash is stored. email is the address the
-- token was sent to; a verification token stops working if the account's email changes.
CREATE TABLE email_tokens (
    token_hash BYTEA       PRIMARY KEY,
    user_id    UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    purpose    TEXT        NOT NULL CHECK (purpose IN ('password_reset', 'email_verification')),
    email      TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL,
    used_at    TIMESTAMPTZ
);

CREATE INDEX email_tokens_user_idx ON email_tokens (user_id, purpose, created_at DESC);
CREATE INDEX email_tokens_expires_idx ON email_tokens (expires_at);

-- Outgoing email queue, written in the same transaction as the action that sends it and
-- drained by every replica. Bodies are cleared once a message is sent or given up on,
-- since they can contain single-use links.
CREATE TABLE mail_outbox (
    id              UUID        PRIMARY KEY,
    kind            TEXT        NOT NULL,
    to_address      TEXT        NOT NULL,
    subject         TEXT        NOT NULL,
    text_body       TEXT        NOT NULL,
    html_body       TEXT        NOT NULL DEFAULT '',
    status          TEXT        NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'sent', 'failed')),
    attempts        INTEGER     NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Messages still unsent at this time are dropped (their links have expired).
    expires_at      TIMESTAMPTZ NOT NULL,
    locked_until    TIMESTAMPTZ,
    last_error      TEXT        NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at    TIMESTAMPTZ
);

CREATE INDEX mail_outbox_due_idx ON mail_outbox (next_attempt_at) WHERE status = 'pending';
CREATE INDEX mail_outbox_completed_idx ON mail_outbox (completed_at) WHERE completed_at IS NOT NULL;

-- +goose Down
DROP TABLE mail_outbox;
DROP TABLE email_tokens;
DROP TABLE uploads;
ALTER TABLE instance_settings DROP COLUMN config_revision;
DROP TABLE instance_config;
