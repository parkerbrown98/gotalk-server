# Public index

[← API overview](api-overview.md) · [Documentation](README.md)

A Gotalk instance can list itself in the public Gotalk directory at
[gotalk.sh/explore](https://gotalk.sh/explore). The index then shows the instance's public forum boards in
search engines and the directory. This page covers the instance side. The full contract between instances
and the index is the
[index protocol](https://github.com/parkerbrown98/gotalk-platform/blob/main/docs/index-protocol.md), and its
JSON Schemas live in the platform repo's `packages/index-protocol`.

## On this page

- [What gets indexed](#what-gets-indexed)
- [Opting in](#opting-in)
- [Discovery](#discovery)
- [Index endpoints](#index-endpoints)
- [Change feed](#change-feed)
- [Pings](#pings)
- [Notices and policy text](#notices-and-policy-text)

## What gets indexed

Nothing is indexed by default. Content reaches the index only when every level agrees:

| Level | Condition |
|---|---|
| Instance | An administrator turned directory listing on |
| Place | `visibility` is `public`, the place is not NSFW, and its owners set `indexable` (off by default) |
| Board | `@everyone` can read it and every board above it, neither it nor any board above it is NSFW, and none of them set `indexable: false` |
| Topic | In an indexed board, not deleted, and its opening post is visible |
| Post | Not deleted, and its author has not excluded their posts or deleted their account |

`invite_only` and `private` places and NSFW places and boards are never indexed, whatever their flags say.
Chat, direct messages, attachments, reactions, revisions and member lists are never exposed. Authors appear as
`id`, `username` and `display_name` only, never with an email address, IP address or avatar.

Everything the index endpoints return can also be read by a signed-out visitor through the regular API, as
described in [Reading without an account](api-places.md#reading-without-an-account). The rules live in one
place in the server (`internal/service/index.go` and the `Index*` queries), so other code paths cannot return
unfiltered data.

## Opting in

| Setting | Where | Default | Who |
|---|---|---|---|
| Directory listing | `PATCH /instance/config` with a `directory` section, or `directory.*` in [configuration](configuration.md#public-directory) | off | Administrators |
| Place `indexable` | `PATCH /places/{place}` | off | `MANAGE_PLACE` |
| Board `indexable` | `PATCH /boards/{boardID}` | on | `MANAGE_BOARDS` |
| `exclude_from_index` | `PATCH /users/@me` | off | Each person |

Setting `indexable: true` on an NSFW place or board answers `422`. Places and boards report `indexable` (the
owners' choice), `indexed` (in the index right now) and `index_notice`, a plain-language explanation for
clients to show. The account (`GET /users/@me`) carries `exclude_from_index` and its own `index_notice`.

The `directory` section works like the other [instance settings](api-instance.md#instance-settings-administrators).
It holds `listed`, `contact_email`, `platform_url`, `pings_disabled` and a write-only `signing_key`. Saving runs a
live check of the instance's own public URL (`server.public_url`): it must use HTTPS and answer
`/.well-known/gotalk-instance`. The check never contacts the index. Administrators can also use these endpoints,
which need a login session:

- `GET /instance/directory`: listing status, published keys, the number of indexed places, the last ping, the
  notice, suggested policy text, and links to the index's status page for this instance and the operator guide.
- `POST /instance/directory/keys/rotate`: replaces the generated signing key. The old public key stays published
  so pings signed just before the switch still verify.
- `DELETE /instance/directory/keys/previous`: stops publishing the old key.
- `GET /instance/audit-log`: instance-wide audit entries, including listing changes, key rotation and people
  excluding their posts. Place and board changes are in each place's audit log.

## Discovery

While listing is on, `/.well-known/gotalk-instance` carries a `directory` block. The index treats an instance as
listed only while this block says `listed: true`, so turning listing off removes the instance within the index's
15-minute re-check.

```json
"directory": {
  "listed": true,
  "protocol": 1,
  "index_url": "https://forum.example/api/v1/index",
  "contact_email": "ops@forum.example",
  "keys": [{ "alg": "Ed25519", "public_key": "A6EHv_POEL4dcN0Y50vAmWfk1jCbpQ1fHdyGZBJVMbg" }]
}
```

`GET /instance` adds `directory: { listed, places_indexed }` for clients.

## Index endpoints

All are `GET`, unauthenticated, under `/api/v1/index`, and listed in the OpenAPI document under the **Index** tag.
They use their own `index` [rate-limit tier](configuration.md#rate-limiting), always counted per IP and never
against user tiers. Responses carry `Cache-Control: no-cache` and a strong `ETag`; send `If-None-Match` to get
`304 Not Modified` when nothing changed. Anything missing or not indexable answers `404` with no hint about which.

| Endpoint | Returns |
|---|---|
| `GET /index/places` | Indexable places with their indexable boards (and the categories that group them), ordered by ID. Empty while listing is off |
| `GET /index/boards/{boardID}/topics` | A board's indexable topics, ordered by `updated_at`, then ID. `updated_since` filters by time. Categories return no topics |
| `GET /index/topics/{topicID}` | A topic summary and a page of its indexable posts, with Markdown `content`, in post-number order |
| `GET /index/changes` | The [change feed](#change-feed) |

Pages hold up to `limit` items (default 100, at most 500; topic posts default to 50, at most 100). Pass
`next_cursor` back as `cursor`; it is `null` on the last page. A topic's `updated_at` moves whenever something the
index shows changes: an edit, a reply, a removal, a move, or an author changing their display name.
`reply_count` counts indexable replies only, and posts left out of the index leave gaps in `number`.

Errors use problem details with a `type` of `https://gotalk.sh/problems/{code}` and the same `code`:
`not_found`, `cursor_expired` (`410`), `rate_limited` (`429` with `Retry-After`), `invalid_cursor` (`400`, a
cursor this instance did not issue) and `invalid_parameter` (`400`, for example a malformed `updated_since`).

## Change feed

`GET /index/changes?cursor=…&limit=…` returns an ordered list of events that name changed objects, never
content:

| Event | Fields | Emitted when |
|---|---|---|
| `place.upsert` | `place_id` | A place becomes indexable (opted in, made public, NSFW turned off, listing turned on) or its name, slug, description, locale or icon change |
| `place.delete` | `place_id` | A place stops being indexable: opted out, made `invite_only` or `private`, marked NSFW, deleted, or listing turned off |
| `board.upsert` | `place_id`, `board_id` | A board becomes indexable (created, opted in, NSFW turned off, readable by `@everyone` again) or is renamed, moved or reordered |
| `board.delete` | `place_id`, `board_id` | A board stops being indexable: opted out, NSFW, deleted, or hidden from `@everyone` by an overwrite or a role change |
| `topic.upsert` | `place_id`, `board_id`, `topic_id` | A topic is created, edited, replied to, solved or unsolved, locked, archived, pinned or moved; a post is edited or deleted |
| `topic.delete` | `place_id`, `board_id`, `topic_id` | A topic is deleted, moved out of indexed boards, or its author excludes their posts |
| `user.exclude` | `user_id` | Someone with indexed posts excludes their posts or deletes their account |
| `user.include` | `user_id` | Someone includes their posts again. Each affected topic also gets `topic.upsert` |

Permission changes that hide or reveal boards emit one event per affected board, in the same transaction as the
change. Events are only emitted for objects that are, or were, in the index.

- Omitting `cursor` starts at the oldest retained event. `cursor=latest` returns no events and the current end of
  the feed, so a reader can resume after a full reconcile.
- `seq` strictly increases. Several events for one object are coalesced into the latest, so readers must treat
  each event as "read the current state".
- Events are kept for 30 days. A cursor older than that answers `410 cursor_expired`; run a reconcile and resume
  from `cursor=latest`.

Events are recorded in the same transaction as the change and numbered when the feed is read, in commit order,
so a reader never skips an event that commits late.

## Pings

After changes, the instance tells the index to read the feed early with a signed
`POST {directory.platform_url}/api/index/v1/ping` and the body `{"origin": "<server.public_url>"}`. Pings are hints:
they carry no content, go out at most once every 30 seconds across all replicas, are retried a few times on
failure, and never block a write. They need `server.public_url`, and stop when listing is off or
`directory.pings_disabled` is set.

Pings are signed with Ed25519 as `GOTALK-INDEX-SIG-V1`, which matches the push gateway's request signing but uses
its own domain separator, with the public key as `Gotalk-Key-Id`. The key is generated and stored on first use
unless `directory.signing_key` is set. The same seed as the push gateway key may be reused.

## Notices and policy text

The clause in an instance's terms that lets the index show excerpts of public posts is pending legal review.
Until it is written, the server claims no license: `GET /instance/directory` returns the notice to show next to
the listing switch and `license_pending: true`, and `policy_text` offers suggested sections for the terms, privacy
policy and guidelines, with the license clause as a placeholder. Administrators publish policy text themselves
through [policies](api-moderation.md).
