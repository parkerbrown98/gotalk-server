# Gotalk Helm chart

This Helm v3 chart deploys the Gotalk server image (`ghcr.io/parkerbrown98/gotalk-server`) with production-oriented defaults: a non-root distroless container, read-only root filesystem, `/tmp` `emptyDir` for backup temp files, liveness/readiness/startup probes, optional ingress, optional HPA/PDB, and an optional backup CronJob.

## Quick evaluation

For a throwaway cluster evaluation, enable the built-in PostgreSQL and Redis components:

```sh
helm install gotalk ./deploy/helm/gotalk \
  --namespace gotalk --create-namespace \
  --set postgresql.enabled=true \
  --set redis.enabled=true
```

Then get the browser setup link from logs:

```sh
kubectl -n gotalk logs deploy/gotalk
```

The bundled PostgreSQL is intentionally minimal and is not recommended for production.

## Production example

Use managed PostgreSQL and Redis, S3-compatible storage, TLS at your ingress, and a Kubernetes Secret for sensitive values:

```sh
kubectl -n gotalk create secret generic gotalk-env \
  --from-literal=GOTALK_DATABASE_URL='postgres://gotalk:REDACTED@postgres.example.com:5432/gotalk?sslmode=require' \
  --from-literal=GOTALK_REDIS_URL='rediss://:REDACTED@redis.example.com:6380/0' \
  --from-literal=GOTALK_AUTH_JWT_SECRET='replace-with-at-least-32-random-characters' \
  --from-literal=GOTALK_STORAGE_S3_ACCESS_KEY_ID='REDACTED' \
  --from-literal=GOTALK_STORAGE_S3_SECRET_ACCESS_KEY='REDACTED' \
  --from-literal=GOTALK_SETUP_ADMIN_PASSWORD='change-me-please'

helm upgrade --install gotalk ./deploy/helm/gotalk \
  --namespace gotalk --create-namespace \
  --set existingSecret=gotalk-env \
  --set replicaCount=2 \
  --set storage.driver=s3 \
  --set storage.s3.bucket=gotalk-media \
  --set storage.s3.region=us-east-1 \
  --set server.publicUrl=https://forum.example.com \
  --set setup.admin.username=admin \
  --set setup.admin.email=admin@example.com \
  --set ingress.enabled=true \
  --set ingress.className=nginx \
  --set ingress.hosts[0].host=forum.example.com
```

For nginx ingress and the `/api/v1/gateway` WebSocket, set idle timeouts above Gotalk's 30s heartbeat, for example:

```yaml
ingress:
  annotations:
    nginx.ingress.kubernetes.io/proxy-read-timeout: "3600"
    nginx.ingress.kubernetes.io/proxy-send-timeout: "3600"
```

## Configuration and secrets

Every Gotalk setting can be provided as a `GOTALK_*` environment variable. This chart supports:

- `existingSecret`: an envFrom-friendly Secret containing any sensitive `GOTALK_*` keys.
- Dedicated existing secrets for database, Redis, auth, setup admin password, S3, mail, and LiveKit credentials.
- Chart-managed Secrets when sensitive values are provided directly in `values.yaml` and `existingSecret` is not set.
- `extraEnv` and `extraEnvFrom` for advanced settings.

The chart validates these unsafe combinations with clear Helm `fail` messages:

- no PostgreSQL configured (`database.url`, `database.existingSecret.name`, or `postgresql.enabled=true`);
- multiple replicas/autoscaling with local storage unless `persistence.accessModes` includes `ReadWriteMany`;
- multiple replicas/autoscaling without Redis.

## Local storage persistence

When `storage.driver=local` (the default), `/data` is mounted from a PVC by default:

```yaml
persistence:
  enabled: true
  size: 10Gi
  storageClass: ""
  accessModes: [ReadWriteOnce]
```

For more than one replica, use S3 storage or a ReadWriteMany PVC:

```yaml
storage:
  driver: local
persistence:
  accessModes: [ReadWriteMany]
```

## Backups

Enable the CronJob to run `/gotalk backup --upload --keep <N>`:

```yaml
backup:
  enabled: true
  schedule: "0 3 * * *"
  keep: 14
```

The backup job uses the same environment as the app. Uploaded backups require configured storage. If using local storage, the backup job must mount the same ReadWriteMany PVC as the application so media files are available.

Manual commands can be run with a temporary pod or job using the same env, for example `backup list`, `restore --from-storage NAME`, or `restore --input -`.

## Upgrading

```sh
helm repo update
helm upgrade gotalk ./deploy/helm/gotalk -n gotalk -f values.yaml
```

Gotalk runs database migrations under a PostgreSQL advisory lock, so rolling updates and multiple replicas can safely start together. Keep `terminationGracePeriodSeconds` greater than `server.shutdown_timeout` (20s by default); the chart default is 30s.

Before changing storage backends, take a backup and confirm media files are available in the new backend. For image upgrades, set `image.tag` or leave it empty to use `.Chart.AppVersion`.
