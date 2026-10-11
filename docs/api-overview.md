# API overview

[← Documentation](README.md)

Everything lives under `/api/v1`. The full, always-current contract is the OpenAPI document
at `/api/v1/openapi.json` (browse it at `/api/v1/docs`).

The API guide is split by topic:

| Page | Covers |
|---|---|
| **Overview** (this page) | Connecting a client, authentication, password reset and email verification, errors and rate limits |
| [Instance and uploads](api-instance.md) | Instance settings, avatar/icon uploads, attachments, link previews |
| [Places, roles and boards](api-places.md) | Places, roles and permissions, boards, reading without an account |
| [Forum](api-forum.md) | Topics and posts, read state, feeds, search, drafts |
| [Chat and messaging](api-chat.md) | Channels, messages, read state, direct messages, presence |
| [Voice](api-voice.md) | Voice channels and call quality |
| [Moderation and policies](api-moderation.md) | Reports, timeouts, bans, audit log, policies and consent, transparency |
| [Developers](api-developers.md) | API tokens, applications and bots, slash commands, webhooks |
| [Real-time gateway](gateway.md) | WebSocket protocol, events, close codes |
| [Endpoint reference](api-reference.md) | Every endpoint grouped by area |

## On this page

- [Connecting a client](#connecting-a-client)
- [Authentication](#authentication)
- [Password reset and email verification](#password-reset-and-email-verification)
- [Errors and rate limits](#errors-and-rate-limits)

## Connecting a client

Given only a domain, fetch `/.well-known/gotalk-instance` to find
the API base URL and gateway URL, then `GET /api/v1/instance` for the instance name,
registration mode, supported API versions (`min_version`/`max_version`), feature flags,
limits (message and post lengths, webhooks per place, …), webhook events, policy links and
published rate limits. Every API response carries a `Gotalk-Api-Version` header; clients may
send the version they were built for in the same header, and an instance that does not
support it answers `400` instead of failing call by call.

## Authentication

`POST /auth/register` or `/auth/login` returns a short-lived JWT access
token (send as `Authorization: Bearer <token>`) and a single-use refresh token. `POST /auth/refresh`
rotates both; presenting an already-used refresh token revokes the whole session. Sessions
can be listed and revoked under `/users/@me/sessions`.

For scripts and integrations use [personal access tokens](api-developers.md#api-tokens); bots
authenticate with their own [bot tokens](api-developers.md#applications-and-bots).

## Password reset and email verification

Available when email is configured (`features.password_reset`
and `features.email_verification` in `/instance`). `POST /auth/password-reset` with an email
address sends a link to `<instance>/reset-password?token=…` (valid for an hour, once), and
always answers `202` so it cannot reveal who has an account; repeated requests within a
minute send nothing more. That page (or any client) calls `POST /auth/password-reset/confirm`
with the token and new password, which signs out every session and revokes personal access
tokens. Registration sends a verification link (`<instance>/verify-email?token=…`, valid for 48
hours) that calls `POST /auth/verify-email`; `POST /users/@me/email/verification` sends a new
one. Links use `server.public_url` when set. `email_verified` is on `GET /users/@me`.

## Errors and rate limits

**Errors** are [RFC 9457](https://www.rfc-editor.org/rfc/rfc9457) `application/problem+json`
documents. **Rate limits** are reported on every response via `X-RateLimit-Limit`,
`X-RateLimit-Remaining`, `X-RateLimit-Reset` and `X-RateLimit-Tier`; exceeded limits return
`429` with `Retry-After`. `GET /rate-limits` shows the caller's standing in every tier without
using up a request. Limits apply per user (shared by their sessions and personal access tokens;
each bot is its own user) or per IP address when signed out. The tiers are configurable; see
[Rate limiting](configuration.md#rate-limiting).
