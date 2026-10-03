-- +goose Up

-- Bot accounts are users that act for an application. They cannot log in with a password.
ALTER TABLE users ADD COLUMN is_bot BOOLEAN NOT NULL DEFAULT false;

-- Third-party integrations. Every application has a bot account that acts on its behalf.
CREATE TABLE applications (
    id          UUID        PRIMARY KEY,
    owner_id    UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    bot_user_id UUID        NOT NULL UNIQUE REFERENCES users (id),
    name        TEXT        NOT NULL,
    description TEXT        NOT NULL DEFAULT '',
    icon_url    TEXT,
    -- Anyone with MANAGE_PLACE can add a public application's bot to their place; private
    -- applications can only be added by their owner.
    is_public   BOOLEAN     NOT NULL DEFAULT false,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX applications_owner_id_idx ON applications (owner_id);

-- Slash commands an application's bot answers. Invocations are relayed to the bot over the
-- gateway and are not stored.
CREATE TABLE application_commands (
    id             UUID        PRIMARY KEY,
    application_id UUID        NOT NULL REFERENCES applications (id) ON DELETE CASCADE,
    name           TEXT        NOT NULL,
    description    TEXT        NOT NULL,
    options        JSONB       NOT NULL DEFAULT '[]',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (application_id, name)
);

-- Long-lived credentials for the API and gateway: personal access tokens and bot tokens.
-- Only a SHA-256 hash of each token is stored.
CREATE TABLE api_tokens (
    id             UUID        PRIMARY KEY,
    user_id        UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    application_id UUID        REFERENCES applications (id) ON DELETE CASCADE,
    kind           TEXT        NOT NULL CHECK (kind IN ('personal', 'bot')),
    name           TEXT        NOT NULL,
    token_hash     BYTEA       NOT NULL,
    -- The token's first characters, so people can recognize it in listings.
    token_hint     TEXT        NOT NULL,
    scopes         TEXT[]      NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at   TIMESTAMPTZ,
    expires_at     TIMESTAMPTZ,
    revoked_at     TIMESTAMPTZ,
    CHECK ((kind = 'bot') = (application_id IS NOT NULL))
);

CREATE UNIQUE INDEX api_tokens_hash_key ON api_tokens (token_hash);
CREATE INDEX api_tokens_user_id_idx ON api_tokens (user_id) WHERE revoked_at IS NULL;
CREATE INDEX api_tokens_application_id_idx ON api_tokens (application_id) WHERE application_id IS NOT NULL;

-- Outgoing webhooks: place events POSTed to an external URL, signed with the secret.
CREATE TABLE webhooks (
    id                   UUID        PRIMARY KEY,
    place_id             UUID        NOT NULL REFERENCES places (id) ON DELETE CASCADE,
    name                 TEXT        NOT NULL,
    url                  TEXT        NOT NULL,
    secret               TEXT        NOT NULL,
    events               TEXT[]      NOT NULL,
    is_active            BOOLEAN     NOT NULL DEFAULT true,
    created_by           UUID        REFERENCES users (id) ON DELETE SET NULL,
    -- Deliveries that failed every attempt in a row; reaching the limit disables the webhook.
    consecutive_failures INTEGER     NOT NULL DEFAULT 0,
    disabled_reason      TEXT        NOT NULL DEFAULT '',
    last_delivery_at     TIMESTAMPTZ,
    last_success_at      TIMESTAMPTZ,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX webhooks_place_id_idx ON webhooks (place_id);

-- The delivery queue (an outbox written in the same transaction as the event) and its log.
CREATE TABLE webhook_deliveries (
    id              UUID        PRIMARY KEY,
    webhook_id      UUID        NOT NULL REFERENCES webhooks (id) ON DELETE CASCADE,
    event           TEXT        NOT NULL,
    payload         JSONB       NOT NULL,
    status          TEXT        NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'succeeded', 'failed')),
    attempts        INTEGER     NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Set while a replica is delivering, so others skip it; expires if that replica dies.
    locked_until    TIMESTAMPTZ,
    response_status INTEGER,
    last_error      TEXT        NOT NULL DEFAULT '',
    duration_ms     INTEGER,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at    TIMESTAMPTZ
);

CREATE INDEX webhook_deliveries_due_idx ON webhook_deliveries (next_attempt_at) WHERE status = 'pending';
CREATE INDEX webhook_deliveries_webhook_idx ON webhook_deliveries (webhook_id, id DESC);
CREATE INDEX webhook_deliveries_completed_idx ON webhook_deliveries (completed_at) WHERE completed_at IS NOT NULL;

-- Versioned instance policies. Publishing a change adds a version; old versions stay
-- readable as the policy's changelog.
CREATE TABLE policy_documents (
    id               UUID        PRIMARY KEY,
    kind             TEXT        NOT NULL CHECK (kind IN ('terms', 'privacy', 'guidelines')),
    version          INTEGER     NOT NULL,
    title            TEXT        NOT NULL,
    content          TEXT        NOT NULL,
    -- What changed since the previous version.
    summary          TEXT        NOT NULL DEFAULT '',
    requires_consent BOOLEAN     NOT NULL DEFAULT false,
    effective_at     TIMESTAMPTZ NOT NULL,
    published_by     UUID        REFERENCES users (id) ON DELETE SET NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (kind, version)
);

-- Append-only log of consent given or withdrawn, per purpose (a policy kind or another
-- processing purpose such as analytics).
CREATE TABLE consent_records (
    id             UUID        PRIMARY KEY,
    user_id        UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    purpose        TEXT        NOT NULL,
    policy_version INTEGER,
    granted        BOOLEAN     NOT NULL,
    ip_address     TEXT        NOT NULL DEFAULT '',
    user_agent     TEXT        NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX consent_records_user_idx ON consent_records (user_id, purpose, created_at DESC);

-- Transparency reports aggregate the audit log by action and time.
CREATE INDEX audit_log_action_time_idx ON audit_log (action, created_at);
CREATE INDEX reports_created_at_idx ON reports (created_at);

-- +goose Down
DROP INDEX reports_created_at_idx;
DROP INDEX audit_log_action_time_idx;
DROP TABLE consent_records;
DROP TABLE policy_documents;
DROP TABLE webhook_deliveries;
DROP TABLE webhooks;
DROP TABLE api_tokens;
DROP TABLE application_commands;
DROP TABLE applications;
ALTER TABLE users DROP COLUMN is_bot;
