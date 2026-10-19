-- +goose Up

-- Opt-in to the public index (gotalk.sh/explore). See docs/api-index.md.
--
-- indexable is the owner's choice. in_index is derived: the object is currently exposed
-- through /api/v1/index (directory listing on, place public, not NSFW, readable by
-- @everyone, and every flag on the way down allows it). The service maintains in_index and
-- emits change feed events whenever it flips, so in_index is never set by hand.
ALTER TABLE places
    ADD COLUMN indexable BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN in_index  BOOLEAN NOT NULL DEFAULT false;

ALTER TABLE boards
    ADD COLUMN indexable BOOLEAN NOT NULL DEFAULT true,
    ADD COLUMN in_index  BOOLEAN NOT NULL DEFAULT false;

ALTER TABLE users ADD COLUMN exclude_from_index BOOLEAN NOT NULL DEFAULT false;

-- When the topic last changed in a way the index can see (edits, replies, removals, moves).
-- GET /index/boards/{id}/topics orders and filters by it.
ALTER TABLE topics ADD COLUMN index_updated_at TIMESTAMPTZ NOT NULL DEFAULT now();
UPDATE topics SET index_updated_at = GREATEST(updated_at, last_post_at);
CREATE INDEX topics_index_listing_idx ON topics (board_id, index_updated_at, id) WHERE deleted_at IS NULL;

ALTER TABLE instance_config DROP CONSTRAINT instance_config_section_check;
ALTER TABLE instance_config ADD CONSTRAINT instance_config_section_check
    CHECK (section IN ('storage', 'mail', 'voice', 'cors', 'directory'));

-- directory_signing_key is the Ed25519 seed generated for signing index pings when no key is
-- configured. directory_previous_key is a retired public key still published after a
-- rotation, until it is revoked. index_feed_seq is the last change feed sequence number
-- handed out; index_feed_pruned_seq is the highest one removed by retention, so older
-- cursors get cursor_expired. index_last_ping_at debounces pings across replicas.
ALTER TABLE instance_settings
    ADD COLUMN directory_signing_key  BYTEA,
    ADD COLUMN directory_previous_key TEXT,
    ADD COLUMN index_feed_seq         BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN index_feed_pruned_seq  BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN index_last_ping_at     TIMESTAMPTZ;

-- The public change feed. One row per object: a new event for an object replaces the
-- previous one (coalescing) and goes to the back of the queue. seq is NULL until the row is
-- sequenced; sequencing runs under a lock on instance_settings and only sees committed
-- rows, so seq order matches commit order and readers never skip an event. ord orders
-- unsequenced rows by when they were recorded.
CREATE TABLE index_changes (
    object_type TEXT        NOT NULL CHECK (object_type IN ('place', 'board', 'topic', 'user')),
    object_id   UUID        NOT NULL,
    type        TEXT        NOT NULL CHECK (type IN (
                                'place.upsert', 'place.delete', 'board.upsert', 'board.delete',
                                'topic.upsert', 'topic.delete', 'user.exclude', 'user.include')),
    place_id    UUID,
    board_id    UUID,
    topic_id    UUID,
    user_id     UUID,
    at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    ord         TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    seq         BIGINT,
    PRIMARY KEY (object_type, object_id)
);

CREATE UNIQUE INDEX index_changes_seq_key ON index_changes (seq);
CREATE INDEX index_changes_pending_idx ON index_changes (ord) WHERE seq IS NULL;
CREATE INDEX index_changes_at_idx ON index_changes (at) WHERE seq IS NOT NULL;

-- Instance-wide audit trail (directory listing, signing keys, a person's index exclusion).
-- Place-level changes stay in audit_log.
CREATE TABLE instance_audit_log (
    id          UUID        PRIMARY KEY,
    actor_id    UUID        REFERENCES users (id) ON DELETE SET NULL,
    action      TEXT        NOT NULL,
    target_type TEXT        NOT NULL DEFAULT '',
    target_id   UUID,
    metadata    JSONB       NOT NULL DEFAULT '{}',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX instance_audit_log_id_idx ON instance_audit_log (id DESC);

-- +goose Down
DROP TABLE instance_audit_log;
DROP TABLE index_changes;
ALTER TABLE instance_settings
    DROP COLUMN index_last_ping_at,
    DROP COLUMN index_feed_pruned_seq,
    DROP COLUMN index_feed_seq,
    DROP COLUMN directory_previous_key,
    DROP COLUMN directory_signing_key;
DELETE FROM instance_config WHERE section = 'directory';
ALTER TABLE instance_config DROP CONSTRAINT instance_config_section_check;
ALTER TABLE instance_config ADD CONSTRAINT instance_config_section_check
    CHECK (section IN ('storage', 'mail', 'voice', 'cors'));
DROP INDEX topics_index_listing_idx;
ALTER TABLE topics DROP COLUMN index_updated_at;
ALTER TABLE users DROP COLUMN exclude_from_index;
ALTER TABLE boards DROP COLUMN in_index, DROP COLUMN indexable;
ALTER TABLE places DROP COLUMN in_index, DROP COLUMN indexable;
