-- +goose Up

-- Moderation additions to phase 1 tables.
ALTER TABLE place_members ADD COLUMN timeout_until TIMESTAMPTZ;
ALTER TABLE place_bans ADD COLUMN expires_at TIMESTAMPTZ;

-- Boards form a tree within a place: root categories group boards, and boards may nest
-- (depth is capped by the service). Categories only contain boards; boards hold topics.
CREATE TABLE boards (
    id                UUID        PRIMARY KEY,
    place_id          UUID        NOT NULL REFERENCES places (id) ON DELETE CASCADE,
    parent_id         UUID        REFERENCES boards (id) ON DELETE RESTRICT,
    kind              TEXT        NOT NULL DEFAULT 'board' CHECK (kind IN ('category', 'board')),
    slug              TEXT        NOT NULL,
    name              TEXT        NOT NULL,
    description       TEXT        NOT NULL DEFAULT '',
    position          INTEGER     NOT NULL DEFAULT 0,
    reply_mode        TEXT        NOT NULL DEFAULT 'flat' CHECK (reply_mode IN ('flat', 'threaded')),
    solutions_enabled BOOLEAN     NOT NULL DEFAULT false,
    is_nsfw           BOOLEAN     NOT NULL DEFAULT false,
    -- Derived: readable by signed-out visitors. Maintained by the service and used to scope
    -- instance-wide search without evaluating permissions per row.
    is_public         BOOLEAN     NOT NULL DEFAULT false,
    topic_count       INTEGER     NOT NULL DEFAULT 0,
    post_count        INTEGER     NOT NULL DEFAULT 0,
    last_post_at      TIMESTAMPTZ,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX boards_slug_key ON boards (place_id, lower(slug));
CREATE INDEX boards_place_id_idx ON boards (place_id, position);
CREATE INDEX boards_parent_id_idx ON boards (parent_id);

-- Per-board permission overwrites for a role (including @everyone). Applied from the root
-- board down to the target board.
CREATE TABLE board_overwrites (
    board_id UUID   NOT NULL REFERENCES boards (id) ON DELETE CASCADE,
    role_id  UUID   NOT NULL REFERENCES roles (id) ON DELETE CASCADE,
    place_id UUID   NOT NULL REFERENCES places (id) ON DELETE CASCADE,
    allow    BIGINT NOT NULL DEFAULT 0,
    deny     BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (board_id, role_id)
);

CREATE INDEX board_overwrites_place_id_idx ON board_overwrites (place_id);
CREATE INDEX board_overwrites_role_id_idx ON board_overwrites (role_id);

CREATE TABLE topics (
    id               UUID        PRIMARY KEY,
    place_id         UUID        NOT NULL REFERENCES places (id) ON DELETE CASCADE,
    board_id         UUID        NOT NULL REFERENCES boards (id) ON DELETE CASCADE,
    author_id        UUID        REFERENCES users (id) ON DELETE SET NULL,
    title            TEXT        NOT NULL,
    slug             TEXT        NOT NULL,
    tags             TEXT[]      NOT NULL DEFAULT '{}',
    is_pinned        BOOLEAN     NOT NULL DEFAULT false,
    is_locked        BOOLEAN     NOT NULL DEFAULT false,
    is_archived      BOOLEAN     NOT NULL DEFAULT false,
    solution_post_id UUID,
    -- post_count counts live posts; last_post_number only grows so post numbers are stable.
    post_count       INTEGER     NOT NULL DEFAULT 0,
    last_post_number INTEGER     NOT NULL DEFAULT 0,
    last_post_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_poster_id   UUID        REFERENCES users (id) ON DELETE SET NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at       TIMESTAMPTZ,
    deleted_by       UUID        REFERENCES users (id) ON DELETE SET NULL
);

CREATE INDEX topics_board_listing_idx ON topics (board_id, is_pinned DESC, last_post_at DESC)
    WHERE deleted_at IS NULL;
CREATE INDEX topics_place_latest_idx ON topics (place_id, last_post_at DESC) WHERE deleted_at IS NULL;
CREATE INDEX topics_tags_idx ON topics USING gin (tags) WHERE deleted_at IS NULL;
CREATE INDEX topics_author_id_idx ON topics (author_id);

CREATE TABLE posts (
    id             UUID        PRIMARY KEY,
    topic_id       UUID        NOT NULL REFERENCES topics (id) ON DELETE CASCADE,
    place_id       UUID        NOT NULL REFERENCES places (id) ON DELETE CASCADE,
    -- Denormalized from the topic (kept in sync on moves) so search can filter cheaply.
    board_id       UUID        NOT NULL REFERENCES boards (id) ON DELETE CASCADE,
    author_id      UUID        REFERENCES users (id) ON DELETE SET NULL,
    parent_id      UUID        REFERENCES posts (id) ON DELETE SET NULL,
    post_number    INTEGER     NOT NULL,
    content        TEXT        NOT NULL,
    reaction_count INTEGER     NOT NULL DEFAULT 0,
    edit_count     INTEGER     NOT NULL DEFAULT 0,
    edited_at      TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at     TIMESTAMPTZ,
    deleted_by     UUID        REFERENCES users (id) ON DELETE SET NULL,
    UNIQUE (topic_id, post_number)
);

CREATE INDEX posts_author_id_idx ON posts (author_id, created_at DESC);
CREATE INDEX posts_board_id_idx ON posts (board_id);
CREATE INDEX posts_parent_id_idx ON posts (parent_id) WHERE parent_id IS NOT NULL;

ALTER TABLE topics ADD CONSTRAINT topics_solution_post_id_fkey
    FOREIGN KEY (solution_post_id) REFERENCES posts (id) ON DELETE SET NULL;

-- Full-text index, one document per live post. The opening post's document also carries
-- the topic title (weight A).
CREATE TABLE search_documents (
    post_id    UUID        PRIMARY KEY REFERENCES posts (id) ON DELETE CASCADE,
    vector     TSVECTOR    NOT NULL,
    indexed_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX search_documents_vector_idx ON search_documents USING gin (vector);

CREATE TABLE post_revisions (
    id         UUID        PRIMARY KEY,
    post_id    UUID        NOT NULL REFERENCES posts (id) ON DELETE CASCADE,
    editor_id  UUID        REFERENCES users (id) ON DELETE SET NULL,
    -- Content as it was before this edit.
    content    TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX post_revisions_post_id_idx ON post_revisions (post_id, created_at);

CREATE TABLE post_reactions (
    post_id    UUID        NOT NULL REFERENCES posts (id) ON DELETE CASCADE,
    user_id    UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    emoji      TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (post_id, emoji, user_id)
);

CREATE INDEX post_reactions_user_id_idx ON post_reactions (user_id);

-- Watch/mute preferences. The most specific target (topic, then board, then ancestor
-- boards, then place) decides whether a notification is delivered.
CREATE TABLE subscriptions (
    user_id     UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    target_type TEXT        NOT NULL CHECK (target_type IN ('place', 'board', 'topic')),
    target_id   UUID        NOT NULL,
    place_id    UUID        NOT NULL REFERENCES places (id) ON DELETE CASCADE,
    level       TEXT        NOT NULL CHECK (level IN ('watching', 'muted')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, target_type, target_id)
);

CREATE INDEX subscriptions_target_idx ON subscriptions (target_id) WHERE level = 'watching';

CREATE TABLE topic_reads (
    user_id               UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    topic_id              UUID        NOT NULL REFERENCES topics (id) ON DELETE CASCADE,
    last_read_post_number INTEGER     NOT NULL,
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, topic_id)
);

CREATE INDEX topic_reads_topic_id_idx ON topic_reads (topic_id);

CREATE TABLE notifications (
    id         UUID        PRIMARY KEY,
    user_id    UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    kind       TEXT        NOT NULL,
    place_id   UUID        REFERENCES places (id) ON DELETE CASCADE,
    topic_id   UUID        REFERENCES topics (id) ON DELETE CASCADE,
    post_id    UUID        REFERENCES posts (id) ON DELETE CASCADE,
    actor_id   UUID        REFERENCES users (id) ON DELETE SET NULL,
    data       JSONB       NOT NULL DEFAULT '{}',
    read_at    TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX notifications_user_id_idx ON notifications (user_id, id DESC);
CREATE INDEX notifications_unread_idx ON notifications (user_id) WHERE read_at IS NULL;
-- One reaction notification per reactor per post, however many emoji they add.
CREATE UNIQUE INDEX notifications_reaction_dedupe ON notifications (user_id, post_id, actor_id)
    WHERE kind = 'reaction';

CREATE TABLE drafts (
    user_id    UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    key        TEXT        NOT NULL,
    data       JSONB       NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, key)
);

CREATE TABLE reports (
    id               UUID        PRIMARY KEY,
    place_id         UUID        NOT NULL REFERENCES places (id) ON DELETE CASCADE,
    reporter_id      UUID        REFERENCES users (id) ON DELETE SET NULL,
    target_type      TEXT        NOT NULL CHECK (target_type IN ('post', 'user')),
    post_id          UUID        REFERENCES posts (id) ON DELETE CASCADE,
    -- The reported user, or the author of the reported post.
    target_user_id   UUID        REFERENCES users (id) ON DELETE SET NULL,
    reason           TEXT        NOT NULL
                                 CHECK (reason IN ('spam', 'harassment', 'inappropriate', 'off_topic', 'other')),
    details          TEXT        NOT NULL DEFAULT '',
    -- The post content when it was reported, so later edits cannot hide evidence.
    content_snapshot TEXT        NOT NULL DEFAULT '',
    status           TEXT        NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'resolved', 'dismissed')),
    resolved_by      UUID        REFERENCES users (id) ON DELETE SET NULL,
    resolved_at      TIMESTAMPTZ,
    resolution_note  TEXT        NOT NULL DEFAULT '',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX reports_place_status_idx ON reports (place_id, status, id DESC);
CREATE UNIQUE INDEX reports_open_dedupe ON reports (reporter_id, target_type, COALESCE(post_id, target_user_id))
    WHERE status = 'open';

CREATE TABLE audit_log (
    id          UUID        PRIMARY KEY,
    place_id    UUID        NOT NULL REFERENCES places (id) ON DELETE CASCADE,
    actor_id    UUID        REFERENCES users (id) ON DELETE SET NULL,
    action      TEXT        NOT NULL,
    target_type TEXT        NOT NULL DEFAULT '',
    target_id   UUID,
    reason      TEXT        NOT NULL DEFAULT '',
    metadata    JSONB       NOT NULL DEFAULT '{}',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX audit_log_place_id_idx ON audit_log (place_id, id DESC);
CREATE INDEX audit_log_target_id_idx ON audit_log (place_id, target_id);

-- +goose Down
DROP TABLE audit_log;
DROP TABLE reports;
DROP TABLE drafts;
DROP TABLE notifications;
DROP TABLE topic_reads;
DROP TABLE subscriptions;
DROP TABLE post_reactions;
DROP TABLE post_revisions;
DROP TABLE search_documents;
ALTER TABLE topics DROP CONSTRAINT topics_solution_post_id_fkey;
DROP TABLE posts;
DROP TABLE topics;
DROP TABLE board_overwrites;
DROP TABLE boards;
ALTER TABLE place_bans DROP COLUMN expires_at;
ALTER TABLE place_members DROP COLUMN timeout_until;
