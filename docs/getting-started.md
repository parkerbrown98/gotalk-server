# Getting started

[← Documentation](README.md)

Run a Gotalk instance with Docker Compose, finish setup in the browser (or headlessly), and
optionally turn on voice.

## On this page

- [Quick start (Docker Compose)](#quick-start-docker-compose)
- [Skip the browser (headless setup)](#skip-the-browser-headless-setup)
- [Changing settings later](#changing-settings-later)
- [Voice channels](#voice-channels)
- [Next steps](#next-steps)

## Quick start (Docker Compose)

```sh
docker compose up -d
docker compose logs gotalk     # copy the setup link it prints
```

Open the link (`http://localhost:8080/setup?token=…`), and the wizard walks you through
pre-flight checks, naming your instance, the administrator account, file storage (local disk
or S3-compatible), email (or "skip for now") and optional voice. Each step can be tested live
(the email step sends you a test message) before you finish. That's it.

For anything beyond trying it out locally, copy [.env.example](../.env.example) to `.env` and
set at least `POSTGRES_PASSWORD` and `GOTALK_SERVER_PUBLIC_URL`.

To try email without a real provider, start the bundled [Mailpit](https://mailpit.axllent.org)
and read the messages at <http://localhost:8025>:

```sh
GOTALK_MAIL_DRIVER=smtp GOTALK_MAIL_SMTP_HOST=mailpit GOTALK_MAIL_SMTP_PORT=1025 \
GOTALK_MAIL_SMTP_TLS=none GOTALK_MAIL_FROM="Gotalk <noreply@localhost>" \
docker compose --profile mail up -d
```

## Skip the browser (headless setup)

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

See [Commands](cli.md) for every CLI command and [Configuration](configuration.md) for the
`setup.*` keys.

## Changing settings later

Once set up, `/setup` becomes the instance settings page: sign in as an instance
administrator to change file storage, email, voice and CORS, test them (the email section
sends you a test message) and see live health. The same is available through the API
(`GET/PATCH /instance/config`, see [Instance and uploads](api-instance.md#instance-settings-administrators)).
Settings saved this way are stored in the database and apply to every replica within 15
seconds, without a restart.

A section whose enabling key is set in the config file or environment (`storage.driver`,
`mail.driver`, `voice.livekit_url`, `server.cors_allowed_origins`) is managed there and shown
read-only in the browser; that is how infrastructure-as-code deployments keep settings in one
place.

## Voice channels

Voice and video need a [LiveKit](https://livekit.io) server, which routes the media. The
Compose file includes one behind the `voice` profile, already set up to send its webhooks to
Gotalk:

```sh
GOTALK_VOICE_LIVEKIT_URL=ws://localhost:7880 docker compose --profile voice up -d
```

That works for clients on the same machine. For others, set `GOTALK_VOICE_LIVEKIT_URL` to an
address they can reach (`wss://…` when served over TLS), `LIVEKIT_NODE_IP` to the host's
public IP, open ports 7880/tcp, 7881/tcp and 7882/udp, and change `LIVEKIT_API_SECRET`. To use
an existing or managed LiveKit deployment instead, set the `voice.*` settings from
[Configuration](configuration.md) and point its webhook at
`https://<your instance>/api/v1/voice/webhook`, signed with the same API key.
Webhooks are optional: without them Gotalk polls LiveKit every 30 seconds.

## Next steps

- Running without Docker: [Commands and running the binary](cli.md)
- Pick storage and an email provider: [File storage and email](storage-and-email.md)
- Go to production: [Deployment](deployment.md) and [Backup and restore](backup-and-restore.md)
- Tune everything: [Configuration](configuration.md)
