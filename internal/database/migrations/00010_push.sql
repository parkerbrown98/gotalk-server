-- +goose Up

-- Push notifications through a Gotalk push gateway. See docs/push.md.

ALTER TABLE instance_config DROP CONSTRAINT instance_config_section_check;
ALTER TABLE instance_config ADD CONSTRAINT instance_config_section_check
    CHECK (section IN ('storage', 'mail', 'voice', 'cors', 'directory', 'push'));

-- The instance's registration with its push gateway (one row). signing_key is the Ed25519
-- seed generated when push.signing_key is not configured. gateway_url and public_key record
-- which gateway and key instance_id and key_id belong to, so the instance registers again
-- when either changes. checked_at is when a replica last synced the registration, which
-- spreads the periodic status check across replicas.
CREATE TABLE push_gateway (
    id          SMALLINT    PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    signing_key BYTEA,
    gateway_url TEXT,
    public_key  TEXT,
    instance_id TEXT,
    key_id      TEXT,
    status      TEXT        NOT NULL DEFAULT 'unregistered'
                            CHECK (status IN ('unregistered', 'pending', 'active', 'suspended', 'blocked', 'error')),
    detail      TEXT        NOT NULL DEFAULT '',
    verified_at TIMESTAMPTZ,
    checked_at  TIMESTAMPTZ,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
INSERT INTO push_gateway (id) VALUES (1);

-- A device's gateway subscription for one account. The server never sees the platform token;
-- it keeps the gateway's subscription ID and the device's public key material (p256dh and
-- auth) to encrypt payloads. Each subscription belongs to the login session that added it,
-- so signing out (or being signed out) stops pushes to that device. Several accounts on this
-- instance may share one gateway subscription ID, each with its own keys.
CREATE TABLE push_subscriptions (
    id              UUID        PRIMARY KEY,
    user_id         UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    session_id      UUID        NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    gateway_url     TEXT        NOT NULL,
    subscription_id TEXT        NOT NULL,
    app_id          TEXT        NOT NULL,
    platform        TEXT        NOT NULL CHECK (platform IN ('ios', 'android', 'web')),
    p256dh          BYTEA       NOT NULL,
    auth            BYTEA       NOT NULL,
    mode            TEXT        NOT NULL DEFAULT 'encrypted' CHECK (mode IN ('encrypted', 'id_only')),
    device_name     TEXT        NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_push_at    TIMESTAMPTZ,
    UNIQUE (user_id, gateway_url, subscription_id)
);

CREATE INDEX push_subscriptions_gateway_idx ON push_subscriptions (gateway_url, subscription_id);
CREATE INDEX push_subscriptions_session_idx ON push_subscriptions (session_id);

-- Per-person push settings. Mutes on places, boards, topics, channels and conversations
-- already decide which notifications exist; these decide which of them also become pushes.
-- Quiet hours are minutes after midnight in the person's time zone; start > end wraps
-- past midnight.
CREATE TABLE push_preferences (
    user_id             UUID        PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    disabled_categories TEXT[]      NOT NULL DEFAULT '{}',
    quiet_hours_start   SMALLINT    CHECK (quiet_hours_start BETWEEN 0 AND 1439),
    quiet_hours_end     SMALLINT    CHECK (quiet_hours_end BETWEEN 0 AND 1439),
    timezone            TEXT        NOT NULL DEFAULT 'UTC',
    paused_until        TIMESTAMPTZ,
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((quiet_hours_start IS NULL) = (quiet_hours_end IS NULL))
);

-- Notifications waiting to be pushed, written in the same transaction as the notification
-- and drained by every replica. pending lists the subscriptions still to push to after a
-- partial failure (NULL means all of the person's). Rows are deleted once handled.
CREATE TABLE push_outbox (
    notification_id UUID        PRIMARY KEY REFERENCES notifications (id) ON DELETE CASCADE,
    user_id         UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    pending         UUID[],
    attempts        INTEGER     NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    locked_until    TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX push_outbox_due_idx ON push_outbox (next_attempt_at);

-- +goose Down
DROP TABLE push_outbox;
DROP TABLE push_preferences;
DROP TABLE push_subscriptions;
DROP TABLE push_gateway;
DELETE FROM instance_config WHERE section = 'push';
ALTER TABLE instance_config DROP CONSTRAINT instance_config_section_check;
ALTER TABLE instance_config ADD CONSTRAINT instance_config_section_check
    CHECK (section IN ('storage', 'mail', 'voice', 'cors', 'directory'));
