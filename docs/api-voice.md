# Voice

[← API overview](api-overview.md) · [Documentation](README.md)

Voice and video run through [LiveKit](https://livekit.io). To set up a LiveKit server see
[Voice channels](getting-started.md#voice-channels) and the [`voice.*` settings](configuration.md#voice).

## On this page

- [Joining a voice channel](#joining-a-voice-channel)
- [Permissions during a call](#permissions-during-a-call)
- [Voice state and moderation](#voice-state-and-moderation)
- [Call quality](#call-quality)
- [Keeping state in sync](#keeping-state-in-sync)

## Joining a voice channel

Places can have `voice` channels next to their text channels, optionally
inside categories and with a `user_limit` (up to 99; `MOVE_MEMBERS` bypasses it). Media goes
through LiveKit, not through Gotalk: `POST /channels/{id}/voice` checks `CONNECT_VOICE` and
returns the LiveKit `url` plus a short-lived `token` for the channel's room, and the client
connects with any LiveKit SDK, which handles WebRTC signaling and media. The token encodes
what the caller may publish: the microphone needs `SPEAK`, and camera and screen sharing need
`SHARE_SCREEN`. Timed-out members can listen but not speak or share. A user is in one voice
channel at a time; joining another one moves them. Voice channel overwrites take the voice
permissions (`VIEW_CHANNELS`, `MANAGE_CHANNELS`, `CONNECT_VOICE`, `SPEAK`, `SHARE_SCREEN`,
`MUTE_MEMBERS`, `MOVE_MEMBERS`), and categories take chat and voice permissions alike.

## Permissions during a call

Permissions are enforced for the whole stay, not only at join time. When roles, overwrites,
timeouts or membership change, Gotalk updates each participant's permissions in LiveKit
(which stops tracks that are no longer allowed) or disconnects them if they lost access.
Ending the session that joined (logout, revocation, …) also disconnects them.

## Voice state and moderation

Clients report their own `self_mute`, `self_deaf`, `self_video` and `self_stream` state with
`PATCH /users/@me/voice` and speaking indicators with the gateway op
`{"op":"voice_speaking","d":{"speaking":true}}`; Gotalk relays both to everyone who can see
the channel. Push-to-talk and voice activity detection are client-side.
`GET /places/{place}/voice-states` lists who is in which channel. Moderators can server-mute or
deafen members (`MUTE_MEMBERS`; the mute or deafen persists across the place's voice channels), move
them between channels (`MOVE_MEMBERS` in both channels; the member receives a
`VOICE_SERVER_UPDATE` with a token for the new room), or disconnect them (`MOVE_MEMBERS`). All of
this is audited.

## Call quality

Clients may send call quality samples (packet loss, jitter, round-trip time,
bitrate) to `POST /users/@me/voice/telemetry`. `MANAGE_CHANNELS` sees each session's averages
under `GET /channels/{id}/voice/sessions` for troubleshooting.

## Keeping state in sync

Gotalk learns that a participant connected or left from LiveKit webhooks, and also checks
every 30 seconds against LiveKit's room list, so missed webhooks heal themselves. Joins that
never connect expire after `voice.join_timeout`.
