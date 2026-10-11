# Real-time gateway

[← API overview](api-overview.md) · [Documentation](README.md)

Connect a WebSocket to `gateway_url` from `/api/v1/instance` (`/api/v1/gateway`). Every
frame is a JSON object with an `op`, plus `d` (data) and, for events, `t` (type).

## On this page

- [Connection flow](#connection-flow)
- [Events](#events)
- [Permissions and sessions](#permissions-and-sessions)
- [Close codes](#close-codes)

## Connection flow

1. The server sends `{"op":"hello","d":{"heartbeat_interval":30000}}`.
2. Within 10 seconds, send `{"op":"identify","d":{"token":"<access token>","status":"online"}}`.
   The server replies with a `READY` event containing `user`, `session_id`, `place_ids` and
   `voice_state` (null when not in a voice channel). Personal access tokens with the `gateway`
   scope and bot tokens work too; their `session_id` is the token's ID.
3. Send `{"op":"heartbeat"}` every `heartbeat_interval` milliseconds; the server answers
   `{"op":"heartbeat_ack"}`. Change status with
   `{"op":"presence_update","d":{"status":"idle"}}`, and relay speaking indicators while in
   voice with `{"op":"voice_speaking","d":{"speaking":true}}`.
4. Events arrive as `{"op":"dispatch","t":"MESSAGE_CREATE","d":{…}}`. Their payloads use the
   same JSON shapes as the REST API.

## Events

| Event | Sent to |
|---|---|
| `READY` | The connecting session |
| `MESSAGE_CREATE`, `MESSAGE_UPDATE` (edits, pins, new threads), `MESSAGE_DELETE` | Everyone who can see the channel |
| `MESSAGE_REACTION_ADD`, `MESSAGE_REACTION_REMOVE`, `TYPING_START` | Everyone who can see the channel |
| `CHANNEL_CREATE`, `CHANNEL_UPDATE`, `CHANNEL_DELETE` | Everyone who can see the channel (conversation participants for DMs) |
| `CHANNEL_RECIPIENT_ADD`, `CHANNEL_RECIPIENT_REMOVE` | Group conversation participants |
| `CHANNEL_READ` | The reader's other sessions |
| `TOPIC_READ_STATE_UPDATE` | The reader's sessions: `topics` lists changed read states; after marking a place read it is empty, with `all: true`, `place_id`, `board_id` and `before` |
| `READ_RECEIPT` | The other participants of a direct conversation |
| `NOTIFICATION_CREATE` | The notified user (forum and chat notifications) |
| `PRESENCE_UPDATE` | Members of the user's places and their conversation partners |
| `PLACE_JOIN`, `PLACE_LEAVE`, `PLACE_DELETE` | The member (or every member, for deletion) |
| `VOICE_STATE_UPDATE` | The user and everyone who can see the voice channel; `channel_id` is null when the user left it |
| `VOICE_SERVER_UPDATE` | A member who was moved to another voice channel (new `url`, `token` and `room`) |
| `VOICE_SPEAKING` | Everyone who can see the voice channel |
| `INTERACTION_CREATE` | The bot whose slash command was invoked |

## Permissions and sessions

Permission changes such as roles, overwrites, joins and kicks take effect immediately: a
kicked member stops receiving the place's events. Ending a session (logout, revocation,
password change, account deletion, or refresh-token reuse) closes its gateway connection.
Sessions cannot be resumed; after reconnecting, clients refetch what they need with
`after=<last message ID>`.

When running several replicas, set `redis.url` so events reach clients connected to any
replica; see [Deployment](deployment.md#cloud-and-multi-replica-deployments). Reverse proxies must pass WebSocket
upgrades through and keep the idle timeout above the 30 second heartbeat.

## Close codes

| Close code | Meaning |
|---|---|
| `1001` | Server shutting down; reconnect |
| `4000` | Internal error |
| `4001` | Invalid frame, unknown op, or invalid status |
| `4002` | Sent something before `identify` |
| `4003` | Sent `identify` twice |
| `4004` | Authentication failed, or the API token lacks the `gateway` scope |
| `4007` | Events may have been missed (e.g. the server lost its Redis connection); reconnect |
| `4008` | Too many frames (over 120 per minute) or too slow to read events |
| `4009` | Missed heartbeats |
| `4010` | Session ended |
