-- +goose Up

-- Voice channels live alongside text channels, optionally inside a category. Media flows
-- through LiveKit; Gotalk tracks who is connected and what they may do.
ALTER TABLE channels DROP CONSTRAINT channels_kind_check;
ALTER TABLE channels ADD CONSTRAINT channels_kind_check
    CHECK (kind IN ('category', 'text', 'voice', 'thread', 'dm', 'group_dm'));
-- Maximum simultaneous participants of a voice channel; 0 means unlimited.
ALTER TABLE channels ADD COLUMN user_limit INTEGER NOT NULL DEFAULT 0
    CHECK (user_limit BETWEEN 0 AND 99);

DROP INDEX channels_place_idx;
CREATE INDEX channels_place_idx ON channels (place_id, position) WHERE kind IN ('category', 'text', 'voice');

-- Server mute/deafen imposed by moderators. They persist across voice channels of the place.
ALTER TABLE place_members
    ADD COLUMN voice_muted    BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN voice_deafened BOOLEAN NOT NULL DEFAULT false;

-- One row per stay in a voice channel, kept for diagnostics (connection times and the
-- call quality reported by the client).
CREATE TABLE voice_sessions (
    id                UUID             PRIMARY KEY,
    channel_id        UUID             NOT NULL REFERENCES channels (id) ON DELETE CASCADE,
    place_id          UUID             NOT NULL REFERENCES places (id) ON DELETE CASCADE,
    user_id           UUID             NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    started_at        TIMESTAMPTZ      NOT NULL DEFAULT now(),
    connected_at      TIMESTAMPTZ,
    ended_at          TIMESTAMPTZ,
    end_reason        TEXT,
    telemetry_samples INTEGER          NOT NULL DEFAULT 0,
    packet_loss_avg   DOUBLE PRECISION NOT NULL DEFAULT 0,
    packet_loss_max   DOUBLE PRECISION NOT NULL DEFAULT 0,
    jitter_ms_avg     DOUBLE PRECISION NOT NULL DEFAULT 0,
    rtt_ms_avg        DOUBLE PRECISION NOT NULL DEFAULT 0,
    bitrate_kbps_avg  DOUBLE PRECISION NOT NULL DEFAULT 0
);

CREATE INDEX voice_sessions_channel_idx ON voice_sessions (channel_id, started_at DESC);
CREATE INDEX voice_sessions_user_id_idx ON voice_sessions (user_id);
CREATE INDEX voice_sessions_open_idx ON voice_sessions (started_at) WHERE ended_at IS NULL;
CREATE INDEX voice_sessions_ended_idx ON voice_sessions (ended_at) WHERE ended_at IS NOT NULL;

-- Who is in which voice channel right now. A user is in at most one voice channel on the
-- instance; joining another one moves them.
CREATE TABLE voice_states (
    user_id          UUID        PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    channel_id       UUID        NOT NULL REFERENCES channels (id) ON DELETE CASCADE,
    place_id         UUID        NOT NULL REFERENCES places (id) ON DELETE CASCADE,
    voice_session_id UUID        NOT NULL REFERENCES voice_sessions (id) ON DELETE CASCADE,
    -- The login session that joined; ending it disconnects voice.
    auth_session_id  UUID        NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    self_mute        BOOLEAN     NOT NULL DEFAULT false,
    self_deaf        BOOLEAN     NOT NULL DEFAULT false,
    self_video       BOOLEAN     NOT NULL DEFAULT false,
    self_stream      BOOLEAN     NOT NULL DEFAULT false,
    -- Copies of the member's server mute/deafen.
    server_mute      BOOLEAN     NOT NULL DEFAULT false,
    server_deaf      BOOLEAN     NOT NULL DEFAULT false,
    -- What the SFU currently allows, derived from permissions and server mute.
    can_speak        BOOLEAN     NOT NULL DEFAULT false,
    can_stream       BOOLEAN     NOT NULL DEFAULT false,
    -- Set once LiveKit reports the participant connected.
    participant_sid  TEXT,
    connected_at     TIMESTAMPTZ,
    joined_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX voice_states_channel_id_idx ON voice_states (channel_id);
CREATE INDEX voice_states_place_id_idx ON voice_states (place_id);
CREATE INDEX voice_states_auth_session_id_idx ON voice_states (auth_session_id);
CREATE INDEX voice_states_voice_session_id_idx ON voice_states (voice_session_id);

-- +goose Down
DROP TABLE voice_states;
DROP TABLE voice_sessions;
ALTER TABLE place_members DROP COLUMN voice_deafened, DROP COLUMN voice_muted;
DELETE FROM channels WHERE kind = 'voice';
DROP INDEX channels_place_idx;
CREATE INDEX channels_place_idx ON channels (place_id, position) WHERE kind IN ('category', 'text');
ALTER TABLE channels DROP COLUMN user_limit;
ALTER TABLE channels DROP CONSTRAINT channels_kind_check;
ALTER TABLE channels ADD CONSTRAINT channels_kind_check
    CHECK (kind IN ('category', 'text', 'thread', 'dm', 'group_dm'));
