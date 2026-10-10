# gotalk-server

The backend for [Gotalk](../../README.md): a forum-first, self-hostable alternative to Discord.
This repository implements **Phase 1 (Foundation)**, **Phase 2 (Forum Core)**,
**Phase 3 (Real-Time Layer)**, **Phase 4 (Voice)**, **Phase 5 (Platform Maturity)**,
**Phase 6 (Topic Feeds)** and **Phase 7 (Self-Hosting & Cloud Polish)** of the
[backend plan](docs/backend-plan.md): accounts, places, roles and permissions, invites
and bans, the first-run setup wizard, boards, topics and posts, ranked topic feeds with
voting, full-text search, notifications, moderation tools, chat channels and threads, direct and group messages,
presence, a WebSocket gateway, and voice/video channels routed through a self-hosted
[LiveKit](https://livekit.io) server, plus the infrastructure to run it anywhere from a
Raspberry Pi to Kubernetes: pluggable file storage (local disk or any S3-compatible store)
and email (SMTP, SendGrid, Mailgun, Postmark, Resend, Amazon SES), password reset and email
verification, image uploads, backups, and a Helm chart.

- Single static Go binary (~20 MB distroless image), PostgreSQL required, Redis and LiveKit optional
- Browser setup wizard **or** fully headless setup from environment variables; the same page
  lets administrators change storage, email, voice and CORS later
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
pre-flight checks, naming your instance, the administrator account, file storage (local disk
or S3-compatible), email (or "skip for now") and optional voice. Each step can be tested live
(the email step sends you a test message) before you finish. That's it.

For anything beyond trying it out locally, copy [.env.example](.env.example) to `.env` and
set at least `POSTGRES_PASSWORD` and `GOTALK_SERVER_PUBLIC_URL`.

To try email without a real provider, start the bundled [Mailpit](https://mailpit.axllent.org)
and read the messages at <http://localhost:8025>:

```sh
GOTALK_MAIL_DRIVER=smtp GOTALK_MAIL_SMTP_HOST=mailpit GOTALK_MAIL_SMTP_PORT=1025 \
GOTALK_MAIL_SMTP_TLS=none GOTALK_MAIL_FROM="Gotalk <noreply@localhost>" \
docker compose --profile mail up -d
```

### Skip the browser (headless setup)

Set all three admin variables and setup completes on boot:

```sh
GOTALK_SETUP_ADMIN_USERNAME=operator \
GOTALK_SETUP_ADMIN_EMAIL=operator@example.com \
GOTALK_SETUP_ADMIN_PASSWORD=change-me-please \
docker compose up -d
```

(`admin`, `root` and a few other names are reserved.) Or run it once as a job (e.g. a
Kubernetes `Job` or CI step) with `gotalk setup`, which also runs the pre-flight checks first
and refuses to continue if one fails (`--skip-checks` overrides). Both are idempotent: on an
already configured instance they do nothing, unless asked to:

- `gotalk setup --reset` re-applies the `setup.*` values that are set: instance name,
  description and registration mode, and the administrator account, which is created if
  missing, or promoted to administrator with its password reset and its sessions and personal
  access tokens revoked. This is also the way back in after losing the admin password.
- `gotalk setup --reset-settings` forgets storage, email, voice and CORS settings saved in the
  browser, so the config file, environment and defaults apply again.

### Changing settings later

Once set up, `/setup` becomes the instance settings page: sign in as an instance
administrator to change file storage, email, voice and CORS, test them (the email section
sends you a test message) and see live health. The same is available through the API
(`GET/PATCH /instance/config`, see below). Settings saved this way are stored in the
database and apply to every replica within 15 seconds, without a restart.

A section whose enabling key is set in the config file or environment (`storage.driver`,
`mail.driver`, `voice.livekit_url`, `server.cors_allowed_origins`) is managed there and shown
read-only in the browser; that is how infrastructure-as-code deployments keep settings in one
place.

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

Prebuilt binaries (Linux, macOS and Windows) are attached to each
[GitHub release](https://github.com/parkerbrown98/gotalk-server/releases), with a
`checksums.txt` for verification. Tags with a suffix such as `v0.1.0-beta.1` are published as
pre-releases. Or build from source:

```sh
go build -o gotalk ./cmd/gotalk
GOTALK_DATABASE_URL="host=localhost user=gotalk dbname=gotalk sslmode=disable" PGPASSWORD=… ./gotalk serve
```

Without `GOTALK_DATABASE_URL`, the server connects to a local PostgreSQL database named
`gotalk` (user and password `gotalk`) on `localhost:5432`. Migrations run automatically on
boot.

| Command | Purpose |
|---|---|
| `gotalk serve` | Run the server (default when no command is given). Logs one pre-flight line per dependency at boot |
| `gotalk migrate` / `gotalk migrate status` | Apply migrations / show schema version |
| `gotalk setup [--reset] [--reset-settings] [--skip-checks]` | Complete first-run setup from config/env, then exit (see above) |
| `gotalk check` | Run the pre-flight checks (database, Redis, public URL, storage, email, voice) with a fix-it hint per problem; exits 1 if any fails |
| `gotalk backup [--output FILE\|-] [--no-media] [--upload [--keep N]]` | Write a backup archive; see [Backups](#backup-and-restore) |
| `gotalk backup list` | List backups stored in the storage backend |
| `gotalk restore --input FILE\|- \| --from-storage NAME [--force] [--no-media]` | Restore a backup |
| `gotalk healthcheck` | Exit 0 if the local server is healthy (used by the image's `HEALTHCHECK`) |
| `gotalk version` | Print the version |

All commands accept `--config path/to/gotalk.yaml` (alias `--from-file`).

## File storage and email

Both are plugins chosen by name, configured in the setup wizard, the settings page, the
config file or the environment.

**Storage** keeps uploaded images (and backups made with `backup --upload`):

- `local` (default): a directory, `/data` in the container image (a named volume in Compose,
  a PersistentVolumeClaim in the Helm chart). It is not shared between machines, so more than
  one replica needs `s3` or a ReadWriteMany volume.
- `s3`: any S3-compatible store: AWS S3 (leave the endpoint empty and set the region),
  Cloudflare R2 (region `auto`), MinIO, SeaweedFS and Backblaze B2 (set
  `s3_force_path_style` for MinIO and SeaweedFS), Google Cloud Storage through its XML API with
  HMAC keys. Requests are signed with AWS Signature V4; the access key needs to read, write,
  list and delete objects under the prefix.

By default Gotalk serves uploads itself under `/media/…` (with immutable caching headers, a
sandboxing CSP and only for files it recorded as uploads, so backups in the same bucket stay
private). Set `storage.public_url` to a CDN or public bucket URL to link there instead.

**Email** sends password reset links and email verification:

| Driver | Settings |
|---|---|
| `smtp` | `smtp_host`, `smtp_port`, `smtp_tls` (`starttls` default, `tls` for implicit TLS on 465, `none`), `smtp_username`, `smtp_password`; PLAIN, LOGIN (Office 365) and CRAM-MD5 auth |
| `sendgrid` | `api_key` (needs the Mail Send permission) |
| `mailgun` | `api_key`, `domain`; `api_url: https://api.eu.mailgun.net` for the EU region |
| `postmark` | `api_key` (server token) |
| `resend` | `api_key` (sending-only keys work) |
| `ses` | `region`, `access_key_id`, `secret_access_key` (Amazon SES v2 API) |
| `log` | Development only: emails are written to the server log instead of being sent |

All need `mail.from`. Messages go through an outbox in PostgreSQL written in the same
transaction as the action that sends them, and are delivered by any replica with retries
(15 seconds up to 30 minutes, six attempts). Message bodies (which contain single-use links)
are erased once sent or given up on, and links that expire before they could be sent are
dropped. Without email, the instance works, but reports `degraded` with `email` in
`degraded_features`, and the password reset and verification endpoints answer `503`.

Third-party drivers can be added in Go with `storage.Register` / `mail.Register` (see
[internal/storage](internal/storage) and [internal/mail](internal/mail)).

## Backup and restore

`gotalk backup` writes one `.tar.gz` holding every table (as PostgreSQL `COPY` data from a
single consistent snapshot, so the server can keep running) and the uploaded files, read
through the storage driver. No `pg_dump` is needed, and an archive taken from local storage
restores into S3 and the other way round, which also makes it the way to move between storage
backends.

```sh
gotalk backup                               # gotalk-backup-<UTC time>.tar.gz in the current directory
gotalk backup --output - > backup.tar.gz    # to stdout
gotalk backup --upload --keep 14            # into the storage backend under backups/, keeping the 14 newest
gotalk backup list
```

To restore, stop the server and run `gotalk restore` against an empty database (or one with a
fresh, never set-up instance); `--force` replaces an existing instance's data. The archive is
verified in full first (checksums, every table present with the recorded row count), so a
damaged file never touches the database. The database is then restored in one transaction
with foreign keys re-validated, migrated to the running version, and the media copied into the
configured storage.

```sh
gotalk restore --input backup.tar.gz
docker compose exec -T gotalk /gotalk restore --input - < backup.tar.gz   # from the host
gotalk restore --from-storage gotalk-backup-20260101-030000.tar.gz --force
```

Archives contain everything in the database, including password hashes and secrets saved
in the settings page; store them accordingly. In Kubernetes, the Helm chart's `backup`
values schedule `backup --upload` as a CronJob.

## Deploying to Kubernetes

- **Helm:** [deploy/helm/gotalk](deploy/helm/gotalk) (see its README). It supports managed
  PostgreSQL/Redis via URLs or existing Secrets, S3 or a PVC for storage, every mail driver,
  LiveKit, an Ingress, autoscaling, a PodDisruptionBudget, a backup CronJob, a hardened pod
  (non-root, read-only root filesystem with an `emptyDir` at `/tmp` for backups), and an
  optional built-in PostgreSQL and Redis for evaluation. It refuses configurations that would
  break at runtime (no database, several replicas with unshared local storage or without Redis).
- **Plain manifests:** [deploy/kubernetes](deploy/kubernetes), a Kustomize base for two
  replicas behind an Ingress with managed PostgreSQL, Redis and S3, configured from a
  `gotalk.env` file turned into a Secret.

Both pass `helm lint`/`kubeconform` in CI. For the gateway WebSocket, raise the ingress
idle timeout above the 30 second heartbeat (for ingress-nginx,
`nginx.ingress.kubernetes.io/proxy-read-timeout` and `proxy-send-timeout`). The setup link is
printed in the pod log (`kubectl logs deploy/gotalk`), or set the `setup.admin` values for
headless setup.

## Deploying to the cloud

The same binary is built for unattended, horizontally scaled deployments:

- **12-factor config.** Every setting is an environment variable. `DATABASE_URL`,
  `REDIS_URL` and `PORT` (as injected by Heroku, Render, Fly, Cloud Run, …) are honored
  automatically.
- **Secrets from files.** Append `_FILE` to any variable to read it from a mounted file,
  e.g. `GOTALK_SETUP_ADMIN_PASSWORD_FILE=/run/secrets/admin-password`. libpq variables such as
  `PGPASSWORD` also work.
- **Managed dependencies.** Point `GOTALK_DATABASE_URL` at any managed PostgreSQL,
  `GOTALK_REDIS_URL` at any managed Redis, `GOTALK_STORAGE_*` at any S3-compatible bucket and
  `GOTALK_MAIL_*` at a hosted email provider. The Compose services are a local convenience.
  Empty variables are ignored, so Compose files and templates can pass optional ones through.
- **Multiple replicas.** Migrations take a PostgreSQL advisory lock so replicas can boot
  together, setup completion is race-safe, and the JWT secret is shared through the
  database. Set `GOTALK_REDIS_URL` so rate limits, presence and gateway events are shared
  across replicas. Without Redis, a client connected to one replica would miss events
  caused by requests served by another. Use S3 storage (or a shared volume) so every replica
  sees the same uploads. Settings changed in the browser reach every replica within 15
  seconds.
- **Probes.** `GET /healthz` is liveness (process is serving; its `state` field says
  `awaiting_setup`, `healthy` or `degraded`). `GET /readyz` is readiness: HTTP 503 when
  PostgreSQL or Redis is unreachable; otherwise HTTP 200 with `awaiting_setup` (so the wizard
  stays reachable through your load balancer), `ready`, or `degraded` when an optional
  feature fails: LiveKit unreachable, storage not writable, or email not configured or
  rejecting the connection. Storage and email results are cached (1 and 5 minutes) so probes
  stay cheap. `GET /api/v1/instance` carries the same `status` and `degraded_features` for
  clients. SIGTERM drains in-flight requests and closes
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
| `server.cors_allowed_origins` | `*` | Origins allowed to call the API and open gateway connections (any client by default); setting it makes CORS read-only in the browser |
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
| `ratelimit.default` | `300-M` | Default tier, per user (or per IP when anonymous); a user's personal access tokens share it |
| `ratelimit.auth` | `10-M` | Login, registration, refresh, setup, and password endpoints |
| `ratelimit.content` | `30-M` | Creating topics, replies, and reports; marking feeds read in bulk |
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
| `webhooks.allow_private_networks` | `false` | Let webhooks reach loopback, private and link-local addresses. Keep off unless every place manager is trusted |
| `webhooks.timeout` | `10s` | Timeout of each webhook delivery attempt |
| `webhooks.delivery_retention` | `168h` | How long finished webhook deliveries stay in the delivery log |
| `storage.driver` | `local` | `local` or `s3`; setting it makes storage read-only in the browser |
| `storage.local_path` | `data` (`/data` in the image) | Directory of the `local` driver |
| `storage.s3_endpoint` | AWS S3 in `s3_region` | S3 API endpoint (R2, MinIO, B2, GCS, …) |
| `storage.s3_region` | `us-east-1` | Signing region (`auto` for R2) |
| `storage.s3_bucket` / `s3_prefix` | | Bucket, and an optional key prefix inside it |
| `storage.s3_access_key_id` / `s3_secret_access_key` | | S3 credentials |
| `storage.s3_force_path_style` | `false` | Path-style URLs (MinIO, SeaweedFS, most self-hosted stores) |
| `storage.public_url` | *(served under `/media/`)* | Base URL of a CDN or public bucket serving uploads |
| `uploads.max_size` | `8388608` | Largest accepted upload in bytes (64 KiB–100 MiB) |
| `mail.driver` | *(email off)* | `smtp`, `sendgrid`, `mailgun`, `postmark`, `resend`, `ses` or `log`; setting it makes email read-only in the browser |
| `mail.from` | | Sender, e.g. `Gotalk <noreply@forum.example.com>` |
| `mail.smtp_host` / `smtp_port` / `smtp_tls` | / `587` / `starttls` | SMTP server; `smtp_tls` is `starttls`, `tls` or `none` |
| `mail.smtp_username` / `smtp_password` | | SMTP credentials |
| `mail.api_key` / `api_url` / `domain` | | API provider key, base URL override, Mailgun domain |
| `mail.region` / `access_key_id` / `secret_access_key` | | Amazon SES |

Rates use `<limit>-<period>` where period is `S`, `M`, `H`, or `D`. CORS origins may be `*`,
exact origins (`https://app.example.com`), origins with one wildcard (`https://*.example.com`) or
custom schemes. The desktop app's webview sends `tauri://localhost` (macOS and Linux) or
`http://tauri.localhost` (Windows), so allow those when restricting origins.

## API overview

Everything lives under `/api/v1`. The full, always-current contract is the OpenAPI document
at `/api/v1/openapi.json` (browse it at `/api/v1/docs`).

**Connecting a client.** Given only a domain, fetch `/.well-known/gotalk-instance` to find
the API base URL and gateway URL, then `GET /api/v1/instance` for the instance name,
registration mode, supported API versions (`min_version`/`max_version`), feature flags,
limits (message and post lengths, webhooks per place, …), webhook events, policy links and
published rate limits. Every API response carries a `Gotalk-Api-Version` header; clients may
send the version they were built for in the same header, and an instance that does not
support it answers `400` instead of failing call by call.

**Authentication.** `POST /auth/register` or `/auth/login` returns a short-lived JWT access
token (send as `Authorization: Bearer …`) and a single-use refresh token. `POST /auth/refresh`
rotates both; presenting an already-used refresh token revokes the whole session. Sessions
can be listed and revoked under `/users/@me/sessions`.

**Password reset and email verification** (when email is configured; `features.password_reset`
and `features.email_verification` in `/instance`). `POST /auth/password-reset` with an email
address sends a link to `<instance>/reset-password?token=…` (valid for an hour, once), and
always answers `202` so it cannot reveal who has an account; repeated requests within a
minute send nothing more. That page (or any client) calls `POST /auth/password-reset/confirm`
with the token and new password, which signs out every session and revokes personal access
tokens. Registration sends a verification link (`<instance>/verify-email?token=…`, valid for 48
hours) that calls `POST /auth/verify-email`; `POST /users/@me/email/verification` sends a new
one. Links use `server.public_url` when set. `email_verified` is on `GET /users/@me`.

**Uploads.** Avatars (`PUT /users/@me/avatar`), place icons and banners
(`PUT /places/{place}/icon`, `/banner`, `MANAGE_PLACE`) and the instance icon
(`PUT /instance/icon`, administrators) take the raw image as the request body: PNG, JPEG, GIF
or WebP up to `limits.upload_size` bytes and `limits.upload_max_side` pixels per side. EXIF,
XMP and text metadata (such as a photo's GPS position) are stripped without re-encoding the
image. The response carries the new URL; `DELETE` on the same paths removes the image. Replaced
files are deleted right away, and files nothing refers to any more (for example after an
account deletion) after about an hour. URLs set directly with `PATCH` keep working.

**Instance settings (administrators).** `GET /instance/config` shows storage, email, voice and
CORS with their `source` (`default`, `config` or `settings`), whether they are editable and
which secrets are set (secrets are never returned). `PATCH /instance/config` takes any of
`storage`, `mail`, `voice`, `cors`, checks them live, and saves and applies them on every
replica; a failing check answers `422` unless `?force=true`. Secrets left empty keep their
current value. `DELETE /instance/config/{section}` forgets saved settings,
`POST /instance/config/test` checks settings without saving (optionally sending a test email),
and `GET /instance/checks` runs the pre-flight checks. These need a login session, not an API
token. During first-run setup, `GET /setup/status` with a `Gotalk-Setup-Token` header and
`POST /setup/test` do the same with the setup token, and `POST /setup` accepts a `settings`
object.

**API tokens.** For scripts and integrations, `POST /users/@me/tokens` creates a personal
access token (`gtp_…`, shown once, stored only as a hash) with scopes: `read` allows `GET`
requests, `write` everything else, `gateway` the real-time connection, and `admin` keeps an
instance administrator's powers (without it, an administrator's token acts as a regular
user). Tokens may expire after up to 366 days and are sent like access tokens. They cannot
manage credentials: logging out, sessions, password changes, account deletion, other tokens,
creating or deleting applications, and recording consent all require a login session.
Revoking a token closes its gateway connections, and changing the password revokes every
personal access token.

**Applications and bots.** `POST /applications` registers an application together with a bot
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
the command, and the bot receives an `INTERACTION_CREATE` gateway event; it answers by sending
an ordinary message. Interactions are not stored, so offline bots miss them. Deleting an
application removes its bot from every place; its messages stay.

**Webhooks.** Members with `MANAGE_WEBHOOKS` can create up to 10 outgoing webhooks per place
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
resolution) unless `webhooks.allow_private_networks` is set.

**Policies and consent.** Instance administrators publish versioned policies
(`POST /policies/{kind}` for `terms`, `privacy` and `guidelines`), optionally scheduled up to a
year ahead and marked as requiring consent. `GET /policies` lists the versions in effect,
`/policies/{kind}/versions` is the changelog (each version can carry a summary of what
changed), and `/instance` links to them. Consent is an append-only log per purpose:
`POST /users/@me/consents` records accepting a policy version or giving or withdrawing
another purpose such as `analytics`. `GET /users/@me/consents` returns the latest decision per
purpose plus the `outstanding` policies whose current version the user has not accepted, which
clients should show. Registration accepts `accept_policies: true` to record consent to every
current policy that requires it. Consent records survive account deletion as proof, without
IP address or user agent.

**Transparency.** `GET /transparency` (instance-wide) and `GET /places/{place}/transparency`
(anyone who can see the place) report, for a period of up to 366 days (default: the last 30),
how many reports were filed by reason and their status, how many moderation actions were
taken (warnings, timeouts, kicks, bans, unbans, voice disconnects, deletions), and how much
content moderators removed. Reports contain counts only.

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

**Read state, subscriptions, notifications.** `PUT /topics/{id}/read` records that the
caller opened a topic and, with an optional `post_number`, how far they have read (positions
never move backwards). Authenticated topic responses include a `viewer` object: `read` (opened,
and not marked unread since), `has_new_replies` and `new_reply_count` (posts since the last open),
`unread_count` (posts after the read position), `last_read_post_number`, `vote` and `subscription`; the older top-level
`last_read_post_number` and `unread_count` fields remain. `DELETE /topics/{id}/read` marks a
topic unread again but keeps the position. Read changes reach the user's other sessions as
`TOPIC_READ_STATE_UPDATE` events. Authors have read their own topics and replies.
Places, boards and topics can be set to `watching`, `normal` or `muted`. Watching a place
or board notifies about new topics, and watching a topic notifies about every reply. Authors
watch their own topics automatically. The most specific setting wins, so watching a topic
inside a muted place still notifies. Notifications (`mention`, `reply`, `topic_reply`,
`new_topic`, `reaction`, `solution`, `moderation`, `direct_message`) carry a `data` snapshot
for rendering and are pushed to the recipient's gateway sessions as they are created.
Moderation notices ignore mutes.

**Feeds.** `GET /places/{place}/feed` ranks topics from every board of a place the caller
can read; `GET /feed` does the same across places, with `scope=home` (the caller's places, the
default when signed in, leaving out muted places, boards and topics) or `scope=all` (public
content on the instance, the only scope when signed out). `sort` is `hot` (default; score and
replies weighed against age), `new`, `active` (latest reply), `top`, `rising` (fastest-growing
topics of the last 48 hours) or `controversial` (many votes split between up and down);
`top` and `controversial` take a window `t` of `hour`, `day`, `week` (default), `month`,
`year` or `all`. Filters: `tag`, `solved`, `hide_read` (leave out opened topics without new
posts), `nsfw` (NSFW boards, and on instance feeds NSFW places, are left out unless true),
`include_archived`, and on place feeds `board` (a board or category and everything below it)
and `pinned=first` (pinned topics returned separately on the first page). Items are topics
plus their `board`, `place`, an `is_nsfw` flag and a plain-text `excerpt` of the opening post.
Pages are cursor-based: pass `next_cursor` back as `cursor` with the same parameters. A cursor
fixes the time the first page was loaded (`as_of`), so windows do not slide while paging. Keys
that change while someone pages (such as scores on `hot` and `top`) can move a topic across
the cursor, so clients should drop duplicate IDs. Members vote with
`PUT /topics/{id}/vote` (`{"value": 1}` or `-1`, needs `ADD_REACTIONS`, not on their own
topics) and `DELETE /topics/{id}/vote`. Places can turn voting off (`voting_enabled`), and
feeds then rank by the reactions on each opening post. `POST /feed/read` records up to 100
opens at once (offline replay, or importing what someone read while signed out), skipping
topics the caller cannot see. `POST /places/{place}/feed/read` marks a place (or a `board_id`)
read up to `before`, usually the feed's `as_of`, touching at most the 5000 most recently
active topics.

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
   `voice_state` (null when not in a voice channel). Personal access tokens with the `gateway`
   scope and bot tokens work too; their `session_id` is the token's ID.
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
| `TOPIC_READ_STATE_UPDATE` | The reader's sessions: `topics` lists changed read states; after marking a place read it is empty, with `all: true`, `place_id`, `board_id` and `before` |
| `READ_RECEIPT` | The other participants of a direct conversation |
| `NOTIFICATION_CREATE` | The notified user (forum and chat notifications) |
| `PRESENCE_UPDATE` | Members of the user's places and their conversation partners |
| `PLACE_JOIN`, `PLACE_LEAVE`, `PLACE_DELETE` | The member (or every member, for deletion) |
| `VOICE_STATE_UPDATE` | The user and everyone who can see the voice channel; `channel_id` is null when the user left it |
| `VOICE_SERVER_UPDATE` | A member who was moved to another voice channel (new `url`, `token` and `room`) |
| `VOICE_SPEAKING` | Everyone who can see the voice channel |
| `INTERACTION_CREATE` | The bot whose slash command was invoked |

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
| `4004` | Authentication failed, or the API token lacks the `gateway` scope |
| `4007` | Events may have been missed (e.g. the server lost its Redis connection); reconnect |
| `4008` | Too many frames (over 120 per minute) or too slow to read events |
| `4009` | Missed heartbeats |
| `4010` | Session ended |

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

**Errors** are [RFC 9457](https://www.rfc-editor.org/rfc/rfc9457) `application/problem+json`
documents. **Rate limits** are reported on every response via `X-RateLimit-Limit`,
`X-RateLimit-Remaining`, `X-RateLimit-Reset` and `X-RateLimit-Tier`; exceeded limits return
`429` with `Retry-After`. `GET /rate-limits` shows the caller's standing in every tier without
using up a request. Limits apply per user (shared by their sessions and personal access tokens;
each bot is its own user) or per IP address when signed out.

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
| `cmd/gotalk` | CLI entry point: serve, migrate, setup, check, backup, restore, healthcheck |
| `internal/api` | HTTP layer: chi router, huma operations, middleware, DTOs, WebSocket gateway, `/media` serving |
| `internal/service` | Business logic shared by the API and CLI (forum permissions are evaluated in `forum.go`, chat permissions in `channels.go`, voice state and LiveKit reconciliation in `voice.go`, webhook queueing and delivery in `webhooks.go`, provider settings and reloading in `providers.go`, pre-flight checks in `health.go`, the email outbox, password reset and verification in `mailflows.go`, uploads in `uploads.go`) |
| `internal/realtime` | Gateway event routing (hub), Redis or in-memory event broker, and presence store |
| `internal/livekit` | Minimal LiveKit client: participant tokens, RoomService calls, webhook verification |
| `internal/storage` | Storage driver registry and the `local` and `s3` drivers |
| `internal/mail` | Mail driver registry and the `smtp`, `sendgrid`, `mailgun`, `postmark`, `resend`, `ses` and `log` drivers |
| `internal/sigv4` | AWS Signature V4 signing shared by the S3 and SES drivers |
| `internal/media` | Image validation and lossless metadata stripping |
| `internal/backup` | Backup archives: consistent `COPY` dump plus media, verification and restore |
| `internal/store` | sqlc-generated, type-safe queries (do not edit by hand) |
| `internal/database` | Connection handling and embedded goose migrations |
| `internal/permissions` | Permission bits, role hierarchy, and board and channel overwrite rules |
| `internal/auth` | Argon2id passwords, JWT access tokens, refresh tokens, API tokens |
| `internal/config` | Layered configuration and validation |
| `internal/ratelimit` | Rate limit tiers backed by Redis or memory |
| `internal/web` | Embedded landing page, setup wizard / settings page, password reset and email verification pages |
| `deploy/helm/gotalk`, `deploy/kubernetes` | Helm chart and Kustomize manifests |
