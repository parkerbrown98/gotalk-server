-- +goose Up

-- Chat channels. Place channels are categories (top-level groups) and text channels; threads
-- are text-like channels nested under a text channel and inherit its permissions. Direct
-- messages (dm, group_dm) have no place; access comes from channel_recipients.
CREATE TABLE channels (
    id                UUID        PRIMARY KEY,
    place_id          UUID        REFERENCES places (id) ON DELETE CASCADE,
    parent_id         UUID        REFERENCES channels (id) ON DELETE CASCADE,
    kind              TEXT        NOT NULL CHECK (kind IN ('category', 'text', 'thread', 'dm', 'group_dm')),
    name              TEXT        NOT NULL DEFAULT '',
    topic             TEXT        NOT NULL DEFAULT '',
    position          INTEGER     NOT NULL DEFAULT 0,
    is_nsfw           BOOLEAN     NOT NULL DEFAULT false,
    -- Group DM owner or thread creator.
    owner_id          UUID        REFERENCES users (id) ON DELETE SET NULL,
    -- The message a thread was started from, if any.
    thread_message_id UUID,
    is_archived       BOOLEAN     NOT NULL DEFAULT false,
    -- "<lower user id>:<higher user id>" for one-to-one DMs, so each pair has one channel.
    dm_key            TEXT,
    last_message_id   UUID,
    last_message_at   TIMESTAMPTZ,
    message_count     INTEGER     NOT NULL DEFAULT 0,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((place_id IS NULL) = (kind IN ('dm', 'group_dm')))
);

CREATE INDEX channels_place_idx ON channels (place_id, position) WHERE kind IN ('category', 'text');
CREATE INDEX channels_parent_idx ON channels (parent_id);
CREATE INDEX channels_threads_idx ON channels (parent_id, last_message_at DESC NULLS LAST) WHERE kind = 'thread';
CREATE UNIQUE INDEX channels_dm_key ON channels (dm_key) WHERE dm_key IS NOT NULL;
CREATE UNIQUE INDEX channels_thread_message_key ON channels (thread_message_id) WHERE thread_message_id IS NOT NULL;

