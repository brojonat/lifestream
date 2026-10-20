# Lifestream

A personal event store with a durable agent that turns what you send it
into data. Text `weight 175` to a Twilio number; the agent files it into
the `weight` collection as a reading, and a live dashboard charts it with a
weekly average. Text "ran 5k in 31 minutes" and it keeps a `workout`
record. Text "the 176 yesterday was 177" and it corrects the reading.

The agent is a data handler first: it receives every event and turns it
into structured data, or leaves it stored. It never sells, publishes or
texts back. The design is in [VISION.md](VISION.md), work in flight in
[TODO.md](TODO.md), pitfalls in [LEARNINGS.md](LEARNINGS.md).
"Lifestream" is a placeholder name.

**Status (2026-10-20): live.** Texts to the Twilio number reach
`https://lifestream.brojonat.com`, and the agent (Claude Sonnet 5.5, writing
code against the store) files them. Next: two weeks of daily use (TODO 34).
[PRUNED.md](PRUNED.md) records what was cut and where it lives in git:
monetization (tag `pre-prune`) and the capture clients, which moved to
their own repo, `~/projects/lifestream-clients`.

## How it fits together

Two processes in one pod, sharing a volume:

- **Go service** (`cmd/lifestream`, this module). Owns the event store
  (SQLite, append-only, hash-chained per owner), ingest, projections, the
  dashboard (Datastar over SSE), the agent's internal API, and the
  dispatcher that hands raw events to the agent.
- **Agent** (`agent/`, TypeScript, Pi Durable). One long-lived conversation
  per owner. The model has one tool, code mode: it writes JavaScript that
  calls `query` (read-only SQL over the store) and `append` (a derived
  event), both through Go's internal API. It never touches Go's database.

Raw events are never changed. Everything the agent produces is a derived
event pointing at its parent, and every view is a projection that can be
rebuilt from the events.

## Run it

