# Developers

[← API overview](api-overview.md) · [Documentation](README.md)

## On this page

- [API tokens](#api-tokens)
- [Applications and bots](#applications-and-bots)
- [Webhooks](#webhooks)

## API tokens

For scripts and integrations, `POST /users/@me/tokens` creates a personal
access token (`gtp_…`, shown once, stored only as a hash) with scopes: `read` allows `GET`
requests, `write` everything else, `gateway` the real-time connection, and `admin` keeps an
instance administrator's powers (without it, an administrator's token acts as a regular
user). Tokens may expire after up to 366 days and are sent like access tokens. They cannot
manage credentials: logging out, sessions, password changes, account deletion, other tokens,
creating or deleting applications, and recording consent all require a login session.
Revoking a token closes its gateway connections, and changing the password revokes every
personal access token.

## Applications and bots

`POST /applications` registers an application together with a bot
account (`"bot": true` on its user) and returns the bot's token (`gtb_…`, shown once; reset it
with `POST /applications/{id}/bot/token`). Bots authenticate with `Bearer` or `Bot <token>`,
cannot log in with a password, create or own places, join places on their own, or use voice.
Someone with `MANAGE_PLACE` adds a bot with `POST /places/{place}/bots`; private applications
can only be added by their owner, public ones by anyone. Bots then hold `@everyone` like any new
member, and their moderation actions are attributed to the application in the audit log
(`metadata.via` is `bot` or `api_token`). Applications define slash commands with
`PUT /applications/{id}/commands` (owner or the bot itself; up to 50, each with typed options).
`GET /channels/{id}/commands` lists the commands of bots that can see a channel, and
`POST /channels/{id}/interactions` invokes one (`SEND_MESSAGES`). Options are validated against
the command, and the bot receives an `INTERACTION_CREATE` [gateway](gateway.md) event; it answers by sending
an ordinary message. Interactions are not stored, so offline bots miss them. Deleting an
application removes its bot from every place; its messages stay.

## Webhooks

Members with `MANAGE_WEBHOOKS` can create up to 10 outgoing webhooks per place
(`/places/{place}/webhooks`). Each subscribes to events: `member.join`, `member.leave`,
`topic.create`, `post.create`, `message.create`, `message.update` (edits), `message.delete`,
`moderation.action` (every audit log entry; needs `VIEW_AUDIT_LOG`) and `report.create`
(needs `MANAGE_REPORTS`). Managing a webhook, including listing it and reading its deliveries,
also requires the permissions its events need. Content events only cover boards and channels that `@everyone` can
see; staff-only channels and direct messages never reach webhooks. Gotalk POSTs JSON
`{"id", "type", "webhook_id", "place_id", "created_at", "data"}`, where `data` uses the REST
API's shapes, with headers `Gotalk-Event`, `Gotalk-Webhook-Id`, `Gotalk-Delivery-Id` (use it
to deduplicate), `Gotalk-Delivery-Attempt` and `Gotalk-Signature: t=<unix>,v1=<hex>`. The
signature is the HMAC-SHA256 of `<t>.<body>` keyed with the webhook's secret (returned on
creation and by `POST /webhooks/{id}/secret`); reject stale timestamps. Any `2xx` counts as
delivered. Redirects are not followed, and failures are retried after 30 seconds, 2 minutes,
10 minutes, 1 hour and 6 hours. After 10 deliveries in a row fail every attempt, the webhook
is disabled (and audited). Deliveries are written in the same transaction as the event, so
none are lost or sent for rolled-back changes. Any replica can send them, in no particular
order. `GET /webhooks/{id}/deliveries` shows each delivery's attempts, status code, error and
payload; `POST …/redeliver` and `POST /webhooks/{id}/ping` queue new ones. Webhooks cannot
reach loopback, private, link-local or other non-public addresses (checked after DNS
resolution) unless `webhooks.allow_private_networks` is set (see
[Webhooks configuration](configuration.md#webhooks)).
