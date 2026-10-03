# gotalk-server

The backend for [Gotalk](../../README.md): a forum-first, self-hostable alternative to Discord.
This repository implements **Phase 1 (Foundation)**, **Phase 2 (Forum Core)**,
**Phase 3 (Real-Time Layer)** and **Phase 4 (Voice)** of the
[backend plan](../../docs/backend-plan.md): accounts, places, roles and permissions, invites
and bans, the first-run setup wizard, boards, topics and posts, full-text search,
notifications, moderation tools, chat channels and threads, direct and group messages,
presence, a WebSocket gateway, and voice/video channels routed through a self-hosted
[LiveKit](https://livekit.io) server, plus the infrastructure to run it anywhere from a
Raspberry Pi to Kubernetes.

- Single static Go binary (~20 MB distroless image), PostgreSQL required, Redis and LiveKit optional
- Browser setup wizard **or** fully headless setup from environment variables
- Any client can point at any instance: discovery via `/.well-known/gotalk-instance`
  and `GET /api/v1/instance`
- OpenAPI 3.1 spec and interactive docs served at `/api/v1/docs`; real-time events over a
  WebSocket at `/api/v1/gateway`

## Quick start (Docker Compose)

```sh
docker compose up -d
docker compose logs gotalk     # copy the setup link it prints
```

Open the link (`http://localhost:8080/setup?token=…`), and the wizard walks you through
pre-flight checks, naming your instance, and creating the administrator account. That's it.

For anything beyond trying it out locally, copy [.env.example](.env.example) to `.env` and
set at least `POSTGRES_PASSWORD` and `GOTALK_SERVER_PUBLIC_URL`.

### Skip the browser (headless setup)

Set all three admin variables and setup completes on boot:

```sh
GOTALK_SETUP_ADMIN_USERNAME=admin \
GOTALK_SETUP_ADMIN_EMAIL=admin@example.com \
GOTALK_SETUP_ADMIN_PASSWORD=change-me-please \
docker compose up -d
```

Or run it once as a job (e.g. a Kubernetes `Job` or CI step) with `gotalk setup`. Both are
idempotent: on an already configured instance they do nothing.

### Voice channels

Voice and video need a [LiveKit](https://livekit.io) server, which routes the media. The
Compose file includes one behind the `voice` profile, already set up to send its webhooks to
Gotalk:

```sh
GOTALK_VOICE_LIVEKIT_URL=ws://localhost:7880 docker compose --profile voice up -d
```

That works for clients on the same machine. For others, set `GOTALK_VOICE_LIVEKIT_URL` to an
address they can reach (`wss://…` when served over TLS), `LIVEKIT_NODE_IP` to the host's
public IP, open ports 7880/tcp, 7881/tcp and 7882/udp, and change `LIVEKIT_API_SECRET`. To use
an existing or managed LiveKit deployment instead, set the `voice.*` settings below and point
its webhook at `https://<your instance>/api/v1/voice/webhook`, signed with the same API key.
Webhooks are optional: without them Gotalk polls LiveKit every 30 seconds.

## Running the binary

```sh
go build -o gotalk ./cmd/gotalk
GOTALK_DATABASE_URL="host=localhost user=gotalk dbname=gotalk sslmode=disable" PGPASSWORD=… ./gotalk serve
```

Without `GOTALK_DATABASE_URL`, the server connects to a local PostgreSQL database named
`gotalk` (user and password `gotalk`) on `localhost:5432`. Migrations run automatically on
boot.

| Command | Purpose |
|---|---|
| `gotalk serve` | Run the server (default when no command is given) |
| `gotalk migrate` / `gotalk migrate status` | Apply migrations / show schema version |
| `gotalk setup` | Complete first-run setup from config/env, then exit |
| `gotalk healthcheck` | Exit 0 if the local server is healthy (used by the image's `HEALTHCHECK`) |
| `gotalk version` | Print the version |

All commands accept `--config path/to/gotalk.yaml` (alias `--from-file`).

## Deploying to the cloud

The same binary is built for unattended, horizontally scaled deployments:

- **12-factor config.** Every setting is an environment variable. `DATABASE_URL`,
  `REDIS_URL` and `PORT` (as injected by Heroku, Render, Fly, Cloud Run, …) are honored
  automatically.
- **Secrets from files.** Append `_FILE` to any variable to read it from a mounted file,
  e.g. `GOTALK_SETUP_ADMIN_PASSWORD_FILE=/run/secrets/admin-password`. libpq variables such as
  `PGPASSWORD` also work.
- **Managed dependencies.** Point `GOTALK_DATABASE_URL` at any managed PostgreSQL and
  `GOTALK_REDIS_URL` at any managed Redis. The Compose services are a local convenience.
- **Multiple replicas.** Migrations take a PostgreSQL advisory lock so replicas can boot
  together, setup completion is race-safe, and the JWT secret is shared through the
  database. Set `GOTALK_REDIS_URL` so rate limits, presence and gateway events are shared
  across replicas. Without Redis, a client connected to one replica would miss events
  caused by requests served by another.
- **Probes.** `GET /healthz` is liveness (process is serving). `GET /readyz` is readiness
  (database and Redis reachable); it reports `awaiting_setup` with HTTP 200 so the wizard
  stays reachable through your load balancer, and `degraded` (still HTTP 200) when LiveKit is
  configured but unreachable, since only voice is affected. SIGTERM drains in-flight requests and closes
  gateway connections with code `1001` so clients reconnect to another replica.
- **Reverse proxies.** TLS is expected to terminate in front of Gotalk. Set
  `GOTALK_SERVER_TRUST_PROXY=true` so client IPs and the public URL are taken from
  `X-Forwarded-*` headers. Those headers are only honored when the request arrives from an
  address in `GOTALK_SERVER_TRUSTED_PROXIES` (loopback and private ranges by default). The
  proxy must pass WebSocket upgrades through for `/api/v1/gateway`, and its idle timeout
  should exceed the gateway's 30 second heartbeat.

## Configuration

Sources are layered: built-in defaults, then the `--config` YAML file
([config.example.yaml](config.example.yaml)), then environment variables. Environment names
are `GOTALK_` + section + `_` + key, upper-cased: `server.public_url` becomes
`GOTALK_SERVER_PUBLIC_URL`. Lists are comma-separated.

| Key | Default | Description |
|---|---|---|
| `server.addr` | `:8080` | Listen address |
| `server.public_url` | *(derived per request)* | External base URL, e.g. `https://forum.example.com` |
| `server.trust_proxy` | `false` | Honor `X-Forwarded-*` from trusted proxies |
| `server.trusted_proxies` | loopback + private ranges | CIDRs allowed to set forwarding headers |
| `server.cors_allowed_origins` | `*` | Origins allowed to call the API and open gateway connections (any client by default) |
| `server.cors_allow_credentials` | `false` | Allow credentialed CORS (cannot be combined with `*`) |
| `server.shutdown_timeout` | `20s` | Graceful shutdown window |
| `database.url` | local `gotalk` database | PostgreSQL URL or keyword/value DSN |
| `database.max_conns` | `20` | Connection pool size |
| `database.auto_migrate` | `true` | Apply migrations on `serve` |
| `database.connect_timeout` | `30s` | How long to wait for PostgreSQL/Redis at startup |
| `redis.url` | *(none)* | Optional; shares rate limits, presence and gateway events across replicas |
| `auth.jwt_secret` | *(generated, stored in DB)* | HMAC key for access tokens; at least 32 characters |
| `auth.access_token_ttl` | `15m` | Access token lifetime |
| `auth.refresh_token_ttl` | `720h` | Refresh token (session) lifetime |
| `ratelimit.enabled` | `true` | Enable rate limiting |
| `ratelimit.default` | `300-M` | Default tier, per user (or per IP when anonymous) |
| `ratelimit.auth` | `10-M` | Login, registration, refresh, setup, and password endpoints |
| `ratelimit.content` | `30-M` | Creating topics, replies, and reports |
| `ratelimit.chat` | `120-M` | Sending chat messages, reacting to them, typing indicators, and starting threads |
| `log.level` | `info` | `debug`, `info`, `warn`, `error` |
| `log.format` | `json` | `json` or `text` |
| `setup.token` | *(generated)* | Override the browser wizard's one-time token |
| `setup.instance_name` | `Gotalk` | Instance name for headless setup / wizard default |
| `setup.instance_description` | | Instance description for headless setup |
| `setup.registration_mode` | `open` | `open`, `invite_only`, or `closed` |
| `setup.admin_username` / `admin_email` / `admin_password` | | Set all three for headless setup |
| `voice.livekit_url` | *(none)* | LiveKit URL clients connect to (`ws`, `wss`, `http` or `https`); setting it enables voice |
| `voice.livekit_api_url` | `voice.livekit_url` | How this server reaches LiveKit's API, when that differs (e.g. `http://livekit:7880`) |
| `voice.livekit_api_key` / `livekit_api_secret` | | LiveKit API credentials; the secret must be at least 32 characters |
| `voice.token_ttl` | `10m` | How long a voice join token can be used to connect |
| `voice.join_timeout` | `60s` | Joins that never connect to LiveKit are dropped after this |
| `voice.session_retention` | `720h` | How long ended voice sessions and their call quality are kept |

Rates use `<limit>-<period>` where period is `S`, `M`, `H`, or `D`.

## API overview

Everything lives under `/api/v1`. The full, always-current contract is the OpenAPI document
at `/api/v1/openapi.json` (browse it at `/api/v1/docs`).

**Connecting a client.** Given only a domain, fetch `/.well-known/gotalk-instance` to find
the API base URL and gateway URL, then `GET /api/v1/instance` for the instance name,
registration mode, supported API versions, feature flags, and published rate limits.

**Authentication.** `POST /auth/register` or `/auth/login` returns a short-lived JWT access
token (send as `Authorization: Bearer …`) and a single-use refresh token. `POST /auth/refresh`
rotates both; presenting an already-used refresh token revokes the whole session. Sessions
can be listed and revoked under `/users/@me/sessions`.

**Places.** Communities are *places* with `public` (listed, open to join), `invite_only`
(listed, join by invite) or `private` (unlisted, hidden from non-members) visibility. Places
are addressed by ID or slug: `/places/{place}`.

**Roles and permissions.** Each place has an implicit `@everyone` role plus custom roles
ordered by `position`. Permissions are a bitfield (list them at `GET /permissions`); a
member's permissions are the union of their roles. Owners and `ADMINISTRATOR` hold
everything. Members can only manage roles and members ranked below their highest role, and
can only grant permissions they hold.

**Boards.** A place's forum is a tree of boards: top-level `category` entries group
`board`s, and boards can nest one more level. Each board can be `flat` or `threaded`, and
can enable Q&A *solutions*. Boards may carry permission *overwrites* that allow or deny
forum permissions (`VIEW_BOARDS`, `CREATE_TOPICS`, `REPLY_TO_TOPICS`, `ADD_REACTIONS`,
`ATTACH_FILES`, `MANAGE_BOARDS`, `MANAGE_POSTS`) for a role. Overwrites apply from the root
board down: at each level the `@everyone` overwrite applies first, then the member's other
roles, so a role allow beats an `@everyone` deny. This covers staff-only boards
(deny `VIEW_BOARDS` for `@everyone`, allow it for staff), announcement boards
(deny `CREATE_TOPICS`), and per-board moderators (allow `MANAGE_POSTS`).

**Reading without an account.** Anyone, signed in or not, can read the boards of a
`public` place that `@everyone` can see. `invite_only` and `private` places are
members-only. Only members can post, react, report users, or subscribe.

**Topics and posts.** Content is Markdown. A topic is created together with its opening
post (post number 1); replies get stable, increasing post numbers. Replies may set
`parent_id`; threaded boards list posts depth-first with a `depth`. Edits keep the previous
version (`/posts/{id}/revisions`). Deleted replies stay in the thread as tombstones with
empty content, which only `MANAGE_POSTS` can still read. Authors can edit their posts and
delete their replies. They can also delete their topics until someone replies. `MANAGE_POSTS`
can do all of this to anyone's content, and can pin, lock, archive or move topics. Archived
topics are read-only and hidden from listings unless `archived=true`. Reactions accept a
Unicode emoji (URL-encoded in the path) or a shortcode, up to 20 distinct per post.
`@username` mentions notify the mentioned user if they can see the board.

**Read state, subscriptions, notifications.** `PUT /topics/{id}/read` records how far a user
has read; authenticated topic listings include `last_read_post_number` and `unread_count`.
Places, boards and topics can be set to `watching`, `normal` or `muted`. Watching a place
or board notifies about new topics, and watching a topic notifies about every reply. Authors
watch their own topics automatically. The most specific setting wins, so watching a topic
inside a muted place still notifies. Notifications (`mention`, `reply`, `topic_reply`,
`new_topic`, `reaction`, `solution`, `moderation`, `direct_message`) carry a `data` snapshot
for rendering and are pushed to the recipient's gateway sessions as they are created.
Moderation notices ignore mutes.

**Search.** `GET /search` searches everything signed-out visitors can read across the
instance; `GET /places/{place}/search` searches every board the caller can read. Queries use
web-search syntax (`"exact phrase"`, `or`, `-exclude`). They can be filtered by `author`,
`tag`, `board`, `solved`, `after`/`before` and `topics_only`, and sorted by `relevance`
(text rank blended with reactions and recency), `newest` or `oldest`. Snippets wrap
matches in Markdown `**bold**`. Indexing uses PostgreSQL full-text search (language-neutral
`simple` configuration) and happens in the same transaction as each write, so results are
never stale.

**Moderation.** Members report posts, chat messages, or other members to
`POST /places/{place}/reports`. Reports snapshot the content and land in a queue
(`MANAGE_REPORTS`) to be resolved or dismissed. `MODERATE_MEMBERS` can warn members or time
them out for up to 28 days. Timed-out members keep read access but lose posting, replying,
messaging, reacting, nickname and invite rights.
Bans may be temporary (`duration` in seconds). Every moderation and administrative action,
including role, board and place changes, is recorded in the audit log (`VIEW_AUDIT_LOG`).
Filter the log by action (`member.ban`, or a whole category such as `member`), actor, or
target. Optional `?reason=` on kick/delete requests is recorded and shown to the affected
user.

**Drafts.** Clients can sync unfinished posts across devices as JSON objects under
`/users/@me/drafts/{key}` (for example `topic:<boardID>` or `reply:<topicID>`).

**Channels.** Alongside its forum, each place has chat channels: top-level `category`
entries group `text` channels. Chat is members-only, even in public places. Categories and
text channels can carry role overwrites for the chat permissions (`VIEW_CHANNELS`,
`SEND_MESSAGES`, `MANAGE_MESSAGES`, `MANAGE_CHANNELS`, `ADD_REACTIONS`, `ATTACH_FILES`).
They apply from the category down to the channel, using the same rules as board
overwrites. `MANAGE_CHANNELS` creates, edits, reorders and deletes channels. Deleting a
channel removes its messages and threads, and deleting a category moves its channels to the
top level. A *thread* is a sub-channel of a text channel, optionally started from one of its
messages, and inherits the channel's permissions. Threads can be archived by their creator
or by `MANAGE_MESSAGES`, and a new message unarchives them.

**Messages.** Message IDs are time-ordered UUIDv7s. `GET /channels/{id}/messages` returns
history in chronological order: the latest page by default, or the page `before`, `after`
or `around` a message ID. Messages are Markdown, up to 4,000 characters, and may set
`reply_to_id`. They also accept a `nonce` that is echoed back so clients can reconcile
optimistic sends. Authors can edit their own messages (previous versions are kept for the
author and `MANAGE_MESSAGES`) and delete them. `MANAGE_MESSAGES` can delete anyone's message
in place channels. The deletion is audited, and the author is told why. Deleted messages are
removed outright; reports keep a snapshot. Messages support pins (up to 50 per channel) and
emoji reactions. `@username` mentions and replies notify members who can see the channel,
unless they muted the channel, its category, or the place.

**Read state.** `PUT /channels/{id}/read` moves the caller's read position forward. Channel
listings include `read_state` (`last_read_message_id`, `mention_count`) and `unread`. In
direct messages every unread message counts toward `mention_count`; elsewhere only mentions
and replies do. Reading a channel also marks the notifications it caused as read.

**Direct messages.** `POST /users/@me/channels` with one recipient returns the caller's
one-to-one conversation with them, creating it if needed. Several recipients start a group
conversation (up to 10 people, with an owner and an optional name). You can only message
people you share a place with; instance administrators are exempt. Group members can add
people they share a place with. Anyone may leave, and the owner may remove others. When the
owner leaves, the longest-standing member takes over. Participants see read receipts
(`GET /channels/{id}/receipts`) and may pin messages. The first unread message of a
conversation creates a `direct_message` notification; the read state counts the rest.

**Presence and typing.** Gateway connections report `online`, `idle`, `dnd` or `invisible`.
A user's presence is their most present connection (invisible counts as offline). People who
share a place or a conversation with a user can read their presence via
`GET /presences?user_ids=…`. `POST /channels/{id}/typing` shows the caller as typing for
about 10 seconds.

**Voice channels.** Places can have `voice` channels next to their text channels, optionally
inside categories and with a `user_limit` (up to 99; `MOVE_MEMBERS` bypasses it). Media goes
through LiveKit, not through Gotalk: `POST /channels/{id}/voice` checks `CONNECT_VOICE` and
returns the LiveKit `url` plus a short-lived `token` for the channel's room, and the client
connects with any LiveKit SDK, which handles WebRTC signaling and media. The token encodes
what the caller may publish: the microphone needs `SPEAK`, and camera and screen sharing need
`SHARE_SCREEN`. Timed-out members can listen but not speak or share. A user is in one voice
channel at a time; joining another one moves them. Voice channel overwrites take the voice
permissions (`VIEW_CHANNELS`, `MANAGE_CHANNELS`, `CONNECT_VOICE`, `SPEAK`, `SHARE_SCREEN`,
`MUTE_MEMBERS`, `MOVE_MEMBERS`), and categories take chat and voice permissions alike.

Permissions are enforced for the whole stay, not only at join time. When roles, overwrites,
timeouts or membership change, Gotalk updates each participant's permissions in LiveKit
(which stops tracks that are no longer allowed) or disconnects them if they lost access.
Ending the session that joined (logout, revocation, …) also disconnects them.

Clients report their own `self_mute`, `self_deaf`, `self_video` and `self_stream` state with
`PATCH /users/@me/voice` and speaking indicators with the gateway op
`{"op":"voice_speaking","d":{"speaking":true}}`; Gotalk relays both to everyone who can see
the channel. Push-to-talk and voice activity detection are client-side. `GET
/places/{place}/voice-states` lists who is in which channel. Moderators can server-mute or
deafen members (`MUTE_MEMBERS`; the mute or deafen persists across the place's voice channels), move
them between channels (`MOVE_MEMBERS` in both channels; the member receives a
`VOICE_SERVER_UPDATE` with a token for the new room), or disconnect them (`MOVE_MEMBERS`). All of
this is audited. Clients may send call quality samples (packet loss, jitter, round-trip time,
bitrate) to `POST /users/@me/voice/telemetry`. `MANAGE_CHANNELS` sees each session's averages
under `GET /channels/{id}/voice/sessions` for troubleshooting.

Gotalk learns that a participant connected or left from LiveKit webhooks, and also checks
every 30 seconds against LiveKit's room list, so missed webhooks heal themselves. Joins that
never connect expire after `voice.join_timeout`.

### Real-time gateway

Connect a WebSocket to `gateway_url` from `/api/v1/instance` (`/api/v1/gateway`). Every
frame is a JSON object with an `op`, plus `d` (data) and, for events, `t` (type).

1. The server sends `{"op":"hello","d":{"heartbeat_interval":30000}}`.
2. Within 10 seconds, send `{"op":"identify","d":{"token":"<access token>","status":"online"}}`.
   The server replies with a `READY` event containing `user`, `session_id`, `place_ids` and
   `voice_state` (null when not in a voice channel).
3. Send `{"op":"heartbeat"}` every `heartbeat_interval` milliseconds; the server answers
   `{"op":"heartbeat_ack"}`. Change status with
   `{"op":"presence_update","d":{"status":"idle"}}`, and relay speaking indicators while in
   voice with `{"op":"voice_speaking","d":{"speaking":true}}`.
4. Events arrive as `{"op":"dispatch","t":"MESSAGE_CREATE","d":{…}}`. Their payloads use the
   same JSON shapes as the REST API.

| Event | Sent to |
|---|---|
| `READY` | The connecting session |
| `MESSAGE_CREATE`, `MESSAGE_UPDATE` (edits, pins, new threads), `MESSAGE_DELETE` | Everyone who can see the channel |
| `MESSAGE_REACTION_ADD`, `MESSAGE_REACTION_REMOVE`, `TYPING_START` | Everyone who can see the channel |
| `CHANNEL_CREATE`, `CHANNEL_UPDATE`, `CHANNEL_DELETE` | Everyone who can see the channel (conversation participants for DMs) |
| `CHANNEL_RECIPIENT_ADD`, `CHANNEL_RECIPIENT_REMOVE` | Group conversation participants |
| `CHANNEL_READ` | The reader's other sessions |
| `READ_RECEIPT` | The other participants of a direct conversation |
| `NOTIFICATION_CREATE` | The notified user (forum and chat notifications) |
| `PRESENCE_UPDATE` | Members of the user's places and their conversation partners |
| `PLACE_JOIN`, `PLACE_LEAVE`, `PLACE_DELETE` | The member (or every member, for deletion) |
| `VOICE_STATE_UPDATE` | The user and everyone who can see the voice channel; `channel_id` is null when the user left it |
| `VOICE_SERVER_UPDATE` | A member who was moved to another voice channel (new `url`, `token` and `room`) |
| `VOICE_SPEAKING` | Everyone who can see the voice channel |

Permission changes such as roles, overwrites, joins and kicks take effect immediately: a
kicked member stops receiving the place's events. Ending a session (logout, revocation,
password change, account deletion, or refresh-token reuse) closes its gateway connection.
Sessions cannot be resumed; after reconnecting, clients refetch what they need with
`after=<last message ID>`.

| Close code | Meaning |
|---|---|
| `1001` | Server shutting down; reconnect |
| `4000` | Internal error |
| `4001` | Invalid frame, unknown op, or invalid status |
| `4002` | Sent something before `identify` |
| `4003` | Sent `identify` twice |
| `4004` | Authentication failed |
| `4007` | Events may have been missed (e.g. the server lost its Redis connection); reconnect |
| `4008` | Too many frames (over 120 per minute) or too slow to read events |
| `4009` | Missed heartbeats |
| `4010` | Session ended |

| Area | Endpoints |
|---|---|
| Instance | `GET/PATCH /instance`, `GET /permissions`, `GET /setup/status`, `POST /setup` |
| Auth | `POST /auth/register`, `/auth/login`, `/auth/refresh`, `/auth/logout` |
| Users | `GET/PATCH/DELETE /users/@me`, `POST /users/@me/password`, `GET /users/@me/sessions`, `DELETE /users/@me/sessions/{id}`, `GET /users/@me/places`, `GET /users/{username}` |
| Places | `GET/POST /places`, `GET/PATCH/DELETE /places/{place}`, `POST /places/{place}/join`, `/leave`, `/transfer`, `GET /places/{place}/permissions/@me` |
| Members | `GET /places/{place}/members?q=`, `GET/PATCH/DELETE /places/{place}/members/{userID}`, `PUT/DELETE …/members/{userID}/roles/{roleID}` |
| Bans | `GET /places/{place}/bans`, `PUT/DELETE /places/{place}/bans/{userID}` |
| Roles | `GET/POST /places/{place}/roles`, `PATCH/DELETE /places/{place}/roles/{roleID}` |
| Invites | `GET/POST /places/{place}/invites`, `DELETE /places/{place}/invites/{code}`, `GET/POST /invites/{code}` |
| Boards | `GET/POST /places/{place}/boards`, `GET/PATCH/DELETE /boards/{boardID}`, `GET /boards/{boardID}/overwrites`, `PUT/DELETE /boards/{boardID}/overwrites/{roleID}`, `PUT /boards/{boardID}/subscription` |
| Topics | `GET/POST /boards/{boardID}/topics`, `GET /places/{place}/topics`, `GET /places/{place}/tags`, `GET/PATCH/DELETE /topics/{topicID}`, `PUT/DELETE /topics/{topicID}/solution`, `PUT /topics/{topicID}/read`, `PUT /topics/{topicID}/subscription` |
| Posts | `GET/POST /topics/{topicID}/posts`, `GET/PATCH/DELETE /posts/{postID}`, `GET /posts/{postID}/revisions`, `GET/PUT/DELETE /posts/{postID}/reactions/{emoji}` |
| Search | `GET /search`, `GET /places/{place}/search` |
| Notifications | `GET /users/@me/notifications`, `GET …/notifications/unread-count`, `POST …/notifications/read-all`, `POST …/notifications/{id}/read`, `DELETE …/notifications/{id}`, `PUT /places/{place}/subscription` |
| Drafts | `GET /users/@me/drafts`, `GET/PUT/DELETE /users/@me/drafts/{key}` |
| Moderation | `GET/POST /places/{place}/reports`, `PATCH /places/{place}/reports/{reportID}`, `GET /places/{place}/audit-log`, `PUT/DELETE /places/{place}/members/{userID}/timeout`, `POST /places/{place}/members/{userID}/warnings` |
| Channels | `GET/POST /places/{place}/channels`, `GET/PATCH/DELETE /channels/{channelID}`, `GET /channels/{channelID}/overwrites`, `PUT/DELETE /channels/{channelID}/overwrites/{roleID}`, `GET/POST /channels/{channelID}/threads`, `PUT /channels/{channelID}/subscription`, `PUT /channels/{channelID}/read`, `POST /channels/{channelID}/typing` |
| Messages | `GET/POST /channels/{channelID}/messages`, `GET/PATCH/DELETE /messages/{messageID}`, `GET /messages/{messageID}/revisions`, `GET/PUT/DELETE /messages/{messageID}/reactions/{emoji}`, `GET /channels/{channelID}/pins`, `PUT/DELETE /channels/{channelID}/pins/{messageID}` |
| Direct messages | `GET/POST /users/@me/channels`, `PUT/DELETE /channels/{channelID}/recipients/{userID}`, `GET /channels/{channelID}/receipts` |
| Real time | `GET /presences?user_ids=…`, WebSocket `/gateway` |
| Voice | `POST /channels/{channelID}/voice`, `GET /channels/{channelID}/voice/sessions`, `GET/PATCH/DELETE /users/@me/voice`, `POST /users/@me/voice/telemetry`, `GET /places/{place}/voice-states`, `GET/PATCH/DELETE /places/{place}/members/{userID}/voice`, `POST /voice/webhook` (LiveKit only) |

**Errors** are [RFC 9457](https://www.rfc-editor.org/rfc/rfc9457) `application/problem+json`
documents. **Rate limits** are reported on every response via `X-RateLimit-Limit`,
`X-RateLimit-Remaining`, and `X-RateLimit-Reset`; exceeded limits return `429` with
`Retry-After`.

## Development

Requirements: Go 1.27+ and Docker (for integration tests and `sqlc`).

```sh
go test ./...                      # unit + integration tests (starts PostgreSQL/Redis containers)
golangci-lint run ./...            # lint, same config as CI
docker run --rm -v "$PWD:/src" -w /src sqlc/sqlc:1.30.0 generate   # after editing SQL
```

Integration tests skip automatically when Docker is unavailable. On Docker Desktop for
Windows, set `TESTCONTAINERS_RYUK_DISABLED=true` if container start-up times out; the
tests clean up their own containers.

To change the schema, add a new numbered file to
[internal/database/migrations](internal/database/migrations), add or edit queries in
[internal/store/queries](internal/store/queries), and regenerate with `sqlc`. CI fails if the
generated code is out of date.

### Layout

| Path | Contents |
|---|---|
| `cmd/gotalk` | CLI entry point: serve, migrate, setup, healthcheck |
| `internal/api` | HTTP layer: chi router, huma operations, middleware, DTOs, WebSocket gateway |
| `internal/service` | Business logic shared by the API and CLI (forum permissions are evaluated in `forum.go`, chat permissions in `channels.go`, voice state and LiveKit reconciliation in `voice.go`) |
| `internal/realtime` | Gateway event routing (hub), Redis or in-memory event broker, and presence store |
| `internal/livekit` | Minimal LiveKit client: participant tokens, RoomService calls, webhook verification |
| `internal/store` | sqlc-generated, type-safe queries (do not edit by hand) |
| `internal/database` | Connection handling and embedded goose migrations |
| `internal/permissions` | Permission bits, role hierarchy, and board and channel overwrite rules |
| `internal/auth` | Argon2id passwords, JWT access tokens, refresh tokens |
| `internal/config` | Layered configuration and validation |
| `internal/ratelimit` | Rate limit tiers backed by Redis or memory |
| `internal/web` | Embedded landing page and setup wizard |
