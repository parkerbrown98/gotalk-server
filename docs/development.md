# Development

[← Documentation](README.md)

## On this page

- [Requirements and tests](#requirements-and-tests)
- [Changing the schema](#changing-the-schema)
- [Layout](#layout)

## Requirements and tests

Requirements: Go 1.27+ and Docker (for integration tests and `sqlc`).

```sh
go test ./...                      # unit + integration tests (starts PostgreSQL/Redis containers)
go test -tags pushgateway -run TestRealPushGateway ./internal/api   # against a real push gateway container
golangci-lint run ./...            # lint, same config as CI
docker run --rm -v "$PWD:/src" -w /src sqlc/sqlc:1.30.0 generate   # after editing SQL
```

Integration tests skip automatically when Docker is unavailable. On Docker Desktop for
Windows, set `TESTCONTAINERS_RYUK_DISABLED=true` if container start-up times out; the
tests clean up their own containers. The `sqlc` release binaries for Windows, macOS and
Linux generate the same code without Docker.

The push gateway test runs `GOTALK_GATEWAY_IMAGE` (default
`ghcr.io/parkerbrown98/gotalk-gateway:edge`); to test a local gateway checkout, build it with
`docker build -t gotalk-gateway:dev .` there and set `GOTALK_GATEWAY_IMAGE=gotalk-gateway:dev`.
See [Push notifications](push.md#testing).

The public index tests validate responses against the index protocol's JSON Schemas, copied from
gotalk-platform into [internal/api/testdata/index-protocol](../internal/api/testdata/index-protocol). See its
README to refresh them when the protocol changes.

## Changing the schema

To change the schema, add a new numbered file to
[internal/database/migrations](../internal/database/migrations), add or edit queries in
[internal/store/queries](../internal/store/queries), and regenerate with `sqlc`. CI fails if the
generated code is out of date.

## Layout

| Path | Contents |
|---|---|
| `cmd/gotalk` | CLI entry point: serve, migrate, setup, check, backup, restore, healthcheck |
| `internal/api` | HTTP layer: chi router, huma operations, middleware, DTOs, WebSocket gateway, `/media` serving |
| `internal/service` | Business logic shared by the API and CLI (forum permissions are evaluated in `forum.go`, chat permissions in `channels.go`, voice state and LiveKit reconciliation in `voice.go`, webhook queueing and delivery in `webhooks.go`, provider settings and reloading in `providers.go`, pre-flight checks in `health.go`, the email outbox, password reset and verification in `mailflows.go`, uploads in `uploads.go`, attachments and link previews in `attachments.go`, the public index rules, derived index state and change feed in `index.go`, directory settings and keys in `directory.go`, index pings in `indexping.go`, push settings and the gateway registration in `push.go`, push subscriptions and preferences in `pushsubs.go`, the push outbox and payloads in `pushdelivery.go`) |
| `internal/realtime` | Gateway event routing (hub), Redis or in-memory event broker, and presence store |
| `internal/livekit` | Minimal LiveKit client: participant tokens, RoomService calls, webhook verification |
| `internal/storage` | Storage driver registry and the `local` and `s3` drivers |
| `internal/mail` | Mail driver registry and the `smtp`, `sendgrid`, `mailgun`, `postmark`, `resend`, `ses` and `log` drivers |
| `internal/sigv4` | AWS Signature V4 signing shared by the S3 and SES drivers |
| `internal/media` | Image validation and lossless metadata stripping |
| `internal/unfurl` | Link preview fetching (OpenGraph, Twitter cards, `<title>`) restricted to public addresses |
| `internal/backup` | Backup archives: consistent `COPY` dump plus media, verification and restore |
| `internal/store` | sqlc-generated, type-safe queries (do not edit by hand) |
| `internal/database` | Connection handling and embedded goose migrations |
| `internal/permissions` | Permission bits, role hierarchy, and board and channel overwrite rules |
| `internal/auth` | Argon2id passwords, JWT access tokens, refresh tokens, API tokens |
| `internal/config` | Layered configuration and validation |
| `internal/ratelimit` | Rate limit tiers backed by Redis or memory |
| `internal/indexsig` | `GOTALK-INDEX-SIG-V1` signing for pings to the public index |
| `internal/pushgw` | Push gateway client: `GOTALK-SIG-V1` request signing, RFC 8291 payload encryption, registration, pushes and device deletion |
| `internal/web` | Embedded landing page, setup wizard / settings page, password reset and email verification pages |
| `deploy/helm/gotalk`, `deploy/kubernetes` | Helm chart and Kustomize manifests |
