-- name: LockUserVoice :exec
-- Serializes voice changes for one user (joins, moves and leaves) within a transaction.
SELECT pg_advisory_xact_lock(hashtextextended('gotalk:voice:' || @user_id::text, 0));

-- name: TryVoiceSweepLock :one
-- Lets one replica at a time reconcile voice states with LiveKit.
SELECT pg_try_advisory_xact_lock(hashtextextended('gotalk:voice:sweep', 0));

-- name: GetVoiceState :one
SELECT * FROM voice_states WHERE user_id = @user_id;

-- name: UpsertVoiceState :one
-- A new stay in a voice channel; replaces any previous state of the user.
INSERT INTO voice_states (user_id, channel_id, place_id, voice_session_id, auth_session_id,
                          self_mute, self_deaf, server_mute, server_deaf, can_speak, can_stream)
VALUES (@user_id, @channel_id, @place_id, @voice_session_id, @auth_session_id,
        @self_mute, @self_deaf, @server_mute, @server_deaf, @can_speak, @can_stream)
ON CONFLICT (user_id) DO UPDATE
SET channel_id       = EXCLUDED.channel_id,
    place_id         = EXCLUDED.place_id,
    voice_session_id = EXCLUDED.voice_session_id,
    auth_session_id  = EXCLUDED.auth_session_id,
    self_mute        = EXCLUDED.self_mute,
    self_deaf        = EXCLUDED.self_deaf,
    self_video       = false,
    self_stream      = false,
    server_mute      = EXCLUDED.server_mute,
    server_deaf      = EXCLUDED.server_deaf,
    can_speak        = EXCLUDED.can_speak,
    can_stream       = EXCLUDED.can_stream,
    participant_sid  = NULL,
    connected_at     = NULL,
    joined_at        = now(),
    updated_at       = now()
RETURNING *;

-- name: ListPlaceVoiceStates :many
SELECT * FROM voice_states WHERE place_id = @place_id ORDER BY joined_at, user_id;

-- name: ListChannelVoiceStates :many
SELECT * FROM voice_states WHERE channel_id = @channel_id ORDER BY joined_at, user_id;

-- name: ListAllVoiceStates :many
SELECT * FROM voice_states ORDER BY channel_id, joined_at;

-- name: CountOtherVoiceStates :one
SELECT count(*)::integer FROM voice_states WHERE channel_id = @channel_id AND user_id <> @user_id;

-- name: DeleteVoiceState :one
-- Removes one specific stay, so a stale request cannot end a newer one.
DELETE FROM voice_states WHERE user_id = @user_id AND voice_session_id = @voice_session_id
RETURNING *;

-- name: DeleteVoiceParticipant :one
-- Removes the state behind a LiveKit participant that left.
DELETE FROM voice_states
WHERE user_id = @user_id AND channel_id = @channel_id AND participant_sid = @participant_sid
RETURNING *;

-- name: DeleteConnectedRoomVoiceStates :many
DELETE FROM voice_states WHERE channel_id = @channel_id AND connected_at IS NOT NULL
RETURNING *;

-- name: MarkVoiceConnected :one
UPDATE voice_states
SET participant_sid = @participant_sid,
    connected_at    = COALESCE(connected_at, now()),
    updated_at      = now()
WHERE user_id = @user_id AND channel_id = @channel_id
  AND (sqlc.narg('voice_session_id')::uuid IS NULL OR voice_session_id = sqlc.narg('voice_session_id')::uuid)
RETURNING *;

-- name: UpdateVoiceSelf :one
UPDATE voice_states
SET self_mute   = COALESCE(sqlc.narg('self_mute')::boolean, self_mute),
    self_deaf   = COALESCE(sqlc.narg('self_deaf')::boolean, self_deaf),
    self_video  = COALESCE(sqlc.narg('self_video')::boolean, self_video),
    self_stream = COALESCE(sqlc.narg('self_stream')::boolean, self_stream),
    updated_at  = now()
WHERE user_id = @user_id
RETURNING *;

-- name: UpdateVoiceGrant :one
-- Losing the right to stream also turns the stream flags off.
UPDATE voice_states
SET server_mute = @server_mute,
    server_deaf = @server_deaf,
    can_speak   = @can_speak,
    can_stream  = @can_stream,
    self_video  = self_video AND @can_stream,
    self_stream = self_stream AND @can_stream,
    updated_at  = now()
WHERE user_id = @user_id AND voice_session_id = @voice_session_id
RETURNING *;

-- name: CreateVoiceSession :one
INSERT INTO voice_sessions (id, channel_id, place_id, user_id)
VALUES (@id, @channel_id, @place_id, @user_id)
RETURNING *;

-- name: ConnectVoiceSession :exec
UPDATE voice_sessions SET connected_at = COALESCE(connected_at, now()) WHERE id = @id;

-- name: EndVoiceSession :exec
UPDATE voice_sessions SET ended_at = now(), end_reason = @end_reason
WHERE id = @id AND ended_at IS NULL;

-- name: EndOrphanedVoiceSessions :exec
-- Closes sessions whose state disappeared without ending them (e.g. a crash mid-request).
UPDATE voice_sessions s SET ended_at = now(), end_reason = 'disconnected'
WHERE s.ended_at IS NULL AND s.started_at < @before
  AND NOT EXISTS (SELECT 1 FROM voice_states v WHERE v.voice_session_id = s.id);

-- name: PruneVoiceSessions :exec
DELETE FROM voice_sessions WHERE ended_at < @before;

-- name: RecordVoiceTelemetry :exec
-- Folds one client report into running averages.
UPDATE voice_sessions
SET packet_loss_avg   = (packet_loss_avg * telemetry_samples + @packet_loss::float8) / (telemetry_samples + 1),
    packet_loss_max   = GREATEST(packet_loss_max, @packet_loss::float8),
    jitter_ms_avg     = (jitter_ms_avg * telemetry_samples + @jitter_ms::float8) / (telemetry_samples + 1),
    rtt_ms_avg        = (rtt_ms_avg * telemetry_samples + @rtt_ms::float8) / (telemetry_samples + 1),
    bitrate_kbps_avg  = (bitrate_kbps_avg * telemetry_samples + @bitrate_kbps::float8) / (telemetry_samples + 1),
    telemetry_samples = telemetry_samples + 1
WHERE id = @id AND ended_at IS NULL;

-- name: ListChannelVoiceSessions :many
SELECT * FROM voice_sessions
WHERE channel_id = @channel_id
ORDER BY started_at DESC, id DESC
LIMIT @lim OFFSET @off;

-- name: GetMemberVoiceFlags :one
SELECT voice_muted, voice_deafened FROM place_members WHERE place_id = @place_id AND user_id = @user_id;

-- name: SetMemberVoiceFlags :one
UPDATE place_members
SET voice_muted    = COALESCE(sqlc.narg('muted')::boolean, voice_muted),
    voice_deafened = COALESCE(sqlc.narg('deafened')::boolean, voice_deafened)
WHERE place_id = @place_id AND user_id = @user_id
RETURNING voice_muted, voice_deafened;
