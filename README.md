# Platform API

Control plane for isolated per-project Docker deploys.

## Run

Control-plane stores are dedicated loopback containers so they never collide with per-project Postgres/Redis sidecars:

- PostgreSQL (`deploy-control-db`) on `127.0.0.1:55432` — source of truth
- Redis (`deploy-control-redis`) on `127.0.0.1:56379` — production cache only, keys namespaced `aivex:v1:`

Set `PLATFORM_DATABASE_URL` and `REDIS_URL` in `backend/.env`. Docker must be available to the process user.

```bash
make run
```

```
GET  /healthz
GET  /v1/projects
POST /v1/projects
GET  /v1/projects/{id}
DELETE /v1/projects/{id}
POST /v1/projects/{id}/deploy/github  {"url":"owner/repo"}
POST /v1/projects/{id}/deploy/zip     multipart field: file
POST /v1/projects/{id}/deploy/html    multipart field: file
GET  /v1/projects/{id}/events         SSE logs
```

Each project gets its own Docker network and app container. PostgreSQL and Redis nodes start as isolated sidecars on that network, unpublished to the host.

Copy `backend/.env.example` to `backend/.env` and fill GitHub App values.

## GitHub App

Railway-style connect: install the app, pick a repo, deploy. Later `git push` hits the webhook and rebuilds that service.

1. Create an app at [GitHub Apps](https://github.com/settings/apps/new).
2. Repository permissions: **Contents: Read-only**, **Metadata: Read-only**. No account/OAuth permissions — Runex never signs users in with GitHub.
3. Subscribe to **Push**, **Installation**, and **Installation target** is optional; Runex also handles `installation_repositories`.
4. Do **not** enable “Request user authorization (OAuth) during installation”.
5. Setup URL: `{PLATFORM_PUBLIC_URL}/v1/github/setup` (tunnel the API on port 8080).
6. Webhook URL: `{PLATFORM_PUBLIC_URL}/v1/github/webhook`.
7. Paste App ID, slug, private key, and webhook secret into `.env`, then restart.

```
GET  /v1/github/status
GET  /v1/github/install?return=/projects/{id}
GET  /v1/github/setup
GET  /v1/github/repos?installationId=
POST /v1/github/webhook
POST /v1/projects/{id}/deploy/github  {"repo":"owner/repo","installationId":1,"branch":"main","autoDeploy":true}
```

