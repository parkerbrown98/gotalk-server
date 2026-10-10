# Plain Kubernetes manifests

This directory is a kustomize base for a typical cloud deployment:

- 2 Gotalk replicas;
- external managed PostgreSQL and Redis;
- S3-compatible media storage;
- nginx Ingress with long WebSocket idle timeouts;
- nightly backup CronJob using `gotalk backup --upload --keep 14`.

## Customize

Edit these before applying:

- `gotalk.env.example`: secret values consumed by `secretGenerator` (`GOTALK_DATABASE_URL`, `GOTALK_REDIS_URL`, JWT secret, S3 credentials, optional setup admin password, mail, and LiveKit credentials).
- `kustomization.yaml`: non-secret config such as S3 bucket/region/prefix, log level, and image tag.
- `ingress.yaml`: host, TLS secret, ingress class, and provider-specific annotations.
- `deployment.yaml`: resources, node placement, and replica count if needed.

The generated Secret is named `gotalk-env` because `disableNameSuffixHash` is set. If you prefer externally managed secrets, remove the `secretGenerator` and create a Secret with the same name and keys.

## Apply

From the repository root:

```sh
kubectl apply -k deploy/kubernetes
kubectl -n gotalk rollout status deploy/gotalk
kubectl -n gotalk logs deploy/gotalk
```

Open the setup link printed in the logs, or set `GOTALK_SETUP_ADMIN_USERNAME`, `GOTALK_SETUP_ADMIN_EMAIL`, and `GOTALK_SETUP_ADMIN_PASSWORD` in the generated Secret for headless setup.

## WebSockets and readiness

Gotalk serves the gateway at `/api/v1/gateway`. Your ingress must allow WebSocket upgrades and keep idle connections open for longer than the 30s heartbeat. The included nginx annotations set read/send timeouts to 3600 seconds.

`/healthz` is the liveness probe. `/readyz` is readiness and returns HTTP 503 when PostgreSQL or Redis is unreachable.

## Storage and backups

These manifests assume `GOTALK_STORAGE_DRIVER=s3`. Backups write temporary files under `/tmp`, so both the Deployment and CronJob mount `/tmp` as an `emptyDir` while using a read-only root filesystem.

If you switch to local storage, add a shared ReadWriteMany PVC mounted at `/data` to both the Deployment and backup CronJob when running multiple replicas.
