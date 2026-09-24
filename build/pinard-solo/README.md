# pinard-solo

Single Docker image bundling all services that the k8s cluster provides, for
local / solo-mode operation:

| Service | Port | Description |
|---------|------|-------------|
| NATS | 4222 | JetStream message bus (file-backed) |
| engram | 7437 | Memory backend (SQLite) |
| SurrealDB | 8000 | Knowledge graph (surrealkv) |
| memory-ingester | — | Ingest + recall + rollup (internal) |
| webterm-gateway | 8080 | Web terminal gateway |

Supervisor: **s6-overlay v3**. Startup order: nats → engram + surrealdb →
memory-ingester → webterm-gateway.

## Build

```bash
# From repo root:
cd build/pinard-solo

make fetch-binaries   # download nats-server, surreal, engram binaries
make image            # docker build (tags as pinard-solo:latest)
make image-push       # push to ECR
```

Override the tag: `make image IMAGE_TAG=0.1.0`.

## Run

```bash
docker run -d \
  -v $(pwd)/data:/data \
  -e SURREAL_PASS=changeme \
  -e NATS_VIGNOBLE=myvig \
  -p 4222:4222 \
  -p 7437:7437 \
  -p 8000:8000 \
  -p 8080:8080 \
  pinard-solo:latest
```

## Environment variables

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `SURREAL_PASS` | **yes** | — | SurrealDB root password |
| `NATS_VIGNOBLE` | **yes** | — | Vignoble name for memory services |
| `SURREAL_USER` | no | `root` | SurrealDB root username |
| `ENGRAM_PORT` | no | `7437` | engram listen port |
| `ENGRAM_DATA_DIR` | no | `/data/engram` | engram SQLite storage dir |
| `WEBTERM_PORT` | no | `8080` | webterm-gateway listen port |
| `NATS_URL` | no | `nats://127.0.0.1:4222` | NATS URL for memory services |
| `SURREAL_URL` | no | `http://127.0.0.1:8000` | SurrealDB URL for memory services |
| `ENGRAM_URL` | no | `http://127.0.0.1:7437` | Engram URL for memory services |
| `MEMORY_LLM_API` | no | — | LLM adapter (`anthropic-messages` / `openai-chat`) |
| `ANTHROPIC_API_KEY` | no | — | Direct Anthropic key (if using `static-key` auth) |
| `MEMORY_LLM_AUTH` | no | — | Token source: `static-key` / `url` / `google-sa` |

No secrets are baked into the image. Provide them via `-e` or a mounted `.env`
file sourced at startup.

## Volume layout

All persistent state lives under `/data` (mount a host directory or Docker
volume here so restarts resume from the same state):

```
/data/
  nats/jetstream/    ← NATS JetStream store
  engram/            ← engram SQLite database
  surreal.db         ← SurrealDB surrealkv store
```

## Pointing aoc at local services

In your `~/.config/pinard/credentials.yaml` (or the vignoble `credentials.yaml`):

```yaml
nats:
  url: nats://127.0.0.1:4222

engram:
  # no server: → local-only (engram serve is managed by the daemon)
```

Set `SURREAL_URL=http://127.0.0.1:8000` and memory service env vars as needed
before starting `aoc`.