-- Per-channel permission overwrites for a role, applied from the category down to the
-- channel (threads use their parent's).
CREATE TABLE channel_overwrites (
    channel_id UUID   NOT NULL REFERENCES channels (id) ON DELETE CASCADE,
    role_id    UUID   NOT NULL REFERENCES roles (id) ON DELETE CASCADE,
    place_id   UUID   NOT NULL REFERENCES places (id) ON DELETE CASCADE,
    allow      BIGINT NOT NULL DEFAULT 0,
    deny       BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (channel_id, role_id)
);

CREATE INDEX channel_overwrites_place_id_idx ON channel_overwrites (place_id);
CREATE INDEX channel_overwrites_role_id_idx ON channel_overwrites (role_id);

CREATE TABLE channel_recipients (
    channel_id UUID        NOT NULL REFERENCES channels (id) ON DELETE CASCADE,
    user_id    UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    joined_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (channel_id, user_id)
);

CREATE INDEX channel_recipients_user_id_idx ON channel_recipients (user_id);

-- Message IDs are UUIDv7, so ordering by ID is chronological and IDs double as cursors.
-- Deleting a message removes it outright; reports keep a snapshot of the content.
CREATE TABLE messages (
    id          UUID        PRIMARY KEY,
    channel_id  UUID        NOT NULL REFERENCES channels (id) ON DELETE CASCADE,
    place_id    UUID        REFERENCES places (id) ON DELETE CASCADE,
    author_id   UUID        REFERENCES users (id) ON DELETE SET NULL,
    content     TEXT        NOT NULL,
    reply_to_id UUID        REFERENCES messages (id) ON DELETE SET NULL,
    -- Users who were notified by this message (mentions and the replied-to author).
    mention_ids UUID[]      NOT NULL DEFAULT '{}',
    is_pinned   BOOLEAN     NOT NULL DEFAULT false,
    pinned_at   TIMESTAMPTZ,
    pinned_by   UUID        REFERENCES users (id) ON DELETE SET NULL,
    edit_count  INTEGER     NOT NULL DEFAULT 0,
    edited_at   TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX messages_channel_idx ON messages (channel_id, id);
CREATE INDEX messages_author_id_idx ON messages (author_id);
CREATE INDEX messages_pinned_idx ON messages (channel_id, pinned_at DESC) WHERE is_pinned;
CREATE INDEX messages_reply_to_idx ON messages (reply_to_id) WHERE reply_to_id IS NOT NULL;

ALTER TABLE channels ADD CONSTRAINT channels_thread_message_id_fkey
    FOREIGN KEY (thread_message_id) REFERENCES messages (id) ON DELETE SET NULL;

CREATE TABLE message_revisions (
    id         UUID        PRIMARY KEY,
    message_id UUID        NOT NULL REFERENCES messages (id) ON DELETE CASCADE,
    -- Content as it was before this edit.
    content    TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX message_revisions_message_id_idx ON message_revisions (message_id, created_at);

CREATE TABLE message_reactions (
    message_id UUID        NOT NULL REFERENCES messages (id) ON DELETE CASCADE,
    user_id    UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    emoji      TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (message_id, emoji, user_id)
);

CREATE INDEX message_reactions_user_id_idx ON message_reactions (user_id);

-- Per-user read position in a channel. mention_count counts unread messages that notified
-- the user (every unread message in a DM).
CREATE TABLE channel_reads (
    user_id              UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    channel_id           UUID        NOT NULL REFERENCES channels (id) ON DELETE CASCADE,
    last_read_message_id UUID,
    mention_count        INTEGER     NOT NULL DEFAULT 0,
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, channel_id)
);

CREATE INDEX channel_reads_channel_id_idx ON channel_reads (channel_id);

-- Chat notifications.
ALTER TABLE notifications
    ADD COLUMN channel_id UUID REFERENCES channels (id) ON DELETE CASCADE,
    ADD COLUMN message_id UUID REFERENCES messages (id) ON DELETE CASCADE;
CREATE INDEX notifications_channel_idx ON notifications (user_id, channel_id) WHERE channel_id IS NOT NULL;
-- One unread direct-message notification per conversation; the read state carries the count.
CREATE UNIQUE INDEX notifications_dm_dedupe ON notifications (user_id, channel_id)
    WHERE kind = 'direct_message' AND read_at IS NULL;

-- Channel mutes.
ALTER TABLE subscriptions DROP CONSTRAINT subscriptions_target_type_check;
ALTER TABLE subscriptions ADD CONSTRAINT subscriptions_target_type_check
    CHECK (target_type IN ('place', 'board', 'topic', 'channel'));
-- Direct-message mutes have no place.
ALTER TABLE subscriptions ALTER COLUMN place_id DROP NOT NULL;

-- Message reports.
ALTER TABLE reports
    ADD COLUMN message_id UUID REFERENCES messages (id) ON DELETE SET NULL,
    ADD COLUMN channel_id UUID REFERENCES channels (id) ON DELETE SET NULL;
ALTER TABLE reports DROP CONSTRAINT reports_target_type_check;
ALTER TABLE reports ADD CONSTRAINT reports_target_type_check
    CHECK (target_type IN ('post', 'user', 'message'));
-- Reports dedupe on their target. Message reports keep their author in target_user_id but
-- must not fall back to it once the message is deleted (message_id becomes NULL), or two
-- reports about the same author would collide.
DROP INDEX reports_open_dedupe;
CREATE UNIQUE INDEX reports_open_dedupe ON reports (reporter_id, target_type,
    COALESCE(post_id, message_id, CASE WHEN target_type = 'user' THEN target_user_id END))
    WHERE status = 'open';

-- +goose Down
DROP INDEX reports_open_dedupe;
DELETE FROM reports WHERE target_type = 'message';
CREATE UNIQUE INDEX reports_open_dedupe ON reports (reporter_id, target_type, COALESCE(post_id, target_user_id))
    WHERE status = 'open';
ALTER TABLE reports DROP CONSTRAINT reports_target_type_check;
ALTER TABLE reports ADD CONSTRAINT reports_target_type_check CHECK (target_type IN ('post', 'user'));
ALTER TABLE reports DROP COLUMN channel_id, DROP COLUMN message_id;
DELETE FROM subscriptions WHERE target_type = 'channel';
ALTER TABLE subscriptions ALTER COLUMN place_id SET NOT NULL;
ALTER TABLE subscriptions DROP CONSTRAINT subscriptions_target_type_check;
ALTER TABLE subscriptions ADD CONSTRAINT subscriptions_target_type_check
    CHECK (target_type IN ('place', 'board', 'topic'));
DELETE FROM notifications WHERE channel_id IS NOT NULL OR kind = 'direct_message';
DROP INDEX notifications_dm_dedupe;
DROP INDEX notifications_channel_idx;
ALTER TABLE notifications DROP COLUMN message_id, DROP COLUMN channel_id;
DROP TABLE channel_reads;
DROP TABLE message_reactions;
DROP TABLE message_revisions;
ALTER TABLE channels DROP CONSTRAINT channels_thread_message_id_fkey;
DROP TABLE messages;
DROP TABLE channel_recipients;
DROP TABLE channel_overwrites;
DROP TABLE channels;
