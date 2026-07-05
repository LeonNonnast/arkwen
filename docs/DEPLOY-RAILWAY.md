# Deploying Arkwen to Railway

Arkwen ships a single container that serves **two protocols on one port** (`$PORT`),
multiplexed with [cmux](https://github.com/soheilhy/cmux):

| Protocol | What | How you reach it on Railway |
| --- | --- | --- |
| **gRPC (HTTP/2)** | The `ReadPlane` + `CommandPlane` — the consumer contract | Railway **TCP Proxy** (raw passthrough preserves HTTP/2) |
| **HTTP/1.1** | `/healthz`, `/readyz`, and a zero-run-data landing page — operational only | Railway **HTTP edge** (`https://<app>.up.railway.app`) |

> **Why the split?** Railway's HTTP edge terminates TLS and speaks **HTTP/1.1** to
> your container — it does not forward end-to-end HTTP/2, so gRPC cannot ride the
> `*.up.railway.app` domain. gRPC must use the **TCP proxy**, which is raw TCP
> passthrough. The HTTP healthcheck reaches `/healthz` over the edge on `$PORT`;
> gRPC reaches the same `$PORT` over the TCP proxy. Both work simultaneously.

The event store is **PostgreSQL** when `DATABASE_URL` is set (Railway's managed
Postgres injects it), and in-memory otherwise. Nothing else changes — the store is
a drop-in behind the `eventlog.Log` interface.

---

## 1. Prerequisites

- A Railway account and a GitHub repo containing this project (or the Railway CLI).
- Nothing else — the build is a committed `Dockerfile`; `railway.json` selects it
  (`"builder": "DOCKERFILE"`), so Nixpacks is off automatically.

## 2. Create the service

1. **New Project → Deploy from GitHub repo** (root directory = repo root).
2. **Add a Postgres database**: *New → Database → PostgreSQL*. Railway injects
   `DATABASE_URL` into your service automatically once you reference it (or add a
   variable `DATABASE_URL = ${{Postgres.DATABASE_URL}}`).

## 3. Set service variables

| Variable | Meaning | Default | Set on Railway? |
| --- | --- | --- | --- |
| `PORT` | Listen port (Railway injects it). Pin it so the bind, TCP-proxy port, and healthcheck agree. | `7777` | **Pin `8080`** |
| `DATABASE_URL` | Postgres DSN → durable append-only event store. Absent ⇒ in-memory. | *(empty ⇒ in-mem)* | via the Postgres plugin |
| `ARKWEN_OPERATOR_TOKEN` | Command-plane bearer credential. **Unset on a public bind ⇒ command plane is SEALED** (closed to all). Set it to open the command plane. | *(sealed on public)* | set a **secret** to open commands |
| `ARKWEN_ALLOW_INSECURE_PUBLIC` | Acknowledge that a provisioned token crosses the **plaintext** TCP proxy. Required alongside `ARKWEN_OPERATOR_TOKEN` on a public bind — the app **refuses to start** otherwise (fail-closed). | `false` | `1` to open commands over plaintext |
| `ARKWEN_DEFAULT_WORKER` | Worker kind for runs with no `worker_kind` label. | `claude-code` | optional |
| `ARKWEN_AUTODRIVE` | Drive runs autonomously on enqueue. | `true` | optional |
| `ARKWEN_REQUIRE_OPERATOR_TOKEN` | Fail-fast at boot if the token is unset (strict prod). | `false` | optional |
| `RAILWAY_DEPLOYMENT_DRAINING_SECONDS` | SIGTERM→SIGKILL grace window (Railway default 0). The app drains in ≤15 s. | `0` | **set `25`** |

The app **never logs** `ARKWEN_OPERATOR_TOKEN` or the model key (Invariant 5). Only a
non-secret token *mode* (`sealed`/`provisioned`/`dev-fallback`) is printed.

## 4. Networking

- **Enable the TCP Proxy** (*Settings → Networking → TCP Proxy*), internal port =
  your pinned `PORT` (`8080`). Railway gives you `RAILWAY_TCP_PROXY_DOMAIN:PORT`
  (e.g. `tramway.proxy.rlwy.net:41234`) — **that host:port is your gRPC endpoint.**
- Optionally **Generate Domain (HTTP)** for a browser-reachable `/healthz` and the
  status page. The deploy healthcheck does **not** depend on it — Railway probes the
  container directly on `$PORT`, which cmux answers over HTTP/1.1.

## 5. Deploy & verify

The `railway.json` sets `healthcheckPath: /healthz`, `numReplicas: 1`,
`restartPolicyType: ON_FAILURE`. On deploy Railway waits for `/healthz` → 200.

```bash
# HTTP edge (browser or curl): liveness + status
curl https://<app>.up.railway.app/healthz          # -> ok
curl https://<app>.up.railway.app/                 # -> identity + store + command-plane mode

# gRPC over the TCP proxy: drive a run end-to-end (needs ARKWEN_OPERATOR_TOKEN set)
arkwen ctl run \
  --addr <RAILWAY_TCP_PROXY_DOMAIN>:<PORT> \
  --token "$ARKWEN_OPERATOR_TOKEN" \
  --mission "build me a thing"
# -> streams the event log to RUN_FINISHED, prints RUN_METRICS (COMPLETED)
```

Because `DATABASE_URL` is set, the run's append-only stream is persisted in Postgres
and survives redeploys/restarts.

## 6. Security posture (read this)

- **Sealed by default.** With no `ARKWEN_OPERATOR_TOKEN`, a public deploy binds **no**
  credential: every command-plane RPC returns `Unauthenticated`. `/healthz` and the
  read-only status page still serve. This is the fail-closed default (Invariant 7) —
  the world-known dev token is bound **only** on a loopback bind.
- **Opening the command plane** = set `ARKWEN_OPERATOR_TOKEN` to a strong secret
  **and** `ARKWEN_ALLOW_INSECURE_PUBLIC=1`, then redeploy. ⚠️ Without the second
  variable the app **refuses to start** (fail-closed): it will not silently put a
  live credential on a plaintext public socket. The `=1` is your explicit
  acknowledgement that the token travels **in cleartext** over the public TCP proxy
  (Railway's TCP proxy does not terminate TLS). For anything beyond a demo:
  - prefer **Railway Private Networking** (`<service>.railway.internal:PORT`, full
    HTTP/2, no proxy) if your consumer runs in the same project; or
  - layer **gRPC server TLS** via the existing `grpc.ServerOption` seam in
    `internal/controlplane/serve.go` (needs a cert for the proxy domain).
- **Single replica.** Keep `numReplicas: 1`. Each replica runs its own in-memory
  controller projections + autodrive; multiple replicas would double-drive runs even
  with a shared Postgres log (Invariant 2's single-projection assumption).

## 7. Local dry-run (mirrors Railway)

```bash
make docker-build
# in-memory + sealed (public bind, no token):
docker run --rm -e PORT=8080 -p 8080:8080 arkwen:local
# postgres-backed + open command plane (point at your own PG):
docker run --rm -e PORT=8080 \
  -e DATABASE_URL='postgres://…' \
  -e ARKWEN_OPERATOR_TOKEN='dev-token' \
  -e ARKWEN_ALLOW_INSECURE_PUBLIC=1 \
  -p 8080:8080 arkwen:local
curl localhost:8080/healthz
```
