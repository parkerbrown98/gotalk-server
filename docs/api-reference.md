# Endpoint reference

[← API overview](api-overview.md) · [Documentation](README.md)

Every endpoint is under `/api/v1`, grouped by area. The always-current contract, with request
and response schemas, is the OpenAPI document at `/api/v1/openapi.json` (browse it at
`/api/v1/docs`).

## On this page

- [Endpoints by area](#endpoints-by-area)
- [Where to read more](#where-to-read-more)

## Endpoints by area

| Area | Endpoints |
|---|---|
| Instance | `GET/PATCH /instance`, `PUT/DELETE /instance/icon`, `GET/PATCH /instance/config`, `DELETE /instance/config/{section}`, `POST /instance/config/test`, `GET /instance/checks`, `GET /permissions`, `GET /setup/status`, `POST /setup`, `POST /setup/test` |
| Auth | `POST /auth/register`, `/auth/login`, `/auth/refresh`, `/auth/logout`, `/auth/password-reset`, `/auth/password-reset/confirm`, `/auth/verify-email` |
| Users | `GET/PATCH/DELETE /users/@me`, `PUT/DELETE /users/@me/avatar`, `POST /users/@me/password`, `POST /users/@me/email/verification`, `GET /users/@me/sessions`, `DELETE /users/@me/sessions/{id}`, `GET /users/@me/places`, `GET /users/{username}` |
| Places | `GET/POST /places`, `GET/PATCH/DELETE /places/{place}`, `PUT/DELETE /places/{place}/icon`, `/banner`, `POST /places/{place}/join`, `/leave`, `/transfer`, `GET /places/{place}/permissions/@me` |
| Members | `GET /places/{place}/members?q=`, `GET/PATCH/DELETE /places/{place}/members/{userID}`, `PUT/DELETE …/members/{userID}/roles/{roleID}` |
| Bans | `GET /places/{place}/bans`, `PUT/DELETE /places/{place}/bans/{userID}` |
| Roles | `GET/POST /places/{place}/roles`, `PATCH/DELETE /places/{place}/roles/{roleID}` |
| Invites | `GET/POST /places/{place}/invites`, `DELETE /places/{place}/invites/{code}`, `GET/POST /invites/{code}` |
| Boards | `GET/POST /places/{place}/boards`, `GET/PATCH/DELETE /boards/{boardID}`, `GET /boards/{boardID}/overwrites`, `PUT/DELETE /boards/{boardID}/overwrites/{roleID}`, `PUT /boards/{boardID}/subscription` |
| Topics | `GET/POST /boards/{boardID}/topics`, `GET /places/{place}/topics`, `GET /places/{place}/tags`, `GET/PATCH/DELETE /topics/{topicID}`, `PUT/DELETE /topics/{topicID}/solution`, `PUT/DELETE /topics/{topicID}/read`, `PUT/DELETE /topics/{topicID}/vote`, `PUT /topics/{topicID}/subscription` |
| Feeds | `GET /places/{place}/feed`, `GET /feed`, `POST /places/{place}/feed/read`, `POST /feed/read` |
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
| Developer | `GET/POST /users/@me/tokens`, `DELETE /users/@me/tokens/{tokenID}`, `GET /rate-limits`, `GET/POST /applications`, `GET/PATCH/DELETE /applications/{applicationID}`, `POST /applications/{applicationID}/bot/token`, `GET/PUT /applications/{applicationID}/commands`, `POST /places/{place}/bots`, `GET /channels/{channelID}/commands`, `POST /channels/{channelID}/interactions` |
| Webhooks | `GET/POST /places/{place}/webhooks`, `GET/PATCH/DELETE /webhooks/{webhookID}`, `POST /webhooks/{webhookID}/secret`, `POST /webhooks/{webhookID}/ping`, `GET /webhooks/{webhookID}/deliveries`, `POST /webhooks/{webhookID}/deliveries/{deliveryID}/redeliver` |
| Policies | `GET /policies`, `GET/POST /policies/{kind}`, `GET /policies/{kind}/versions`, `GET /policies/{kind}/versions/{version}`, `GET/POST /users/@me/consents`, `GET /users/@me/consents/history`, `GET /transparency`, `GET /places/{place}/transparency` |

## Where to read more

| Area | Guide |
|---|---|
| Instance, Auth, Users | [API overview](api-overview.md), [Instance and uploads](api-instance.md) |
| Places, Members, Bans, Roles, Invites, Boards | [Places, roles and boards](api-places.md) |
| Topics, Feeds, Posts, Search, Notifications, Drafts | [Forum](api-forum.md) |
| Channels, Messages, Direct messages, Real time | [Chat and messaging](api-chat.md), [Real-time gateway](gateway.md) |
| Voice | [Voice](api-voice.md) |
| Moderation, Policies | [Moderation and policies](api-moderation.md) |
| Developer, Webhooks | [Developers](api-developers.md) |
