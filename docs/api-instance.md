# Instance and uploads

[← API overview](api-overview.md) · [Documentation](README.md)

## On this page

- [Instance settings (administrators)](#instance-settings-administrators)
- [Uploads](#uploads)
- [Attachments](#attachments)
- [Link previews](#link-previews)

## Instance settings (administrators)

`GET /instance/config` shows storage, email, voice and
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

## Uploads

Avatars (`PUT /users/@me/avatar`), place icons and banners
(`PUT /places/{place}/icon`, `/banner`, `MANAGE_PLACE`) and the instance icon
(`PUT /instance/icon`, administrators) take the raw image as the request body: PNG, JPEG, GIF
or WebP up to `limits.upload_size` bytes and `limits.upload_max_side` pixels per side. EXIF,
XMP and text metadata (such as a photo's GPS position) are stripped without re-encoding the
image. The response carries the new URL; `DELETE` on the same paths removes the image. Replaced
files are deleted right away, and files nothing refers to any more (for example after an
account deletion) after about an hour. URLs set directly with `PATCH` keep working.

## Attachments

Messages, new topics and replies take up to `limits.attachments` (10) files.
Upload each with `POST /attachments?filename=…`, the file as the raw body with its
`Content-Type`, then pass the returned IDs as `attachment_ids` within an hour (unsent uploads
are deleted, as are the files of deleted messages). PNG, JPEG, GIF and WebP images are stripped
of metadata and come back with `width` and `height`; any other file is kept as sent and served
only as a download (`Content-Disposition: attachment`, in a sandbox). With files, `content` may
be empty. `Message` and `Post` carry `attachments`, and feed items the opening post's first four
`images` and `image_count`.

## Link previews

Controlled by `features.link_previews` (on by default) and `embeds.enabled`. After a message or
post is saved, the server fetches up to five of its links (skipping code and `<url>`-wrapped
links) and keeps their OpenGraph, Twitter card or `<title>` metadata for a week. The preview
image is copied into storage, so readers never contact the linked site; a link straight to an
image becomes an image preview. `Message` and `Post` list them in `embeds` (a chat message is
re-sent as `MESSAGE_UPDATE` once they arrive), and feed items carry the opening post's first as
`embed`. Fetches only reach public addresses, checked after DNS resolution and on every
redirect, with a 10-second timeout and 1 MiB of HTML.

See [Uploads and link previews](configuration.md#uploads-and-link-previews) for the related
configuration keys.
