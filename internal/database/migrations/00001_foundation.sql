-- +goose Up

-- Singleton row holding instance-wide settings and first-run setup state.
CREATE TABLE instance_settings (
    id                 SMALLINT    PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    name               TEXT        NOT NULL DEFAULT 'Gotalk',
    description        TEXT        NOT NULL DEFAULT '',
    icon_url           TEXT,
    registration_mode  TEXT        NOT NULL DEFAULT 'open'
                                   CHECK (registration_mode IN ('open', 'invite_only', 'closed')),
    setup_token        TEXT,
    setup_completed_at TIMESTAMPTZ,
    jwt_secret         BYTEA       NOT NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE users (
    id                UUID        PRIMARY KEY,
    username          TEXT        NOT NULL,
    email             TEXT        NOT NULL,
    password_hash     TEXT        NOT NULL,
    display_name      TEXT        NOT NULL DEFAULT '',
    bio               TEXT        NOT NULL DEFAULT '',
    pronouns          TEXT        NOT NULL DEFAULT '',
    avatar_url        TEXT,
    is_instance_admin BOOLEAN     NOT NULL DEFAULT false,
    email_verified_at TIMESTAMPTZ,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at        TIMESTAMPTZ
);

-- Usernames stay reserved after account deletion; emails are scrubbed on deletion.
CREATE UNIQUE INDEX users_username_key ON users (lower(username));
CREATE UNIQUE INDEX users_email_key ON users (lower(email));

CREATE TABLE sessions (
    id                 UUID        PRIMARY KEY,
    user_id            UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    refresh_token_hash BYTEA       NOT NULL,
    user_agent         TEXT        NOT NULL DEFAULT '',
    ip_address         TEXT        NOT NULL DEFAULT '',
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at         TIMESTAMPTZ NOT NULL,
    revoked_at         TIMESTAMPTZ
);

CREATE INDEX sessions_user_id_idx ON sessions (user_id) WHERE revoked_at IS NULL;

CREATE TABLE places (
    id           UUID        PRIMARY KEY,
    slug         TEXT        NOT NULL,
    name         TEXT        NOT NULL,
    description  TEXT        NOT NULL DEFAULT '',
    icon_url     TEXT,
    banner_url   TEXT,
    visibility   TEXT        NOT NULL DEFAULT 'public'
                             CHECK (visibility IN ('public', 'invite_only', 'private')),
    is_nsfw      BOOLEAN     NOT NULL DEFAULT false,
    locale       TEXT        NOT NULL DEFAULT 'en',
    owner_id     UUID        NOT NULL REFERENCES users (id),
    member_count INTEGER     NOT NULL DEFAULT 0,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at   TIMESTAMPTZ
);

CREATE UNIQUE INDEX places_slug_key ON places (lower(slug)) WHERE deleted_at IS NULL;
CREATE INDEX places_discovery_idx ON places (member_count DESC, id)
    WHERE deleted_at IS NULL AND visibility <> 'private';
CREATE INDEX places_owner_id_idx ON places (owner_id) WHERE deleted_at IS NULL;

CREATE TABLE place_members (
    place_id  UUID        NOT NULL REFERENCES places (id) ON DELETE CASCADE,
    user_id   UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    nickname  TEXT,
    joined_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (place_id, user_id)
);

CREATE INDEX place_members_user_id_idx ON place_members (user_id);

-- Higher position = higher in the hierarchy. The default (@everyone) role is always position 0.
CREATE TABLE roles (
    id          UUID        PRIMARY KEY,
    place_id    UUID        NOT NULL REFERENCES places (id) ON DELETE CASCADE,
    name        TEXT        NOT NULL,
    color       INTEGER     NOT NULL DEFAULT 0,
    position    INTEGER     NOT NULL DEFAULT 0,
    permissions BIGINT      NOT NULL DEFAULT 0,
    is_default  BOOLEAN     NOT NULL DEFAULT false,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX roles_place_id_idx ON roles (place_id, position);
CREATE UNIQUE INDEX roles_one_default_per_place ON roles (place_id) WHERE is_default;

CREATE TABLE member_roles (
    place_id UUID NOT NULL,
    user_id  UUID NOT NULL,
    role_id  UUID NOT NULL REFERENCES roles (id) ON DELETE CASCADE,
    PRIMARY KEY (place_id, user_id, role_id),
    FOREIGN KEY (place_id, user_id) REFERENCES place_members (place_id, user_id) ON DELETE CASCADE
);

CREATE INDEX member_roles_role_id_idx ON member_roles (role_id);

CREATE TABLE place_bans (
    place_id   UUID        NOT NULL REFERENCES places (id) ON DELETE CASCADE,
    user_id    UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    reason     TEXT        NOT NULL DEFAULT '',
    banned_by  UUID        REFERENCES users (id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (place_id, user_id)
);

CREATE TABLE invites (
    code       TEXT        PRIMARY KEY,
    place_id   UUID        NOT NULL REFERENCES places (id) ON DELETE CASCADE,
    created_by UUID        REFERENCES users (id) ON DELETE SET NULL,
    max_uses   INTEGER,
    uses       INTEGER     NOT NULL DEFAULT 0,
    expires_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX invites_place_id_idx ON invites (place_id);

-- +goose Down
DROP TABLE invites;
DROP TABLE place_bans;
DROP TABLE member_roles;
DROP TABLE roles;
DROP TABLE place_members;
DROP TABLE places;
DROP TABLE sessions;
DROP TABLE users;
DROP TABLE instance_settings;
