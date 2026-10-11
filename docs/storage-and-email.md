# File storage and email

[← Documentation](README.md)

Both are plugins chosen by name, configured in the setup wizard, the settings page, the
config file or the environment.

## On this page

- [Storage](#storage)
- [Email](#email)
- [Custom drivers](#custom-drivers)

## Storage

Storage keeps uploaded images and attachments, link preview images (and backups made with
`backup --upload`):

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

The `storage.*` keys are listed in [Configuration](configuration.md).

## Email

Email sends password reset links and email verification:

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

The `mail.*` keys are listed in [Configuration](configuration.md).

## Custom drivers

Third-party drivers can be added in Go with `storage.Register` / `mail.Register` (see
[internal/storage](../internal/storage) and [internal/mail](../internal/mail)).
