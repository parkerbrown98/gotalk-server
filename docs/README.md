# Gotalk server documentation

[← Back to the README](../README.md)

Everything you need to run, configure, integrate with and contribute to the Gotalk server.

## On this page

- [Run it](#run-it)
- [Use the API](#use-the-api)
- [Contribute](#contribute)
- [Project planning](#project-planning)

## Run it

| Page | What's in it |
|---|---|
| [Getting started](getting-started.md) | Docker Compose quick start, headless setup, changing settings later, voice channels |
| [Commands and running the binary](cli.md) | Prebuilt binaries, building from source, every `gotalk` command |
| [Configuration](configuration.md) | Every setting, grouped by area, with defaults |
| [File storage and email](storage-and-email.md) | Local and S3-compatible storage, email providers, custom drivers |
| [Backup and restore](backup-and-restore.md) | Consistent backups, restores, moving between storage backends |
| [Deployment](deployment.md) | Kubernetes (Helm and Kustomize), cloud and multi-replica deployments, probes, reverse proxies |

## Use the API

Interactive docs are served by every instance at `/api/v1/docs`, with the OpenAPI 3.1 document
at `/api/v1/openapi.json`.

| Page | What's in it |
|---|---|
| [API overview](api-overview.md) | Connecting a client, authentication, password reset, errors and rate limits |
| [Instance and uploads](api-instance.md) | Instance settings, avatar and icon uploads, attachments, link previews |
| [Places, roles and boards](api-places.md) | Places, permissions, board overwrites, reading without an account |
| [Forum](api-forum.md) | Topics and posts, read state, feeds, search, drafts |
| [Chat and messaging](api-chat.md) | Channels, threads, messages, direct messages, presence |
| [Voice](api-voice.md) | Voice channels, moderation, call quality |
| [Moderation and policies](api-moderation.md) | Reports, bans, audit log, policies and consent, transparency |
| [Developers](api-developers.md) | API tokens, bots and slash commands, webhooks |
| [Real-time gateway](gateway.md) | WebSocket protocol, events, close codes |
| [Endpoint reference](api-reference.md) | Every endpoint grouped by area |

## Contribute

| Page | What's in it |
|---|---|
| [Development](development.md) | Tests, linting, schema changes, source layout |

## Project planning

| Page | What's in it |
|---|---|
| [Backend plan](backend-plan.md) | The full feature breakdown, technology stack and phases |
