# Backup and restore

[← Documentation](README.md)

## On this page

- [Backing up](#backing-up)
- [Restoring](#restoring)
- [Security and scheduling](#security-and-scheduling)

## Backing up

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

## Restoring

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

## Security and scheduling

Archives contain everything in the database, including password hashes and secrets saved
in the settings page; store them accordingly. In Kubernetes, the Helm chart's `backup`
values schedule `backup --upload` as a CronJob (see [Deployment](deployment.md#kubernetes)).
