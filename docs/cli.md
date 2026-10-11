# Commands and running the binary

[← Documentation](README.md)

Install a prebuilt binary or build from source, then use the `gotalk` command line.

## On this page

- [Install](#install)
- [Commands](#commands)

## Install

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

## Commands

| Command | Purpose |
|---|---|
| `gotalk serve` | Run the server (default when no command is given). Logs one pre-flight line per dependency at boot |
| `gotalk migrate` / `gotalk migrate status` | Apply migrations / show schema version |
| `gotalk setup [--reset] [--reset-settings] [--skip-checks]` | Complete first-run setup from config/env, then exit (see [headless setup](getting-started.md#skip-the-browser-headless-setup)) |
| `gotalk check` | Run the pre-flight checks (database, Redis, public URL, storage, email, voice) with a fix-it hint per problem; exits 1 if any fails |
| `gotalk backup [--output FILE\|-] [--no-media] [--upload [--keep N]]` | Write a backup archive; see [Backup and restore](backup-and-restore.md) |
| `gotalk backup list` | List backups stored in the storage backend |
| `gotalk restore --input FILE\|- \| --from-storage NAME [--force] [--no-media]` | Restore a backup |
| `gotalk healthcheck` | Exit 0 if the local server is healthy (used by the image's `HEALTHCHECK`) |
| `gotalk version` | Print the version |

All commands accept `--config path/to/gotalk.yaml` (alias `--from-file`). See
[Configuration](configuration.md) for the file format and environment variables.
