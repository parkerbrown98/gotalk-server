# Deployment

[← Documentation](README.md)

## On this page

- [Kubernetes](#kubernetes)
- [Cloud and multi-replica deployments](#cloud-and-multi-replica-deployments)

## Kubernetes

- **Helm:** [deploy/helm/gotalk](../deploy/helm/gotalk) (see its [README](../deploy/helm/gotalk/README.md)).
  It supports managed PostgreSQL/Redis via URLs or existing Secrets, S3 or a PVC for storage,
  every mail driver, LiveKit, an Ingress, autoscaling, a PodDisruptionBudget, a backup CronJob,
  a hardened pod (non-root, read-only root filesystem with an `emptyDir` at `/tmp` for
  backups), and an optional built-in PostgreSQL and Redis for evaluation. It refuses
  configurations that would break at runtime (no database, several replicas with unshared
  local storage or without Redis).
- **Plain manifests:** [deploy/kubernetes](../deploy/kubernetes), a Kustomize base for two
  replicas behind an Ingress with managed PostgreSQL, Redis and S3, configured from a
  `gotalk.env` file turned into a Secret.

Both pass `helm lint`/`kubeconform` in CI. For the gateway WebSocket, raise the ingress
idle timeout above the 30 second heartbeat (for ingress-nginx,
`nginx.ingress.kubernetes.io/proxy-read-timeout` and `proxy-send-timeout`). The setup link is
printed in the pod log (`kubectl logs deploy/gotalk`), or set the `setup.admin` values for
headless setup.

## Cloud and multi-replica deployments

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

See also: [Configuration](configuration.md), [File storage and email](storage-and-email.md) and
[Backup and restore](backup-and-restore.md).
