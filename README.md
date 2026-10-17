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
| `GET /` | Dashboard (live over SSE) |
| `GET /healthz` | `ok` |
| `GET /metrics` | Prometheus text format |

Internal listener (`--api-addr`, reachable only inside the pod; the agent's
side of the contract):

| Route | What |
| --- | --- |
| `GET /api/collections?owner=` | Current collection definitions |
| `GET /api/events/{id}` | One event; `?offset=&limit=` for a byte range of a large payload |
| `POST /api/events` | Append a derived event. `Idempotency-Key` header = the Pi Durable tool-call id. |

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

## Layout

```
cmd/lifestream/      CLI and wiring
internal/store/      SQLite: events (append-only, hash chain), users, dispatch state, projections
internal/ingest/     /twilio/sms and /events
internal/bus/        embedded NATS (in-process) used as the wake-up bus
internal/metrics/    process metrics, /metrics
internal/web/        dashboard handlers and templates
agent/               the Pi Durable agent (TypeScript)
```