[mise](https://mise.jdx.dev/) is the front door; `mise tasks` lists
everything.

```
mise install          # pins Go, Node
mise run setup        # go mod tidy, install air, npm ci
mise run dev          # hot reload on :8080 (all interfaces, so the tailnet reaches it), tees logs/serve.log
mise run dev-agent    # the agent, tees logs/agent.log
mise run tunnel       # texts to the local server instead of prod, until it exits; run before dev
mise run test
mise run lint
mise run build        # ./bin/lifestream
```

Tests use temp dirs only and never touch a real data dir. Anything a test
spawns gets a clean environment or explicit flags, since mise loads `.env`
into it.

`mise run tunnel` starts ngrok (only `/twilio/sms` gets through,
`scripts/ngrok-policy.yml`), writes its URL to `LIFESTREAM_PUBLIC_URL` in
`.env`, and points the Twilio number's webhook at it. On exit it points the
webhook back where it was (prod). The URL changes every run, so restart
`mise run dev` after it.

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

## Clients

Anything that can `POST /events` with a user token is a client. The server
knows nothing about particular clients; each picks its own event
granularity and `idempotency_key`. `lifestream push` is the generic one.
The capture adapters for Claude Code, pi, Codex and Strands (with a secret
scanner that masks keys before anything leaves the machine) are in
`~/projects/lifestream-clients`.

## HTTP

Public listener (`--addr`):

| Route | What |
| --- | --- |
| `POST /twilio/sms` | Twilio webhook. Verifies `X-Twilio-Signature`, drops unknown senders, appends `message.received`, answers empty TwiML. Never waits on the agent. |
| `POST /events` | Generic JSON events `{type, source?, occurred_at?, idempotency_key?, payload}`, `Authorization: Bearer <user token>`. One object, a JSON array, or JSONL; a bad event rejects the whole batch. An event whose `idempotency_key` the owner already used is not stored again, so resending is safe. |
| `GET /` | Metrics: per metric collection, latest / 7-day average / change vs the week before, a chart of readings with a trailing 7-day average and a table (live over SSE; times in the viewer's timezone) |
| `GET /inbox` | The last 50 raw events and what each became: labels, readings, records, what a correction changed, flags |
| `GET /collections`, `GET /c/{id}` | Collections; one collection's definition history (each version) and members |
| `POST /c/{id}/remove`, `POST /inbox/add` | Owner corrections: append `label.corrected` (source `dashboard`), which goes to the agent like any raw event |
| `GET /log`, `POST /log/{id}/undo` | Rule log: every `collection.*` change, with one-click undo (appends the inverse: stop, resume, or `collection.amended {remove_rule}` / re-add) |
| `POST /review/{id}/answer` | Answer a `review.flagged` question (queue at the top of the inbox); appends `review.answered`, which goes to the agent. The agent can withdraw a question (`review.withdrawn`) |
| `GET /rules`, `POST /rules`, `POST /rules/{id}/remove` | The rules doc: the owner's standing instructions (`rule.added` / `rule.removed`), which the agent reads before every event |
| `GET /admin` | Process diagnostics |
| `GET /healthz` | `ok` |
| `GET /metrics` | Prometheus text format |

The dashboard serves the only user and answers 503 if there are several,
until login exists (TODO 24). In prod it sits behind basic auth (Deploy).

Internal listener (`--api-addr`, reachable only inside the pod; the agent's
side of the contract):

| Route | What |
| --- | --- |
| `POST /api/query` | `{sql, params?, limit?}`: one SQL statement on a read-only connection (`mode=ro`, `query_only`), 10 s timeout. Returns `{columns, rows, truncated}`; rows default to 200 (max 1000) and text cells over 8 KB are cut. 400 when SQLite refuses it, writes included |
| `GET /api/schema` | `{tables, events}`: the `CREATE` statements (minus `users`) and every event type the agent may append, with its payload shape reflected from the Go structs. The agent's prompt is built from it |
| `POST /api/events` | Append a derived event `{owner, type, parent_id?, payload}`. `Idempotency-Key` header = the Pi Durable tool task id. 201 new, 200 replay, 422 when it contradicts current state (unknown collection, wrong version, a value that isn't one) |
| `GET /api/collections?owner=&status=` | Current collection definitions with their rules |
| `GET /api/collections/{id}/members`, `/values` | Members (with the definition version each was labeled under); a metric's readings |
| `GET /api/rules?owner=` | The rules doc |
| `GET /api/events?owner=&since=&after=&limit=&max_payload=&dispatched=` | Events in log order; big payloads elided, with `payload_size`. `dispatched=1` keeps the events the agent is sent |
| `GET /api/events/{id}`, `/children` | One event (`?offset=&limit=` for a byte range of a large payload); events derived from it |
| `GET /healthz` | `ok` |

The types the agent may append, with their payloads, are `GET /api/schema`
(`internal/store/query.go`): `collection.started|amended|stopped|resumed`,
`label.applied|removed`, `metric.observed|removed`, `record.saved|removed`,
`rule.added`, `review.flagged|withdrawn`. A collection's id is the id of its
`collection.started` event; a reading's and a record's are their events'
ids, and `replaces` supersedes one.

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
| `LIFESTREAM_URL`, `LIFESTREAM_TOKEN` | push | Base URL of the service; a user token from `users add` |
| `LOG_LEVEL` | all | `debug`, `info`, `warn`, `error` |
| `LOG_FORMAT` | all | `json` for machine-readable logs |
| `TWILIO_ACCOUNT_SID`, `TWILIO_NUMBER` | tunnel | The account and the E.164 number whose webhook `mise run tunnel` moves |
| `DOCKER_REPO`, `LIFESTREAM_HOST` | deploy | Image repo prefix (`brojonat`) and public hostname (`lifestream.brojonat.com`) |
| `DASHBOARD_USER`, `DASHBOARD_PASSWORD` | deploy | Basic auth for the prod dashboard (kept in `.env`, turned into a Secret by hand) |

### Agent (`agent/`)

Node ≥ 22.19 (mise pins 25), no build step: `node src/main.ts`. Pi Durable
and pi-codemode 1.3.0, pinned exactly (experimental; the API changed twice
before 1.3.0).

| Variable | Meaning |
| --- | --- |
| `AGENT_ADDR` | Listen address for `POST /submit` and `GET /healthz` (default `127.0.0.1:8082`); point `LIFESTREAM_AGENT_URL` here |
| `LIFESTREAM_API_URL` | Go's internal API (default `http://127.0.0.1:8081`) |
| `DATA_DIR` | Where `agent.db` lives (default `~/.lifestream`) |
| `AGENT_MODEL` | `provider/modelId` (default `anthropic/claude-sonnet-5-5`). A conversation keeps its model, so on its next event an existing one is moved to this |
| `ANTHROPIC_API_KEY` | Read by the Anthropic provider |

One conversation per owner. Each event is submitted with its id as
`requestId` (dedup survives restarts); SMS and dashboard events steer a busy
conversation, others queue. Two extensions:

- **`data`** (`src/data.ts`): the tools `query` and `append`, callable only
  from scripts, and the sections `role` (the instructions), `schema` (from
  `GET /api/schema`, with the owner's id), `collections` (the open
  definitions) and `owner_rules` (the rules doc). Sections are fetched
  before every model request and rendered deterministically, so the prompt
  cache only breaks when they change.
- **code mode** (Pi Durable's): the one tool the model is offered. Scripts
  run in a QuickJS sandbox (no network, no files) and resume after a
  restart without repeating finished calls.

Appending `collection.started`, or a `collection.amended` with an include or
exclude, starts a look-back (`src/lookback.ts`): a background task that owns
a child conversation, a copy of the steward, and gives it the job as one
message (file the last 7 days that belong, or re-check members). It
reports a one-line summary back to the owner's conversation. Payloads over
4 KB reach the conversation as pointers that a query reads. Tests use
pi-ai's faux provider with real scripts in the sandbox;
`test/e2e.test.ts` drives the real Go binary.

## Deploy

One pod in namespace `lifestream`: the Go service, the agent and a
Litestream sidecar sharing a PVC at `/data`. One replica, `Recreate`, since
SQLite has one writer. An init container restores both databases from the
replicas when the volume is empty. The agent's API stays on the pod's
loopback.

It runs on the `firekeeper` k3s cluster at `https://lifestream.brojonat.com`
(traefik, cert-manager `letsencrypt-prod`, images pulled with `regcred`).
`/twilio/sms` and `/events` are public with their own auth (Twilio
signature, bearer token). Everything else, the dashboard included, is
behind Traefik basic auth until login lands (TODO 24); the credentials are
`DASHBOARD_USER` / `DASHBOARD_PASSWORD` in `.env`.

Litestream writes to the R2 bucket `lifestream` with a token scoped to that
bucket. Both were made through the Cloudflare API, no dashboard; see
LEARNINGS.md.

`mise run deploy` builds and pushes both images at the current commit and
applies `deploy/k8s` (it refuses a dirty tree). From scratch:

```
kubectl create namespace lifestream
kubectl get secret regcred -n default -o json | jq 'del(.metadata.namespace,.metadata.resourceVersion,.metadata.uid,.metadata.creationTimestamp)' | kubectl apply -n lifestream -f -
kubectl -n lifestream create secret generic lifestream-env --from-env-file=.env.prod
#   TWILIO_AUTH_TOKEN, ANTHROPIC_API_KEY, LITESTREAM_BUCKET, LITESTREAM_ENDPOINT,
#   LITESTREAM_ACCESS_KEY_ID, LITESTREAM_SECRET_ACCESS_KEY
printf '%s:%s\n' "$DASHBOARD_USER" "$(openssl passwd -apr1 "$DASHBOARD_PASSWORD")" |
  kubectl -n lifestream create secret generic lifestream-dashboard-auth --from-file=users=/dev/stdin
mise run deploy
kubectl -n lifestream exec deploy/lifestream -c lifestream -- /lifestream users add --data-dir /data --name me --phone +1555…
```

Then point the Twilio number's messaging webhook at
`https://$LIFESTREAM_HOST/twilio/sms` (HTTP POST). Changing a value in
`.env.prod` means re-creating `lifestream-env` (`--dry-run=client -o yaml |
kubectl apply -f -`) and `kubectl -n lifestream rollout restart
deploy/lifestream`.

## Layout

```
cmd/lifestream/      CLI and wiring
internal/store/      SQLite: events (append-only, hash chain), users, dispatch state, projections,
                     the read-only query and the schema the agent sees (query.go)
internal/ingest/     /twilio/sms and /events
internal/api/        the agent's internal API
internal/dispatch/   delivers raw events to the agent
internal/bus/        embedded NATS (in-process), backs the diagnostics samples
internal/metrics/    process metrics, /metrics
internal/web/        dashboard handlers, templates, assets/components.js (d3 + Rocket, <local-time>)
agent/src/           the Pi Durable agent: main, agent (harness, code mode, per-owner conversations),
                     data (extension: query, append, sections), lookback (subagent task), goapi, server
agent/test/          faux-model tests and the end-to-end test
deploy/              deploy.sh and k8s/ (kustomize; Deployment, PVC, Service, Ingresses, basic-auth Middleware, Litestream config)
scripts/             tunnel.sh and its ngrok traffic policy (mise run tunnel)
```
