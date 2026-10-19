# Push notifications

[← Documentation](README.md)

The server sends notifications to phones and browsers through a **Gotalk push gateway**
([gotalk-gateway](https://github.com/parkerbrown98/gotalk-gateway)). APNs, FCM and browser push services only accept
pushes signed with the app publisher's credentials, so instances hand pushes to the publisher's gateway, which
forwards them. The notification text is encrypted on the server to each device's key: the gateway and the push
providers never see it.

## On this page

- [For operators](#for-operators)
- [For client developers](#for-client-developers)
- [What becomes a push](#what-becomes-a-push)
- [Failures and retries](#failures-and-retries)
- [Testing](#testing)

## For operators

### Turning it on

Push is off by default. Turn it on in the setup wizard (the optional *Push notifications* step), in the instance
settings at `/setup`, with `PATCH /instance/config` (section `push`), or in the config file
([configuration](configuration.md#push-notifications)). Setting `push.enabled` in the config file or environment
makes the section read-only in the browser.

Requirements:

- `server.public_url` is the instance's public `https://` address. The gateway identifies the instance by that origin.
- The gateway can reach `{server.public_url}/.well-known/gotalk-instance`, which lists the instance's push key while
  push is on.
- The server can reach the gateway (`push.gateway_url`, default `https://push.gotalk.sh`, the official gateway that the
  official apps use). Clients built with other credentials need their own gateway.

### Registration

Once push is on, the server registers with the gateway (`POST /v1/instances`, with `push.contact_email` or the
oldest administrator's address as the contact) and asks it to verify the instance. The gateway fetches the
well-known document and activates the key it finds there. Pushes wait in the queue until the gateway reports the
instance `active`.

`GET /instance/push` shows the registration (administrators):

| `status` | Meaning |
|---|---|
| `unregistered` | Not registered with the configured gateway yet. |
| `pending` | Registered; the gateway has not verified the instance. `detail` says why (usually that it could not fetch the well-known document). |
| `active` | Verified. Devices can register and pushes are delivered. |
| `suspended` | The gateway's daily re-verification failed for three days, or it reports the instance unverified. |
| `blocked` | The gateway operator blocked the instance; `detail` is their reason. |
| `error` | Something is wrong that a retry alone will not fix, such as a missing `server.public_url` or a registration the gateway refused. A gateway that is briefly unreachable or throttling leaves the status as it was. |

One replica re-checks the status every 6 hours (every 5 minutes while it is not `active`). Changing the push
settings, or `POST /instance/push/verify`, syncs right away. Every status change is recorded in the instance audit
log (`push.status`, with the old and new status and the gateway's reason), and the pre-flight checks
(`GET /instance/checks`, `gotalk check`) report it. While push is on but not `active`, `GET /instance` lists `push`
in `degraded_features`.

### The signing key

Requests to the gateway are signed with an Ed25519 key (GOTALK-SIG-V1). Unless `push.signing_key` is set, the server
generates one and stores it in the database, so every replica uses the same key. Protect database backups
accordingly.

To replace a leaked generated key, call `POST /instance/push/keys/rotate`: the server publishes the new key and
registers it, and the gateway revokes the old one once it verifies the new one (its recovery flow). A configured
key is rotated by changing `push.signing_key`. Pushes pause for the few seconds the gateway needs to verify the new
key.

### Changing gateways

Subscriptions belong to the gateway the device registered with. When `push.gateway_url` changes, the server registers
with the new gateway, and devices registered with the old one stop receiving pushes until the client registers
again with the new gateway (clients do this when they see a new `push.gateway_url` in `GET /instance`).

## For client developers

### Discovery

`GET /instance` returns a `push` object:

```json
{
  "enabled": true,
  "available": true,
  "gateway_url": "https://push.gotalk.sh",
  "modes": ["encrypted", "id_only"],
  "categories": ["mentions", "replies", "direct_messages", "reactions", "new_topics", "solutions", "moderation"],
  "web_apps": [{ "app_id": "gotalk", "vapid_public_key": "BEl62iUY…" }]
}
```

Register only when `available` is true, and only with `gateway_url` (or a gateway the person chose for a custom
build). `features.push` mirrors `available`. `web_apps` carries the VAPID key for `pushManager.subscribe()`; native
clients read the app list from the gateway's own discovery document. `/.well-known/gotalk-instance` also has a
`push` object with the instance's public keys and `gateway_url`.

### Registering a device

1. Register the platform token (or browser endpoint) with the gateway for this instance's origin:
   `POST {gateway_url}/v1/devices` (see the gateway's
   [protocol](https://github.com/parkerbrown98/gotalk-gateway/blob/main/docs/protocol.md#device-registration)).
   Keep its `subscription_secret` on the device; the instance never needs it.
2. Generate a P-256 key pair and a 16-byte auth secret for this account (browsers create them as part of the push
   subscription), and register them with the instance:

```http
POST /api/v1/users/@me/push-subscriptions
Authorization: Bearer <session access token>

{
  "subscription_id": "sub_3kP…",
  "gateway_url": "https://push.gotalk.sh",
  "app_id": "gotalk",
  "platform": "ios",
  "keys": { "p256dh": "<65-byte uncompressed public key, base64url>", "auth": "<16 bytes, base64url>" },
  "mode": "encrypted",
  "device_name": "Alice's iPhone"
}
```

- The subscription belongs to the **login session** that adds it: signing out, or being signed out remotely, stops
  pushes to that device. API tokens cannot add subscriptions.
- Registering the same `subscription_id` again replaces its keys, mode and session.
- Several accounts on one device share the device's gateway subscription; each adds it with its own keys, and the
  device tries its keys on each push (the `aes128gcm` tag tells which fits).
- `mode: id_only` pushes carry no content, only the notification ID for the device to fetch. Browsers always use
  `encrypted`.
- An account can have up to 25 subscriptions.

`GET /users/@me/push-subscriptions` lists the account's devices (`current` marks the calling session's), and
`DELETE /users/@me/push-subscriptions/{id}` removes one. When no other account on the instance uses that gateway
subscription, the server also deletes it at the gateway. Clients should still delete their gateway subscription
themselves when they unregister.

### Preferences

`GET/PUT /users/@me/push-preferences`:

```json
{
  "categories": { "mentions": true, "replies": true, "direct_messages": true, "reactions": false,
                  "new_topics": true, "solutions": true, "moderation": true },
  "quiet_hours": { "start": "22:00", "end": "07:00", "timezone": "Europe/Berlin" },
  "paused_until": "2026-10-20T08:00:00Z"
}
```

`PUT` replaces the preferences: categories left out are on, leaving out `quiet_hours` removes them, and
`paused_until: null` ends a pause. A quiet-hours window whose start is after its end runs past midnight.

Mutes on places, boards, topics, channels and conversations apply first: muted activity creates no notification at
all, so it is never pushed.

### Payload

An encrypted push decrypts (RFC 8291, `aes128gcm`) to the plaintext defined in the gateway's
[payload format](https://github.com/parkerbrown98/gotalk-gateway/blob/main/docs/payload.md#plaintext-schema):

```json
{
  "v": 1,
  "instance": "https://forum.example",
  "user_id": "…",
  "notification_id": "…",
  "type": "mention",
  "title": "Alice mentioned you in Launch day",
  "body": "Ready, @bob? The doors open at nine.",
  "thread_id": "<topic or channel ID>",
  "category": "mentions",
  "badge": 3,
  "created_at": "2026-10-19T16:46:00Z",
  "target": { "place_id": "…", "topic_id": "…", "post_id": "…" }
}
```

`title` and `body` are in English; clients may render their own text from `type` and the fetched notification
instead. Always show the instance name with the notification. Direct messages carry
`target.conversation_id`. `badge` is the account's unread notification count. The plaintext is padded to a multiple
of 256 bytes (at most 2,048), and `body` is shortened to fit.

## What becomes a push

Every notification the server creates (see [Forum](api-forum.md) and [Chat](api-chat.md)) is queued for a push when
its recipient has at least one subscription. Delivery then skips it when:

- it was read or dismissed before delivery, or is more than 6 hours old;
- its category is turned off, the account is paused or in quiet hours, or the person's presence is **do not
  disturb** (a client set it, and no other client of theirs is online);
- the account has no subscription on the current gateway from a signed-in session.

| Type | Category | Urgency | TTL |
|---|---|---|---|
| `mention`, `direct_message` | `mentions`, `direct_messages` | `high` | 24 h |
| `reply`, `topic_reply`, `moderation` | `replies`, `moderation` | `normal` | 24 h |
| `reaction`, `new_topic`, `solution` | `reactions`, `new_topics`, `solutions` | `low` | 6 h |

Pushes are grouped per conversation or topic with a collapse key (an HMAC of the channel or topic ID under the
instance's key, so providers cannot link it to public IDs): a newer push replaces an undelivered older one. A
direct-message conversation has one unread notification at a time, so it pushes once until it is read.

## Failures and retries

Pushes go out in batches of up to 100. Per push:

- `subscription_gone` or `subscription_not_found`: the device's token is dead or the subscription is not bound to
  this instance. The server deletes every account's subscription for it.
- `rate_limited`, `quota_exceeded`, `provider_unavailable`: retried after 10 s, 30 s, 2 min, 5 min and 15 min (or
  the gateway's `Retry-After`), at most 6 attempts and never after 6 hours.
- Other rejections are logged and dropped.

When the gateway rejects the instance itself (`instance_not_verified`, `instance_blocked`), the status changes to
`suspended` or `blocked`, the change is audited, and pushes wait. A rejected key (`unknown_key`,
`invalid_signature`) makes the server register again. Every replica drains the queue; row locks keep a push from
going out twice.

## Testing

`internal/api/push_test.go` runs the whole flow against a fake gateway that verifies signatures and the well-known
document, and decrypts each push with the device's key. `internal/pushgw` checks signing against the gateway
protocol's Appendix A vector and encryption against RFC 8291 Appendix A.

`internal/api/pushgateway_test.go` (build tag `pushgateway`) runs the real gateway container and checks that a
notification becomes a push it accepts:

```sh
go test -tags pushgateway -run TestRealPushGateway ./internal/api
```

It uses `GOTALK_GATEWAY_IMAGE` (default `ghcr.io/parkerbrown98/gotalk-gateway:edge`); CI builds the image from the
gateway's main branch.
