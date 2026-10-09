# Gotalk — Backend Feature Plan

This document breaks down the backend functionality required to support the product vision described in
[README.md](../README.md): a forum-first, search-engine-friendly, self-hostable alternative to Discord with
voice, rich text, and strong moderation/role tooling. The backend is implemented in **Go**, and any client
(official web/mobile/desktop app or third-party) is expected to be able to point itself at any compatible
instance of this server — there is no single "canonical" host.

It is organized by domain area. Each area lists the core features, the data/entities it implies, and notable
edge cases or non-functional concerns the backend needs to account for.

## Technology Stack (Go)

Concrete library/framework choices, picked to stay close to the standard library, keep the deployment story
to "a single static binary + Postgres (+ optional Redis)", and favor tools that are themselves open source.

| Concern | Choice | Notes |
|---|---|---|
| HTTP routing/middleware | [`chi`](https://github.com/go-chi/chi) | Stdlib `net/http`-compatible, composable middleware, plays well with `context` and OpenTelemetry. |
| REST + OpenAPI | [`huma`](https://github.com/danielgtaylor/huma) on top of `chi` | Declarative request/response structs generate OpenAPI 3.1 + validation for free; used for the public/developer API. |
| Database driver | [`pgx`](https://github.com/jackc/pgx) (+ `pgxpool`) | Native Postgres driver/connection pool, no `database/sql` overhead. |
| Query layer | [`sqlc`](https://github.com/sqlc-dev/sqlc) | Generates type-safe Go from hand-written SQL; keeps query plans/indexes easy to reason about. `ent` is an acceptable alternative if graph-style schema evolution and built-in privacy rules are preferred over raw SQL. |
| Migrations | [`goose`](https://github.com/pressly/goose) or [`atlas`](https://atlasgo.io/) | Versioned, reversible schema migrations, run as part of boot/CI. |
| Cache / presence / rate limits | [`go-redis`](https://github.com/redis/go-redis) | Sessions, presence, pub/sub fan-out for the realtime gateway, rate-limit counters. |
| Background jobs & queues | [`river`](https://riverqueue.com/) (Postgres-backed) | Email, search indexing, media processing, digests — no extra infra beyond Postgres; `asynq` (Redis-backed) is a fallback if job volume outgrows Postgres. |
| Realtime gateway (chat/presence) | [`coder/websocket`](https://github.com/coder/websocket) (the maintained successor of `nhooyr.io/websocket`) | Minimal, context-aware WebSocket implementation; paired with Redis pub/sub for multi-node fan-out. |
| Voice/video SFU | [LiveKit](https://github.com/livekit/livekit) (self-hosted, Go-based, Apache-2.0) | Backend issues room/access tokens and manages participants through LiveKit's RoomService API; LiveKit itself handles WebRTC/SFU media routing. Implemented as a small internal client (`internal/livekit`, stdlib + `golang-jwt`) rather than [`livekit-server-sdk-go`](https://github.com/livekit/server-sdk-go), whose root package also pulls in a full WebRTC client stack and would roughly double the binary. `pion/webrtc` remains an option for a fully bespoke SFU later. |
| Full-text search | Postgres `tsvector`/FTS by default; pluggable [Meilisearch](https://www.meilisearch.com/) via `meilisearch-go` | Keeps the default deployment dependency-free; operators can opt into Meilisearch for typo-tolerant/faster search at scale. |
| AuthN/AuthZ primitives | [`golang-jwt/jwt`](https://github.com/golang-jwt/jwt), `golang.org/x/crypto/argon2` | JWT access/refresh tokens, Argon2id password hashing. |
| Request validation | [`go-playground/validator`](https://github.com/go-playground/validator) | Struct-tag validation, used underneath `huma` handlers. |
| Rate limiting | [`ulule/limiter`](https://github.com/ulule/limiter) (Redis store) | Distributed, multi-node-safe rate limiting with standard `X-RateLimit-*` headers. |
| Config | [`koanf`](https://github.com/knadh/koanf) | Layered config (defaults → config file → env vars → flags), needed for the first-run setup wizard. |
| Logging | stdlib `log/slog` | Structured JSON logging, no extra dependency. |
| Metrics / tracing | [`prometheus/client_golang`](https://github.com/prometheus/client_golang), [OpenTelemetry Go SDK](https://github.com/open-telemetry/opentelemetry-go) | `/metrics` endpoint + distributed tracing across the API, gateway, and job workers. |
| Testing | stdlib `testing` + `testify` + [`testcontainers-go`](https://github.com/testcontainers/testcontainers-go) | Integration tests spin up real Postgres/Redis containers in CI. |
| Media processing | [`libvips`](https://www.libvips.org/) via [`bimg`](https://github.com/h2non/bimg) | Thumbnailing/resizing; `dutchcoders/go-clamd` for optional ClamAV virus scanning. |
| Build/deploy | `CGO_ENABLED=0` static binary, multi-stage Docker build | Ships as one binary + a Docker image; Postgres/Redis/LiveKit run as sidecar containers via Compose or Helm. |

## Multi-Instance Architecture — Bring Your Own Server

Rather than a single "self-hosted, single-place" mode baked into the server, **any client can be pointed at
any instance's API base URL**, the same way Mastodon or Matrix clients work. The server itself doesn't
assume it's the only instance in the world, and it doesn't need to federate with others to make this work —
it just needs to describe itself well enough that a generic client can configure against it.

- **Configurable upstream on the client, not the server.** Official and third-party clients store a
  user-chosen API base URL (plus whichever credentials/session belong to that host). The backend has no
  concept of "the" server — every deployment is just an instance.
- **Public instance metadata endpoint** (e.g. `GET /api/v1/instance`): name, icon, description, software
  version/build, supported API version range, enabled feature flags (voice, search backend in use, etc.),
  links to policy documents (ToS/Privacy/Guidelines), registration mode (open/invite-only/closed), and
  published rate-limit tiers. This is what a client reads immediately after the user types in a host name,
  to decide how to render itself and whether it's compatible.
- **`.well-known` discovery** (e.g. `/.well-known/gotalk-instance`) so a client only needs a bare domain
  (`myforum.example`) to resolve the actual API base URL, similar to Mastodon's `/.well-known/webfinger` /
  `nodeinfo` pattern.
- **API version negotiation.** Requests/responses carry an API version (header or URL-prefixed, e.g. `/api/v1/`);
  the instance endpoint advertises the min/max version it supports so clients can detect incompatibility
  up front instead of failing on individual calls.
- **CORS is operator-configurable, not hardcoded.** Since arbitrary client origins (web apps hosted anywhere)
  need to call an arbitrary instance, allowed origins, credentials mode, and exposed headers must be part of
  instance configuration rather than a fixed allow-list.
- **No mandatory federation.** Each instance is fully self-sufficient with its own users/Places/data. Instances
  are not required to talk to each other. An opt-in, instance-maintained "known instances" list (for directory
  discovery UIs) can be layered on later, but it is not a prerequisite for the client-chooses-its-server model.
- **Per-instance isolation.** Sessions, API keys, and rate-limit buckets are always scoped to the instance
  that issued them; nothing in the auth/session model assumes a shared identity across instances.

**Entities:** `InstanceInfo` (effectively a singleton/config-derived resource, not a DB table)

## 1. Identity & Accounts

- User registration (email/password, with optional email verification flow)
- OAuth / SSO login providers (Google, GitHub, Discord-import, generic OIDC)
- Session management via JWT or opaque tokens + refresh tokens
- Multi-factor authentication (TOTP, recovery codes)
- Password reset / account recovery flows
- Account deactivation, soft-delete, and GDPR-style data export/erasure
- Per-user profile: display name, avatar, bio, pronouns, badges
- Username history / reserved name handling
- Device & active session management (view/revoke logged-in sessions)
- Rate-limited login attempts and basic bot/abuse detection (captcha hooks)

**Entities:** `User`, `Credential`, `Session`, `OAuthIdentity`, `MfaFactor`, `UserProfile`

## 2. Spaces ("Places") & Communities

- Create/configure a "Place" (community/server equivalent): name, slug, description, icon/banner
- Public vs. private vs. invite-only Places
- No special "single-place mode" — an instance is always multi-Place-capable; an operator who only wants
  one community simply never creates a second Place (see **Multi-Instance Architecture** above)
- Place discovery directory with search, tags/categories, and featured listings
- Membership management: join, leave, ban, invite links (with expiry/usage limits)
- Per-Place settings: locale, NSFW flag, verification requirements to join/post
- Place-level analytics (member growth, activity, retention)
- Place templates / cloning for quick setup

**Entities:** `Place`, `PlaceMembership`, `Invite`, `PlaceSettings`

## 3. Forum Structure (Boards, Topics, Posts)

- Hierarchical categories/boards within a Place
- Topics (threads) with title, tags, pinned/locked/archived states
- Posts within topics with rich text (Markdown + WYSIWYG-compatible AST) content
- Post revisions/edit history with diffing
- Nested/threaded replies vs. flat chronological reply modes (configurable per board)
- Reactions (emoji/likes) on posts and topics
- Best-answer / solution marking for Q&A-style boards
- Draft saving and scheduled publishing
- Topic subscriptions / watch & mute
- Read/unread tracking per user per topic, including whether the person has opened a topic at all
- Topic feeds (Reddit-style) per Place and instance-wide, with votes and several sort orders (see the
  *Topic feeds* subsection below)
- Full post quoting and mentions (@user, #topic, board links)
- Attachments (images, files) with size/type restrictions per Place
- Soft-delete with tombstones (preserve thread structure when a post is removed)

**Entities:** `Board`, `Topic`, `Post`, `PostRevision`, `Reaction`, `Attachment`, `ReadState`, `Subscription`, `TopicVote`, `TopicFeedStats`, `TopicOpen`

### Topic feeds

A feed is a ranked, cursor-paged list of topics, shown Reddit-style: title, excerpt, board, author, score,
reply count, tags and state badges. Two scopes share one implementation:

- **Place feed** (`GET /places/{place}/feed`): topics from every board the caller can read, optionally
  narrowed to one or more boards or tags.
- **Instance feed** (`GET /feed`): `scope=home` (default when signed in) merges the places the caller has
  joined; `scope=all` covers public content across the instance and is the only scope for signed-out
  callers. Each item carries its place so clients can label it.

**Sort orders** (`sort=`):

| Sort | Meaning |
|---|---|
| `hot` (default) | score and replies weighed against age, so fresh discussion beats stale popularity |
| `new` | newest topic first |
| `active` | latest reply first (classic forum order) |
| `top` | highest score, with a window `t=hour\|day\|week\|month\|year\|all` |
| `rising` | recent topics gaining votes and replies fastest (last 48 hours) |
| `controversial` | many votes that are split between up and down, with the same window as `top` |

Filters: `board`, `tag`, `solved` / `unsolved`, `hide_read`, `nsfw` (excluded by default) and
`include_archived`. Pinned topics can be returned first on a place feed (`pinned=first`).

**Voting:** each person has one up or down vote per topic (changeable and removable). Own topics cannot be
voted on. Score is `up − down`. A place setting turns voting
off, in which case scores fall back to the existing reaction counts and `controversial` is unavailable.

**Read tracking:** a topic is *read* once the person has opened it, which is different from the existing
`ReadState` position (how far through the posts they got). Feed items carry a `viewer` object with `read`,
`has_new_replies` (replies posted after the last open), `unread_count`, `vote` and the last read post
number, so a client can dim opened topics and badge the ones that gained replies. Read state can be set
explicitly (open, mark unread, mark a batch or a whole feed as read) and is pushed to the person's other
sessions over the gateway. Signed-out callers get no `viewer` object; clients keep their own local record.

## 4. Search & Discoverability

- Full-text search across boards/topics/posts (indexed via Postgres FTS, Elasticsearch, or Meilisearch)
- SEO-friendly canonical URLs and server-rendered metadata (OpenGraph, JSON-LD for forum threads)
- Sitemap generation and crawler-friendly pagination for indexing
- Search filters: by board, author, date range, tag, has-attachment, solved/unsolved
- Cross-Place global search (opt-in, respecting visibility/privacy)
- Autocomplete/typeahead for users, boards, and tags
- Search ranking tuned for relevance + recency + reaction weight

**Entities:** `SearchIndexDocument`, `Tag`

## 5. Real-Time Chat

- Text channels within a Place (separate from forum boards, Discord-style)
- Real-time messaging via WebSocket/gateway service
- Message history, edit, delete, and pinning
- Typing indicators, read receipts, presence (online/idle/offline)
- Direct messages and group DMs between users
- Channel categories, permission overwrites per channel
- Slash-command style bot/integration hooks
- Rich embeds for links (unfurling) and inline media previews
- Message threads (lightweight, scoped discussions off a channel message)

**Entities:** `Channel`, `Message`, `MessageRevision`, `Presence`, `DirectMessageThread`

## 6. Voice

- Voice channel creation and join/leave lifecycle
- SFU-based media routing via self-hosted LiveKit (see **Technology Stack (Go)**) for scalable voice
- Signaling service (WebRTC offer/answer/ICE negotiation) over WebSocket
- Per-channel voice state (muted, deafened, speaking indicator, video/screenshare on-off)
- Voice permission rules (who can join/speak/share screen) inherited from role system
- Push-to-talk vs. voice-activity detection support (client-driven, backend just relays state)
- Call quality telemetry (packet loss, bitrate) for diagnostics
- Recording/transcription hooks (opt-in, with consent/notice requirements)

**Entities:** `VoiceChannel`, `VoiceSession`, `VoiceParticipantState`

## 7. Roles, Permissions & Moderation

- Role-based access control (RBAC) with per-Place custom roles
- Granular permission flags (post, reply, react, upload, manage-board, kick, ban, manage-roles, etc.)
- Permission inheritance: global role -> Place role -> board/channel overwrite
- Moderation queue: reported posts/messages/users with triage workflow
- Automated moderation: keyword/regex filters, spam heuristics, rate-limit on new accounts
- Manual actions: warn, mute, kick, ban (temp/permanent), shadow-ban, IP/device ban
- Audit log of all moderation and admin actions (who/what/when/why)
- Appeals workflow for banned/muted users
- Content flags: NSFW, spoiler tagging, mandatory content warnings
- Trust levels / reputation system to auto-grant privileges over time (optional, forum-style)

**Entities:** `Role`, `Permission`, `RoleAssignment`, `Report`, `ModerationAction`, `AuditLogEntry`, `Appeal`

## 8. Notifications

- In-app notification center (mentions, replies, reactions, DMs, moderation actions)
- Email digest notifications (configurable frequency)
- Push notifications (web push / mobile) via subscription endpoints
- Per-category notification preferences (mute topic, mute Place, mute channel)
- Webhook support for external integrations (Place-level outgoing webhooks)

**Entities:** `Notification`, `NotificationPreference`, `PushSubscription`, `Webhook`

## 9. Developer / Public API

- Public instance metadata + capability-negotiation endpoints as described under **Multi-Instance
  Architecture** — this is what lets any client target any instance
- Versioned REST (and/or GraphQL) API mirroring core domain features
- API key issuance per application with scoped permissions
- OAuth2 app registration flow for third-party integrations (bot accounts, client apps)
- Fair, transparent rate limiting (per-token, per-IP, per-endpoint tiers) with documented limits
- Rate-limit headers (remaining, reset) and clear 429 responses
- Pagination, filtering, and sparse-fieldset conventions across list endpoints
- Webhooks for events (new post, new member, moderation action) for bot/integration use
- Bot account framework (distinct identity type, elevated audit visibility)
- Published, versioned API documentation (OpenAPI/GraphQL schema)

**Entities:** `ApiClient`, `ApiKey`, `OAuthApplication`, `RateLimitBucket`

## 10. Transparency, Policy & Compliance

- Public, versioned policy documents (ToS, Privacy Policy, Community Guidelines) served via API
- Changelog/transparency log for policy changes
- Transparency reports: aggregate moderation/report statistics, optionally per-Place
- Consent tracking for data processing (cookie/privacy consent records)
- Data export (user data portability) and right-to-erasure request handling
- Configurable data retention policies (message/post history limits per self-hosted instance)

**Entities:** `PolicyDocument`, `ConsentRecord`, `DataExportRequest`, `TransparencyReportSnapshot`

## 11. Self-Hosting & Deployment

Two goals drive this section equally: someone should be able to `docker run`/`docker compose up` this on a
spare machine or a Raspberry Pi and be chatting in a few minutes with **zero YAML-editing required**, *and*
a team should be able to drop it into Kubernetes/ECS/Fly/Render behind managed Postgres and an S3 bucket
with **zero interactive prompts required**. The setup wizard and the config system are what make both of
these true from the same binary.

### First-Run Setup Wizard (priority feature, not an afterthought)

- **Zero-config quick start:** a single `docker compose up` (bundling Postgres/Redis/the app) or a single
  downloaded binary + `./gotalk serve` gets you to a running instance with sane defaults — no `.env`
  file required to *try it out* locally.
- **Browser-based guided wizard** (Ghost/Discourse/Nextcloud-style): on first boot, if the instance isn't
  configured yet, every request is redirected to `/setup` instead of 500-ing. The wizard walks through, in
  order: instance name/icon/description → admin account creation → storage backend (local disk vs.
  S3-compatible) → mail provider (SMTP or a provider API key, or "skip for now") → optional voice (LiveKit
  URL/keys) → review & confirm. Each step validates live (e.g., "testing SMTP connection…") so mistakes are
  caught before finishing, not discovered later as a confusing runtime error.
- **Pre-flight checks** run before the wizard (and on every boot): Postgres reachable & migrated, storage
  backend writable, (if configured) SMTP/S3/Redis/LiveKit reachable. Failures produce one clear, actionable
  message per dependency instead of a stack trace.
- **Non-interactive / headless setup mode** for automation: everything the browser wizard collects can
  instead be supplied via environment variables, a mounted config file, or a single
  `gotalk setup --config=config.yaml` invocation. This is the mode cloud/CI/IaC
  deployments use — `docker run -e GOTALK_SETUP_ADMIN_EMAIL=... -e DATABASE_URL=...` just works with no
  human ever opening a browser.
- **Idempotent & re-enterable:** re-running setup against an already-configured instance is a safe no-op
  unless explicitly forced (`--reset`), so wizard logic can also back a "reconfigure" admin settings page
  later instead of being a one-shot throwaway code path.
- **Config is persisted, not re-asked:** answers are written to the DB (and/or a generated config file) on
  completion, so container restarts/redeploys don't re-trigger the wizard — only a genuinely fresh instance
  sees it.
- **Clear state signaling:** `/healthz` (and the `/api/v1/instance` metadata endpoint) distinguishes
  "awaiting setup" from "healthy" from "degraded (e.g., mail not configured)" so orchestrators and humans
  alike know exactly what state the instance is in.

### Cloud-Readiness

- 12-factor configuration: every setting settable via env var, with config-file/flag layering for local
  convenience (see `koanf` in **Technology Stack (Go)**) — no setting is *only* reachable through the wizard
- Configurable CORS/allowed-origins so any client (not just an official one) can be pointed at the instance
  (see **Multi-Instance Architecture**)
- First-class support for managed/external dependencies: external Postgres (any managed provider),
  S3-compatible object storage (AWS S3, R2, MinIO, GCS via interop), managed Redis, hosted SMTP — the
  bundled Compose services are a local/dev convenience, not a hard dependency
- Runs behind a reverse proxy/load balancer/ingress unmodified (trusts `X-Forwarded-*`, no baked-in TLS
  requirement — TLS termination is expected to happen in front, though direct autocert/Let's Encrypt support
  is a reasonable built-in option for simple single-VM deploys)
- Stateless API/gateway processes so horizontal scaling is just "run more replicas"; sticky state (presence,
  voice signaling) lives in Redis/LiveKit, not in-process
- Kubernetes-native health model: separate liveness vs. readiness endpoints, graceful shutdown (drain
  in-flight requests/WebSocket connections on SIGTERM)
- Safe concurrent startup: schema migrations use an advisory lock so multiple replicas booting at once don't
  race each other
- Secrets sourced from env vars or mounted files so they work with Kubernetes Secrets, AWS/GCP/Azure secret
  managers, or Vault without any code change
- Helm chart and a plain Kubernetes manifest set, alongside the Docker Compose quick start, as first-class
  deployment artifacts (not just docs)
- Backup & restore tooling (DB dump + media export) that works the same whether storage is local disk or
  object storage
- Federation/import tooling (import threads/users from common forum software or Discord export, stretch goal)

**Entities:** `InstanceConfig`, `SetupState`

## 12. Platform Infrastructure & Cross-Cutting Concerns

*(see **Technology Stack (Go)** above for concrete library choices behind each of these)*

- Database layer (PostgreSQL primary datastore) with migration tooling
- Caching layer (Redis) for sessions, presence, rate limits, hot read paths
- Background job/queue system (email sending, search indexing, media processing, digests)
- Media processing pipeline (image resize/thumbnail, virus scan, file-type validation)
- Structured logging, metrics (Prometheus), and tracing (OpenTelemetry) for observability
- Centralized error handling and consistent API error format
- Automated test suite: unit, integration, and API contract tests
- CI/CD pipeline: lint, test, build, container publish
- Horizontal scalability: stateless API nodes, sticky-session-free gateway/voice design where possible
- Internationalization (i18n) support for system-generated content (emails, notifications)

## Suggested Build Phases

1. **Foundation:** Identity & accounts, Places, basic roles/permissions, Postgres + Redis infra, CI/CD,
   *and a minimal setup wizard/quick-start from day one* — onboarding ease is a launch requirement, not
   something bolted on at the end. **✅ Implemented** in [`repos/gotalk-server`](../repos/gotalk-server/README.md);
   see *Phase 1 status* below.
2. **Forum Core:** Boards/topics/posts, search indexing, notifications, moderation basics.
   **✅ Implemented**; see *Phase 2 status* below.
3. **Real-Time Layer:** Chat channels/messages, presence, DMs.
   **✅ Implemented**; see *Phase 3 status* below.
4. **Voice:** Signaling service, SFU integration, voice permissions.
   **✅ Implemented**; see *Phase 4 status* below.
5. **Platform Maturity:** Public API + rate limiting, webhooks/bots, transparency/policy endpoints, instance
   metadata/capability-negotiation endpoints. **✅ Implemented**; see *Phase 5 status* below.
6. **Topic Feeds:** Reddit-style feeds over forum topics for a Place and for the whole instance, topic
   votes, hot/new/active/top/rising/controversial sorting, cursor paging, and per-user "read" tracking so
   clients can show which topics were already opened. **Planned**; see *Phase 6 plan* below.
7. **Self-Hosting & Cloud Polish:** Full wizard (non-interactive mode, pre-flight checks, reconfigure flow),
   configurable CORS/allowed-origins, Helm chart/k8s manifests, managed-dependency support, backup/restore,
   storage/mail provider plugins.

### Phase 1 status

Delivered:

- **Accounts:** registration (respecting `open` / `invite_only` / `closed` instance modes), login by
  username or email, Argon2id password hashing, short-lived JWT access tokens with rotating single-use
  refresh tokens (reuse revokes the session), session listing/revocation, profile editing, password
  change (signs out other sessions), account deletion with data scrubbing and reserved usernames,
  per-IP/per-user rate limiting with a stricter tier on auth endpoints.
- **Places:** create, discover (search + member-count ranking), view by ID or slug, update, soft-delete,
  ownership transfer, join/leave, and `public` / `invite_only` / `private` visibility.
- **Roles & moderation basics:** stable permission bitfield (including reserved bits for forum, chat, and
  voice phases), implicit `@everyone` role, ordered custom roles, hierarchy enforcement, role assignment,
  nicknames, kick, ban/unban, and invites with expiry and use limits.
- **Setup & operations:** browser setup wizard protected by a one-time token, headless setup on boot or
  via `gotalk setup`, pre-flight checks, `/.well-known/gotalk-instance` and `GET /api/v1/instance`
  discovery, liveness/readiness probes, advisory-locked migrations, graceful shutdown, configurable CORS,
  trusted-proxy handling, 12-factor config with `_FILE` secrets, Docker Compose quick start,
  distroless multi-arch image, and a CI pipeline (lint, sqlc drift check, race-enabled tests, image publish).
- Some items listed under Phase 5 and 7 landed early because they were cheap and foundational: instance
  metadata/capability endpoints, API rate limiting, and configurable CORS.

Deferred from section 1 to later phases, mostly because they depend on outbound email (Phase 7
mail providers): email verification, password reset, OAuth/OIDC login, MFA, captcha hooks, and username
history.

### Phase 2 status

Delivered in [`repos/gotalk-server`](../repos/gotalk-server/README.md):

- **Boards:** per-Place board tree (top-level categories, boards, one level of sub-boards), ordering,
  slugs, flat vs. threaded reply modes, Q&A boards with solutions, NSFW flag, and topic/post counters.
  Per-board role permission overwrites implement the *Place role → board overwrite* inheritance from
  section 7: staff-only boards, announcement boards, and per-board moderators. Overwrites are applied
  from root to leaf, and `@everyone` resolves before other roles.
- **Topics & posts:** Markdown posts with stable post numbers, tags, pinned/locked/archived states,
  moving between boards, edit history (revisions), tombstoned soft-deletes that keep thread structure,
  direct replies (`parent_id`) with depth-first listing in threaded boards, emoji/shortcode reactions,
  accepted answers, per-user read state with unread counts, `@mention` parsing, and server-side drafts
  that sync across devices. Signed-out visitors can read public Places, which keeps content
  crawlable.
- **Search:** Postgres full-text index (`search_documents`, maintained in the same transaction
  as each write). Per-Place search covers every board the caller can read. Instance-wide search
  covers public content only, scoped by a derived per-board `is_public` flag. Supports
  author/tag/board/solved/date filters, relevance ranking blended with reactions and recency, and
  highlighted snippets, plus tag and member autocomplete.
- **Notifications:** in-app notification center (mention, reply, topic reply, new topic, reaction,
  solution, moderation) with unread counts, read/dismiss, and watch/normal/mute preferences at the
  Place, board, and topic level, where the most specific level wins. Recipients are filtered by board
  visibility, so mentions never leak private boards.
- **Moderation basics:** reports queue with content snapshots and resolve/dismiss triage, audit log of
  all moderation and admin actions (Phase 1 actions now included), warnings, timeouts (read-only
  participation), temporary bans, reasons on kicks/deletions, and moderator edits/deletions that notify
  the author. New permission bits: `VIEW_AUDIT_LOG`, `MODERATE_MEMBERS`, `MANAGE_REPORTS`.
- A `content` rate-limit tier for creating topics, replies, and reports.

Deferred from sections 3, 4, 7 and 8:

- **Attachments** (the `ATTACH_FILES` bit is reserved): needs the storage backends and media pipeline
  (Phase 7 / section 12).
- **Scheduled publishing, email digests, and push notifications:** need the background job runner and
  mail/push providers (Phase 7). Notifications are fanned out synchronously; since Phase 3 they are
  also pushed in real time over the gateway.
- **SEO rendering** (OpenGraph/JSON-LD pages, sitemaps): belongs to the web client. The API already
  provides the readable slugs and anonymous read access it needs.
- **Optional Meilisearch backend:** search sits behind a single query today, so Meilisearch can be
  added later without changing the API.
- **Automated moderation** (keyword filters, spam heuristics), appeals, trust levels, and
  shadow/IP bans: later moderation work. Outgoing webhooks are Phase 5.
- Server-side revision diffs (revisions return full previous content for clients to diff) and
  `#topic`/board link resolution in Markdown.

### Phase 3 status

Delivered in [`repos/gotalk-server`](../repos/gotalk-server/README.md):

- **Channels:** per-Place text channels grouped under categories, ordering, topics, an NSFW flag, and
  per-channel role permission overwrites for the reserved chat bits (`VIEW_CHANNELS`, `SEND_MESSAGES`,
  `MANAGE_MESSAGES`, `MANAGE_CHANNELS`, plus `ADD_REACTIONS`/`ATTACH_FILES`). Overwrites apply from
  category to channel, following the board rules from Phase 2. Chat is members-only. Deleting a
  category moves its channels to the top level.
- **Messages:** Markdown messages with time-ordered (UUIDv7) IDs, cursor-paged history
  (`before`/`after`/`around`), replies, client nonces for optimistic UI, author edits with revision
  history (`MessageRevision`), author and moderator deletion (audited, author notified), pins, emoji
  reactions, `@mention` and reply notifications that respect channel/category/place mutes, and message
  reports with content snapshots in the existing moderation queue.
- **Threads:** lightweight sub-channels of a text channel, optionally started from a message. They
  inherit the parent's permissions and can be archived (a new message unarchives them).
- **Read state:** per-user read positions per channel, with unread flags and unread mention counts.
  Reading a channel also reads the notifications it caused, syncs the user's other sessions and, in
  direct messages, sends read receipts to the other participants.
- **Direct messages:** one conversation per pair of users plus group conversations of up to 10 people
  with an owner, a name, adding and removing members, and ownership hand-off. Users can only message
  people they share a Place with, to limit unsolicited messages. Each conversation creates at most one
  unread `direct_message` notification. Conversations can be muted.
- **Gateway:** a WebSocket at `/api/v1/gateway` (hello → identify → READY, heartbeats, documented close
  codes). It streams message, reaction, typing, channel, read, presence, membership and notification
  events, so forum notifications are now real-time too. Events emitted inside a transaction are only
  published after it commits. Each replica routes events to its own connections and checks channel
  visibility against a per-place cache that is invalidated when roles, overwrites, channels, membership
  or ownership change. Kicked or banned members stop receiving events immediately, and ending a session
  (logout, revocation, password change, account deletion, refresh-token reuse) closes its connection.
  With Redis configured, events fan out through Redis pub/sub so clients may connect to any replica.
  If a replica's subscription drops (so events may have been missed), it asks its clients to reconnect
  (close code 4007) rather than risk stale permissions. Graceful shutdown closes connections with code 1001.
- **Presence:** online/idle/dnd/invisible per connection, aggregated per user, announced to the user's
  Places and conversation partners, and readable via `GET /presences`. Presence is stored in memory or in
  Redis with per-connection expiry, so connections on a crashed replica go offline on their own.
- A `chat` rate-limit tier (default 120/minute) for sending messages, reactions, typing and threads, plus
  per-connection limits on gateway frames.

Deferred from section 5:

- **Slash-command bot hooks:** belong with the bot account framework and outgoing webhooks (Phase 5).
- **Link unfurling and inline media previews:** need the background job runner for safe, rate-limited
  outbound fetches, plus the media pipeline (Phase 7). Attachments remain deferred for the same reason.
- **Gateway session resume:** clients reconnect and catch up with `GET …/messages?after=<last ID>`. A
  replayable event log can be added later without changing event payloads.
- **Chat message search and DM reporting:** search remains forum-only. Reports are Place-scoped, so
  reports about direct messages wait for instance-level moderation tooling.
- **Presence offline events after a replica crash:** stale connections stop counting as online within 90
  seconds, but no `PRESENCE_UPDATE` is sent for them.

### Phase 4 status

Delivered in [`repos/gotalk-server`](../repos/gotalk-server/README.md):

- **Voice channels:** a `voice` channel kind alongside text channels, inside categories or at the top
  level, with an optional user limit (`MOVE_MEMBERS` bypasses it). Voice channels have no messages.
  Instance metadata advertises `features.voice` when a LiveKit server is configured.
- **SFU integration:** media flows through a self-hosted LiveKit server. Joining a channel returns
  LiveKit's URL plus a short-lived access token for the channel's room. The client's LiveKit SDK then
  does the WebRTC offer/answer/ICE signaling over LiveKit's own WebSocket, so Gotalk never handles
  media or SDP. The backend talks to LiveKit through a small internal client: HS256 tokens, the
  RoomService Twirp API (remove participant, update permissions, list participants, delete room) and
  webhook verification. It is tested against a real LiveKit container. LiveKit is optional; without it,
  voice channels can be created but not joined. Docker Compose ships a preconfigured LiveKit behind a
  `voice` profile, and readiness reports `degraded` when LiveKit is unreachable.
- **Join/leave lifecycle:** one voice channel per user at a time (joining another one moves them).
  Voice state is stored in PostgreSQL and is therefore shared by every replica. Each stay is recorded as
  a `VoiceSession`. LiveKit webhooks report participants connecting and leaving, a 30-second
  reconciliation loop compares against LiveKit's room list so missed webhooks heal, and joins that never
  connect expire. Leaving, switching channels, being moved or disconnected, losing access, and ending
  the login session that joined all remove the participant from the LiveKit room as well.
- **Voice permissions:** `CONNECT_VOICE`, `SPEAK`, `SHARE_SCREEN`, `MUTE_MEMBERS` and `MOVE_MEMBERS` follow
  the role system with per-channel overwrites applied from category to channel, like chat. The join
  token restricts publishing (microphone for `SPEAK`; camera and screen share for `SHARE_SCREEN`). Role,
  overwrite, timeout and membership changes are pushed to LiveKit while users are connected, which
  stops tracks that are no longer allowed, and participants who lost access are disconnected. Timed-out
  members can listen but not speak or share.
- **Voice state:** self mute/deafen/camera/screen share reported by clients (push-to-talk vs. voice
  activity stays client-side), server mute/deafen, the effective `can_speak`/`can_stream`, and the
  connection status. Changes go out as `VOICE_STATE_UPDATE` gateway events to everyone who can see the
  channel, and `READY` includes the caller's voice state. Speaking indicators sent by clients are relayed
  as `VOICE_SPEAKING`.
- **Moderation:** server mute and deafen (persisting across the place's voice channels), moving members
  between channels (the moved member receives a `VOICE_SERVER_UPDATE` with a token for the new room), and
  disconnecting, all rank-checked and audited.
- **Call quality telemetry:** clients report packet loss, jitter, round-trip time and bitrate. These are
  averaged per voice session and shown to `MANAGE_CHANNELS` with connection times and end reasons.
  Sessions are kept for a configurable retention period (30 days by default).

Deferred from section 6:

- **Recording and transcription hooks:** need LiveKit Egress, object storage for the output (Phase 7 storage
  backends) and a consent/notice flow; the room-per-channel design leaves room for an Egress trigger later.
- **Voice in direct messages (calls):** voice is limited to place channels for now; DM calls need ringing and
  call-invitation flows on top of the same token and state machinery.
- **Stage/broadcast channels and per-user volume:** stage channels can be built on `SPEAK` overwrites plus a
  request-to-speak queue; per-user volume is purely client-side.

### Phase 5 status

Delivered in [`repos/gotalk-server`](../repos/gotalk-server/README.md):

- **API tokens:** personal access tokens (`gtp_…`) with `read` / `write` / `gateway` / `admin` scopes and
  optional expiry, stored only as SHA-256 hashes and shown once. `read` covers `GET` requests and `write`
  everything else. An administrator's token only keeps instance-admin powers with the `admin` scope.
  Operations that manage credentials or need the person themselves (logout, sessions, password, account
  deletion, tokens, creating/deleting applications, consent) require a login session. Tokens use the
  gateway like sessions do; revoking one closes its connections, and changing the password revokes all of
  them.
- **Bots and applications:** an application owns a bot account, a distinct user type (`bot: true`) with
  its own token (`gtb_…`, `Bearer` or `Bot` scheme). Bots cannot log in with a password, create or own
  places, join places by themselves, or use voice. `MANAGE_PLACE` adds them to a place (public
  applications by anyone, private ones by their owner), after which roles and overwrites apply as usual.
  The audit log tags actions taken by bots and tokens (`via`, `token_id`, `application_id`), which gives
  their actions extra visibility in the audit log. Deleting an application removes its bot everywhere, and
  deleting an account deletes the applications it owns.
- **Slash commands (bot hooks deferred from Phase 3):** applications define up to 50 commands with typed
  options. Members list the commands of bots that can see a channel, invoke them with `SEND_MESSAGES`,
  and the bot receives a validated `INTERACTION_CREATE` gateway event and answers with ordinary messages.
- **Outgoing webhooks:** new `MANAGE_WEBHOOKS` permission; up to 10 webhooks per place for member,
  forum, chat, moderation (`VIEW_AUDIT_LOG`) and report (`MANAGE_REPORTS`) events. Content events only
  cover boards and channels visible to `@everyone`. Deliveries are an outbox written in the event's
  transaction and drained by every replica with `FOR UPDATE SKIP LOCKED`. Payloads use the REST
  shapes and are signed with HMAC-SHA256 (`Gotalk-Signature: t=…,v1=…`). Failed attempts are retried with
  backoff (6 attempts over about 7 hours). A webhook is disabled and audited after 10 deliveries in a
  row fail. There is a delivery log with redelivery, plus ping and secret rotation. The delivery client
  ignores proxies, does not follow redirects and refuses non-public addresses after DNS resolution
  (`webhooks.allow_private_networks` lifts this for trusted setups).
- **Rate limiting:** tiers are now also reported in `X-RateLimit-Tier`, and `GET /rate-limits` shows the
  caller's standing in every tier without using up a request. Limits are per user (sessions and personal
  tokens share them; each bot is its own user) or per IP when signed out.
- **Policies, consent and transparency:** versioned `terms` / `privacy` / `guidelines` documents published
  by instance administrators (optionally scheduled and marked as requiring consent). Their version list
  doubles as the policy changelog, and `/instance` links to the versions in effect. Consent is an
  append-only `ConsentRecord` log per purpose with an "outstanding policies" view, and registration
  can accept the current policies. Transparency reports (instance-wide and per place) give counts only:
  reports by reason and status, moderation actions by type, and content removed over a period of up to
  366 days.
- **Capability negotiation:** `/instance` now advertises `min_version` / `max_version`, the version
  header, token scopes, feature flags for the new capabilities, content and resource limits, and webhook
  events. Every API response carries `Gotalk-Api-Version`, and requests that send an unsupported version
  in it get `400` up front.

Deferred from sections 9 and 10:

- **OAuth2 authorization-code flow for third-party client apps:** needs a consent screen in the web client
  and redirect handling. Personal access tokens and bots cover scripts and integrations meanwhile, and
  `api_tokens` scopes are the vocabulary OAuth grants would reuse.
- **Data export and retention policies:** building export archives and enforcing retention windows need
  the background job runner and object storage (Phase 7). Account erasure already exists.
- **HTTP interaction endpoints and interaction persistence:** bots receive commands over the gateway only;
  sending interactions to an application URL (reusing the webhook signer) and replaying them to bots that
  were offline can follow.
- **Sparse fieldsets and cursor pagination everywhere:** list endpoints keep their current offset or
  cursor conventions.
- **Account deletion events:** deleting an account (or an application's bot) does not send `member.leave`
  webhooks or gateway `PLACE_LEAVE` events for each place, matching the existing deletion behavior.

### Phase 6 plan

Goal: a Reddit-style feed of forum topics for a single Place and for the whole instance, sortable several
ways, where each person's opened topics are remembered. It builds on the Phase 2 topics, reactions and read
state, so it needs no new infrastructure (the background job runner from Phase 7 is deliberately not required).

Scope:

- **Feed endpoints:**
  - `GET /places/{place}/feed` and `GET /feed` (`scope=home|all`), with `sort`, `t`, `board`, `tag`,
    `solved`/`unsolved`, `hide_read`, `nsfw`, `include_archived`, `pinned=first` and `limit`
  - cursor paging with an opaque cursor that encodes the sort key and the topic ID, so items do not shift or
    repeat while someone scrolls, unlike the offset paging used elsewhere
  - anonymous access for public Places and `scope=all`; the visible boards are resolved with the existing
    permission resolver for a Place, and by the derived `is_public` flag for instance-wide results (the
    same rule search uses), so private boards never leak
  - muted boards, topics and Places are left out of `scope=home`; moderator-removed and soft-deleted topics
    are never returned, and archived ones only on request
- **Feed item shape:** the topic summary plus the board, the Place (instance feed), the author, a plain-text
  excerpt of the opening post (about 280 characters, no Markdown or HTML), score and vote counts, reply count,
  last activity, tags, pinned/locked/solved/NSFW flags and the `viewer` object (`read`, `has_new_replies`,
  `unread_count`, `vote`, `last_read_post_number`). The viewer object is omitted when signed out.
- **Votes (`TopicVote`):** `PUT /topics/{id}/vote` with `{"value": 1 | -1}` and `DELETE /topics/{id}/vote`;
  one vote per user and topic, no voting on one's own topic, rate-limited, reversible, and gated by a
  `voting_enabled` Place setting (default on). Voting follows the existing forum reaction permission rather than
  adding a permission bit. Account deletion removes a person's votes and recomputes the affected scores.
- **Ranking (`TopicFeedStats`):** one row per topic with `up`, `down`, `score`, `reply_count`, `last_activity_at`,
  `hot_rank` and `controversy`, updated in the same transaction as each vote, reply, deletion or move.
  - `hot_rank` uses a time-independent formula (`sign(s)·log10(max(|s|, 1)) + created_at / 45000`, with `s`
    being the score plus a damped reply weight), so the stored value stays comparable as time passes and no
    periodic recomputation is needed
  - `new`, `active`, `top` and `controversial` sort on indexed columns, with `top` and `controversial`
    narrowed by the `t` window
  - `rising` is computed at query time over a bounded 48-hour candidate set from vote and reply velocity
  - indexes are per Place (`place_id`, sort key, `topic_id`); the home feed merges the member Places' pages
    with a capped candidate window per Place
  - pinned topics keep a separate ordering so `pinned=first` costs nothing on other sorts
- **Read tracking (`TopicOpen`):**
  - `PUT /topics/{id}/read` records that the person opened the topic (sets `first_opened_at` on the first call,
    refreshes `last_opened_at` and snapshots the reply count at that moment). It is idempotent and cheap, and
    the client calls it when a topic is opened, never when it merely scrolls past in a feed
  - `DELETE /topics/{id}/read` marks a topic as unread again
  - `POST /feed/read` takes up to 100 topic IDs (for a client replaying offline opens);
    `POST /places/{place}/feed/read` marks everything in a Place or board read, up to an optional `before`
    cursor, so "mark all as read" matches what the person saw
  - `has_new_replies` is derived by comparing the current reply count with the snapshot from the last open,
    so a topic that was opened and then gained replies is read, but flagged
  - advancing the existing `ReadState` position of a topic also records an open, so topics opened through
    search, a notification or a link count as read without a separate call
  - a `TOPIC_READ_STATE_UPDATE` gateway event syncs the person's other sessions, like chat read state
  - rows are scrubbed on account deletion and removed with their topic; reads older than a configurable
    retention period (a year by default) are pruned lazily on write
- **Capability negotiation:** `/instance` advertises `features.feed` and `features.topic_votes`, the supported
  `sorts`, and the limits (page size, batch size for `POST /feed/read`).
- **Tests:** ranking order for every sort and window, cursor stability while scores change, visibility
  (private boards, muted places, anonymous callers, deleted and archived topics), vote rules and score
  recomputation, read/unread/new-replies transitions, bulk mark-read, and gateway sync. A seed script produces a
  large synthetic Place to check query plans and the `rising` candidate cap.

Deferred:

- Comment (post) votes, vote-based karma and trust levels, and downvote thresholds that hide topics.
- Personalized or learned ranking, saved topics, and custom feeds that combine chosen Places or boards.
- Media thumbnails and link previews in feed items (they need attachments from Phase 7).
- Live feed updates over the gateway ("N new topics"); clients poll or refetch on focus until then.
- Per-board default sort and per-Place feed customization beyond `voting_enabled`.
