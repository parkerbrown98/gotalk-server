# Configuration

[← Documentation](README.md)

Sources are layered: built-in defaults, then the `--config` YAML file
([config.example.yaml](../config.example.yaml)), then environment variables. Environment names
are `GOTALK_` + section + `_` + key, upper-cased: `server.public_url` becomes
`GOTALK_SERVER_PUBLIC_URL`. Lists are comma-separated.

## On this page

- [Server](#server)
- [Database and Redis](#database-and-redis)
- [Authentication](#authentication)
- [Rate limiting](#rate-limiting)
- [Logging](#logging)
- [First-run setup](#first-run-setup)
- [Voice](#voice)
- [Webhooks](#webhooks)
- [Storage](#storage)
- [Uploads and link previews](#uploads-and-link-previews)
- [Email](#email)
- [Value formats](#value-formats)

## Server

| Key | Default | Description |
|---|---|---|
| `server.addr` | `:8080` | Listen address |
| `server.public_url` | *(derived per request)* | External base URL, e.g. `https://forum.example.com` |
| `server.trust_proxy` | `false` | Honor `X-Forwarded-*` from trusted proxies |
| `server.trusted_proxies` | loopback + private ranges | CIDRs allowed to set forwarding headers |
| `server.cors_allowed_origins` | `*` | Origins allowed to call the API and open gateway connections (any client by default); setting it makes CORS read-only in the browser |
| `server.cors_allow_credentials` | `false` | Allow credentialed CORS (cannot be combined with `*`) |
| `server.shutdown_timeout` | `20s` | Graceful shutdown window |

## Database and Redis

| Key | Default | Description |
|---|---|---|
| `database.url` | local `gotalk` database | PostgreSQL URL or keyword/value DSN |
| `database.max_conns` | `20` | Connection pool size |
| `database.auto_migrate` | `true` | Apply migrations on `serve` |
| `database.connect_timeout` | `30s` | How long to wait for PostgreSQL/Redis at startup |
| `redis.url` | *(none)* | Optional; shares rate limits, presence and gateway events across replicas |

## Authentication

| Key | Default | Description |
|---|---|---|
| `auth.jwt_secret` | *(generated, stored in DB)* | HMAC key for access tokens; at least 32 characters |
| `auth.access_token_ttl` | `15m` | Access token lifetime |
| `auth.refresh_token_ttl` | `720h` | Refresh token (session) lifetime |

## Rate limiting

| Key | Default | Description |
|---|---|---|
| `ratelimit.enabled` | `true` | Enable rate limiting |
| `ratelimit.default` | `300-M` | Default tier, per user (or per IP when anonymous); a user's personal access tokens share it |
| `ratelimit.auth` | `10-M` | Login, registration, refresh, setup, and password endpoints |
| `ratelimit.content` | `30-M` | Creating topics, replies, and reports; marking feeds read in bulk |
| `ratelimit.chat` | `120-M` | Sending chat messages, reacting to them, typing indicators, and starting threads |

## Logging

| Key | Default | Description |
|---|---|---|
| `log.level` | `info` | `debug`, `info`, `warn`, `error` |
| `log.format` | `json` | `json` or `text` |

## First-run setup

See [Getting started](getting-started.md#skip-the-browser-headless-setup) for how these are used.

| Key | Default | Description |
|---|---|---|
| `setup.token` | *(generated)* | Override the browser wizard's one-time token |
| `setup.instance_name` | `Gotalk` | Instance name for headless setup / wizard default |
| `setup.instance_description` | | Instance description for headless setup |
| `setup.registration_mode` | `open` | `open`, `invite_only`, or `closed` |
| `setup.admin_username` / `admin_email` / `admin_password` | | Set all three for headless setup |

## Voice

See [Voice channels](getting-started.md#voice-channels) for setting up LiveKit.

| Key | Default | Description |
|---|---|---|
| `voice.livekit_url` | *(none)* | LiveKit URL clients connect to (`ws`, `wss`, `http` or `https`); setting it enables voice |
| `voice.livekit_api_url` | `voice.livekit_url` | How this server reaches LiveKit's API, when that differs (e.g. `http://livekit:7880`) |
| `voice.livekit_api_key` / `livekit_api_secret` | | LiveKit API credentials; the secret must be at least 32 characters |
| `voice.token_ttl` | `10m` | How long a voice join token can be used to connect |
| `voice.join_timeout` | `60s` | Joins that never connect to LiveKit are dropped after this |
| `voice.session_retention` | `720h` | How long ended voice sessions and their call quality are kept |

## Webhooks

| Key | Default | Description |
|---|---|---|
| `webhooks.allow_private_networks` | `false` | Let webhooks reach loopback, private and link-local addresses. Keep off unless every place manager is trusted |
| `webhooks.timeout` | `10s` | Timeout of each webhook delivery attempt |
| `webhooks.delivery_retention` | `168h` | How long finished webhook deliveries stay in the delivery log |

## Storage

See [File storage and email](storage-and-email.md#storage) for what each driver needs.

| Key | Default | Description |
|---|---|---|
| `storage.driver` | `local` | `local` or `s3`; setting it makes storage read-only in the browser |
| `storage.local_path` | `data` (`/data` in the image) | Directory of the `local` driver |
| `storage.s3_endpoint` | AWS S3 in `s3_region` | S3 API endpoint (R2, MinIO, B2, GCS, …) |
| `storage.s3_region` | `us-east-1` | Signing region (`auto` for R2) |
| `storage.s3_bucket` / `s3_prefix` | | Bucket, and an optional key prefix inside it |
| `storage.s3_access_key_id` / `s3_secret_access_key` | | S3 credentials |
| `storage.s3_force_path_style` | `false` | Path-style URLs (MinIO, SeaweedFS, most self-hosted stores) |
| `storage.public_url` | *(served under `/media/`)* | Base URL of a CDN or public bucket serving uploads |

## Uploads and link previews

| Key | Default | Description |
|---|---|---|
| `uploads.max_size` | `8388608` | Largest accepted upload in bytes (64 KiB–100 MiB), attachments included |
| `embeds.enabled` | `true` | Fetch previews of links in messages and posts |
| `embeds.allow_private_networks` | `false` | Let link previews fetch loopback, private and link-local addresses. Development only |

## Email

See [File storage and email](storage-and-email.md#email) for what each driver needs.

| Key | Default | Description |
|---|---|---|
| `mail.driver` | *(email off)* | `smtp`, `sendgrid`, `mailgun`, `postmark`, `resend`, `ses` or `log`; setting it makes email read-only in the browser |
| `mail.from` | | Sender, e.g. `Gotalk <noreply@forum.example.com>` |
| `mail.smtp_host` / `smtp_port` / `smtp_tls` | / `587` / `starttls` | SMTP server; `smtp_tls` is `starttls`, `tls` or `none` |
| `mail.smtp_username` / `smtp_password` | | SMTP credentials |
| `mail.api_key` / `api_url` / `domain` | | API provider key, base URL override, Mailgun domain |
| `mail.region` / `access_key_id` / `secret_access_key` | | Amazon SES |

## Value formats

Rates use `<limit>-<period>` where period is `S`, `M`, `H`, or `D`. CORS origins may be `*`,
exact origins (`https://app.example.com`), origins with one wildcard (`https://*.example.com`) or
custom schemes. The desktop app's webview sends `tauri://localhost` (macOS and Linux) or
`http://tauri.localhost` (Windows), so allow those when restricting origins.
