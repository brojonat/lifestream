# Lifestream

A personal event store with an agent that files what you send it. Text
`weight 175` to a Twilio number; an agent labels it into the `weight`
collection, extracts the value, and a live dashboard charts it with a weekly
average. The spreadsheet goes away.

This is **phase 1** (the personal tracker). The product it grows into, and
every design decision behind it, is in [VISION.md](VISION.md). Work in
flight is in [TODO.md](TODO.md); pitfalls are in [LEARNINGS.md](LEARNINGS.md).
"Lifestream" is a placeholder name.

## How it fits together

Two processes in one pod, sharing a volume:

- **Go service** (`cmd/lifestream`, this module). Owns the event store
  (SQLite, append-only, hash-chained per owner), ingest, projections, the
  dashboard (Datastar over SSE), and the dispatcher that hands raw events
  to the agent.
- **Agent** (`agent/`, TypeScript, Pi Durable). One long-lived conversation
  per owner. It reads events and writes derived events back **only**
  through the Go service's internal API. It never touches Go's database.

Raw events are never changed. Everything the agent produces
(`collection.*`, `label.applied`, `metric.observed`, ...) is a derived event
pointing at its parent, and every view is a projection that can be rebuilt
from the events.

## Run it

[mise](https://mise.jdx.dev/) is the front door; `mise tasks` lists
everything.

```
mise install          # pins Go
mise run setup        # go mod tidy, install air
mise run dev          # hot reload, http://127.0.0.1:8080, tees logs/serve.log
mise run dev-agent    # the agent, tees logs/agent.log
mise run test
mise run lint
mise run build        # ./bin/lifestream
```

Tests use temp dirs only and never touch a real data dir.

## CLI

```
lifestream serve                 # run the service
lifestream users add --name me --phone +15555550100
                                 # allowlist a user; prints {id, token} as JSON
lifestream users ls              # JSONL
echo '{"type":"note","payload":{"text":"hi"}}' | lifestream push
                                 # POST JSON events from stdin to $LIFESTREAM_URL
lifestream rebuild               # drop and rebuild every projection from events
lifestream verify                # check every owner's hash chain
```

`-h` works on every command.

## HTTP

Public listener (`--addr`, what the ingress exposes):

| Route | What |
| --- | --- |
| `POST /twilio/sms` | Twilio webhook. Verifies `X-Twilio-Signature`, drops unknown senders, appends `message.received`, answers empty TwiML. Never waits on the agent. |
| `POST /events` | Generic JSON events, `Authorization: Bearer <user token>`. One object, a JSON array, or JSONL. |
| `GET /` | Metrics: per metric collection, latest / 7-day average / change vs the week before, and a chart of readings with a trailing 7-day average (live over SSE) |
| `GET /inbox` | The last 50 raw events and what each became (labels, values, flags); flagged ones highlighted |
| `GET /collections`, `GET /c/{id}` | Collections; one collection's definition history (each version) and members |
| `POST /c/{id}/remove`, `POST /inbox/add` | Owner corrections: append `label.corrected` (source `dashboard`), which goes to the agent like any raw event |
| `GET /log`, `POST /log/{id}/undo` | Rule log: every `collection.*` change, with one-click undo (appends the inverse: stop, resume, or `collection.amended {remove_rule}` / re-add) |
| `POST /review/{id}/answer` | Answer a `review.flagged` question (queue at the top of the inbox); appends `review.answered`, which goes to the agent |
| `GET /rules`, `POST /rules`, `POST /rules/{id}/remove` | The rules doc: the owner's standing instructions (`rule.added` / `rule.removed`), which the agent reads before every event |
| `GET /admin` | Process diagnostics |
| `GET /healthz` | `ok` |
| `GET /metrics` | Prometheus text format |

Until login exists (TODO 24) the dashboard serves the only user and
answers 503 if there are several, so **don't expose it through the ingress
yet**; use `kubectl port-forward`.

Internal listener (`--api-addr`, reachable only inside the pod; the agent's
side of the contract):

| Route | What |
| --- | --- |
| `GET /api/collections?owner=&status=` | Current collection definitions with their rules |
| `GET /api/collections/{id}/members` | Members, each with the definition version it was labeled under |
| `GET /api/rules?owner=` | The rules doc |
| `GET /api/events?owner=&since=&after=&limit=&max_payload=` | Events in log order; payloads over `max_payload` bytes (default 4096) are elided, with `payload_size` |
| `GET /api/events/{id}/children` | Events derived from it |
| `GET /api/events/{id}` | One event; `?offset=&limit=` returns a byte range of a large payload as `data` |
| `POST /api/events` | Append a derived event `{owner, type, parent_id?, payload}`. `Idempotency-Key` header = the Pi Durable tool *task* id (`api.taskId`; provider tool-call ids aren't unique). 201 new, 200 replay, 422 when it contradicts current state (unknown collection, closed collection, bad version). |
| `GET /healthz` | `ok` |

The agent may append `collection.started|amended|stopped|resumed`,
`label.applied`, `label.removed`, `metric.observed`, `rule.added`, `review.flagged` and
`redaction.created`. Payload shapes are the structs in
`internal/store/projections.go`; a collection's id is the id of its
`collection.started` event.

## Configuration

Environment variables only (mise loads `.env` if present; never commit it).

| Variable | Used by | Meaning |
| --- | --- | --- |
| `LIFESTREAM_ADDR` | serve | Public listen address (default `127.0.0.1:8080`) |
| `LIFESTREAM_API_ADDR` | serve | Internal API listen address (default `127.0.0.1:8081`) |
| `LIFESTREAM_DATA_DIR` | serve, users, rebuild, verify | Where `lifestream.db` and the bus live (default `~/.lifestream`) |
| `LIFESTREAM_PUBLIC_URL` | serve | Base URL as Twilio sees it, for signature checks |
| `TWILIO_AUTH_TOKEN` | serve | Verifies Twilio webhooks; `/twilio/sms` is off without it |
| `LIFESTREAM_AGENT_URL` | serve | Where the dispatcher POSTs raw events (unset: no dispatch) |
| `LIFESTREAM_URL` | push | Base URL of the service |
| `LIFESTREAM_TOKEN` | push | A user token from `users add` |
| `LOG_LEVEL` | all | `debug`, `info`, `warn`, `error` |
| `LOG_FORMAT` | all | `json` for machine-readable logs |

### Agent (`agent/`)

Node ≥ 22.19 (mise pins 25), no build step: `node src/main.ts`. Pi Durable
1.3.0, pinned exactly (it is experimental and has broken its API twice).

| Variable | Meaning |
| --- | --- |
| `AGENT_ADDR` | Listen address for `POST /submit` and `GET /healthz` (default `127.0.0.1:8082`); point `LIFESTREAM_AGENT_URL` here |
| `LIFESTREAM_API_URL` | Go's internal API (default `http://127.0.0.1:8081`) |
| `DATA_DIR` | Where `agent.db` lives (default `~/.lifestream`) |
| `AGENT_MODEL` | `provider/modelId` (default `anthropic/claude-haiku-4-5-20251001`); stored per conversation when it is created |
| `ANTHROPIC_API_KEY` | Read by the Anthropic provider |

One conversation per owner. Each event is submitted with its id as
`requestId` (dedup survives restarts); SMS and dashboard events use
`whenBusy: "steer"`. Two extensions: `collections` (eight tools that write
derived events through Go's API; sections `role` and `collections`, the
open definitions) and `rules` (`add_rule`; section `owner_rules`, the rules
doc). Sections are fetched before every model request and rendered
deterministically, so the prompt cache only breaks when they change.
Starting a collection or adding an include runs a look-back: a background
task (`lifestream.lookback`) that labels the last 7 days (`lookback_days`
overrides; 0 skips) in batches of 20, one model call per batch,
checkpointing as it goes and retrying with backoff before giving up. An
exclude re-checks current members and emits `label.removed`.
Every labeled event also gets one redacted copy (`redaction.created`,
parent = the event) from a background task (`lifestream.redact`): the
deterministic scanner (`agent/src/scanner.ts`; corpus in
`testdata/secrets.json`) masks keys, tokens, private keys, connection
passwords, emails and phone numbers, then the model reviews what's left in
12 KB chunks (up to 10) against the rules doc. The model never sees what
the scanner caught. Payloads over 4 KB reach the conversation as pointers. Tests use pi-ai's faux provider;
`test/e2e.test.ts` drives the real Go binary.

## Deploy

One pod in namespace `lifestream`: the Go service, the agent and a
Litestream sidecar sharing a PVC at `/data`. One replica, `Recreate`, since
SQLite has one writer. An init container restores both databases from the
replicas when the volume is empty. The agent's API stays on the pod's
loopback; the Ingress exposes only `/twilio/sms` and `/events` until login
lands (TODO 24). Use `kubectl -n lifestream port-forward deploy/lifestream
8080` for the dashboard.

```
kubectl create namespace lifestream
kubectl -n lifestream create secret generic lifestream-env --from-env-file=.env.prod
#   TWILIO_AUTH_TOKEN, ANTHROPIC_API_KEY, LITESTREAM_BUCKET, LITESTREAM_ENDPOINT,
#   LITESTREAM_ACCESS_KEY_ID, LITESTREAM_SECRET_ACCESS_KEY
DOCKER_REPO=registry.example.com/me LIFESTREAM_HOST=ls.example.com mise run deploy
kubectl -n lifestream exec deploy/lifestream -c lifestream -- /lifestream users add --data-dir /data --name me --phone +1555…
```

Then point the Twilio number's messaging webhook at
`https://$LIFESTREAM_HOST/twilio/sms` (HTTP POST).

## Layout

```
cmd/lifestream/      CLI and wiring
internal/store/      SQLite: events (append-only, hash chain), users, dispatch state, projections
internal/ingest/     /twilio/sms and /events
internal/api/        the agent's internal API
internal/dispatch/   delivers raw events to the agent
internal/bus/        embedded NATS (in-process), backs the diagnostics samples
internal/metrics/    process metrics, /metrics
internal/web/        dashboard handlers, templates, assets/components.js (d3 + Rocket)
agent/src/           the Pi Durable agent: main, agent (harness, per-owner conversations),
                     collections (extension: tools + sections), goapi, server
agent/test/          faux-model tests, scanner tests, and the end-to-end test
testdata/secrets.json  fake secrets shared by every scanner (agent now, capture adapters later)
deploy/              deploy.sh and k8s/ (kustomize; Deployment, PVC, Service, Ingress, Litestream config)
```
