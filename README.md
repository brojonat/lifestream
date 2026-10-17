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
mise install          # pins Go, Node, uv
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
lifestream capture claude-code < hook.json
                                 # from a harness hook: send the transcript's new lines (claude-code, pi, codex)
lifestream sessions ls           # captured sessions as JSONL, most recently active first
lifestream sessions show <id>    # one session's messages in the common format, JSONL
lifestream rebuild               # drop and rebuild every projection from events
lifestream verify                # check every owner's hash chain
```

`-h` works on every command.

## HTTP

Public listener (`--addr`, what the ingress exposes):

| Route | What |
| --- | --- |
| `POST /twilio/sms` | Twilio webhook. Verifies `X-Twilio-Signature`, drops unknown senders, appends `message.received`, answers empty TwiML. Never waits on the agent. |
| `POST /events` | Generic JSON events, `Authorization: Bearer <user token>`. One object, a JSON array, or JSONL; a bad event rejects the whole batch. `llm.message` events (capture adapters) are keyed by `(session_id, sequence)`, so resending a batch is safe; see [Sessions](#sessions). |
| `GET /` | Metrics: per metric collection, latest / 7-day average / change vs the week before, and a chart of readings with a trailing 7-day average (live over SSE) |
| `GET /inbox` | The last 50 raw events and what each became (labels, values, flags); flagged ones highlighted. Of a session, only the head the agent saw |
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
| `GET /api/events?owner=&since=&after=&limit=&max_payload=&dispatched=` | Events in log order; payloads over `max_payload` bytes (default 4096) are elided, with `payload_size`. `dispatched=1` keeps only what the dispatcher sends the agent (raw events minus session tails); the look-back uses it |
| `GET /api/unredacted?owner=&limit=` | Ids of collection members with no `redaction.created` yet, oldest first; the agent's redaction sweep reads it |
| `GET /api/events/{id}/children` | Events derived from it |
| `GET /api/events/{id}` | One event; `?offset=&limit=` returns a byte range of a large payload as `data` |
| `POST /api/events` | Append a derived event `{owner, type, parent_id?, payload}`. `Idempotency-Key` header = the Pi Durable tool *task* id (`api.taskId`; provider tool-call ids aren't unique). 201 new, 200 replay, 422 when it contradicts current state (unknown collection, closed collection, bad version). |
| `GET /healthz` | `ok` |

The agent may append `collection.started|amended|stopped|resumed`,
`label.applied`, `label.removed`, `metric.observed`, `rule.added`, `review.flagged` and
`redaction.created`. Payload shapes are the structs in
`internal/store/projections.go`; a collection's id is the id of its
`collection.started` event.

### Sessions

A capture adapter sends one `llm.message` event per message, tool call or
result, as it happens. The payload is
`{session_id, sequence, harness, model?, cwd?, git?, message}`: `message`
is the harness's own JSON, unchanged. A session is the events sharing a
`session_id`.

- Only a session's first 3 events (`store.SessionHead`) reach the agent.
  The look-back judges only those too.
- Labeling any message labels its session: the session's other events
  join the collection, and later ones join on arrival while the
  collection is open. No model call.
- Removing one message (`label.removed`, or the owner's
  `label.corrected` remove) takes the whole session out, and later
  messages stop joining. An owner's add puts it back.
- Members that joined this way never pass through the agent; its
  redaction sweep gives them their redacted copy.
- Every message is also read into one common format, whatever the
  harness (`internal/transcript`): `{role, parts}`. Roles are user,
  assistant, tool and system. Parts are text, tool_call (with name,
  call_id, input), tool_result (call_id, error), reasoning, image or
  other. This `session_messages` projection is the replay viewer's
  source. `lifestream rebuild` regenerates it after the format changes,
  and `lifestream sessions` reads it.

### Capturing harness sessions

`lifestream capture <harness>` runs from a harness's end-of-turn hook with
the hook's JSON on stdin. It reads the transcript from the offset saved
for the session (`$XDG_STATE_HOME/lifestream/capture/<harness>/<session>.json`,
default `~/.local/state/...`), sends the new complete lines as
`llm.message` events (line number = sequence), and saves the offset only
after the service answers 200. Each event also carries the harness, the
model, `cwd`, and the git remote (credentials stripped), branch and commit.
If the service is unreachable it warns on stderr and exits 0, and the next
turn sends the backlog. It never prints to stdout, which hooks read.

Before sending, the deterministic secret scanner (`internal/scan`, the Go
twin of the agent's, same corpus) masks keys, tokens, private keys,
connection passwords, emails and phone numbers in every string of the
message, keeping keys and key order. Those secrets never reach the server.
The payload says `"scanned": true` and lists `findings`.
`LIFESTREAM_SCAN=off` sends messages unmasked.

For Claude Code only `user`, `assistant` and `system` lines are sent;
titles, modes and file-history backups stay on the machine. To install:

```
go install ./cmd/lifestream          # the hook runs `lifestream` from PATH
claude plugin marketplace add /path/to/this/repo
claude plugin install lifestream-capture@lifestream
```

pi has no command hooks, so a pi extension does the hook's job: after
every turn and at shutdown it runs `lifestream capture pi` with the
session file. Only `message` entries are sent. To install:

```
cp capture/pi/lifestream.ts ~/.pi/agent/extensions/
```

Codex runs a notify program at the end of every turn, passing JSON with
the thread id as its last argument. Capture finds the rollout file under
`$CODEX_HOME/sessions/` (default `~/.codex`) and sends its `response_item`
lines: messages, tool calls, tool output and reasoning. `session_meta`,
which holds the system instructions, is not sent. In `~/.codex/config.toml`
(Codex runs only one notify program, so this replaces any you have):

```
notify = ["lifestream", "capture", "codex"]
```

Strands has no transcript file, so its adapter is a Python hook provider
that POSTs each message itself (`capture/strands`, package
`lifestream-strands`). It sends on `MessageAddedEvent` and retries at the
end of each invocation. Failed sends wait in memory. Each provider is one
session with its own id and sequence. It runs the Python twin of the
secret scanner, and `bytes` content (images) is sent as base64.

```
uv add /path/to/this/repo/capture/strands
```
```python
from lifestream_strands import LifestreamCapture
agent = Agent(hooks=[LifestreamCapture()])
```

Nothing is captured until `LIFESTREAM_CAPTURE=on`: set it in a project's
`mise.toml` `[env]` for that project, or in your shell profile for all of
them. `LIFESTREAM_DENY` wins even when capture is on.

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
| `LIFESTREAM_URL` | push, capture | Base URL of the service |
| `LIFESTREAM_TOKEN` | push, capture | A user token from `users add` |
| `LIFESTREAM_CAPTURE` | capture | `on` to capture; unset or anything else sends nothing |
| `LIFESTREAM_DENY` | capture | Colon-separated path globs (`~` allowed) never captured; matches `cwd` or any parent |
| `LIFESTREAM_SCAN` | capture | `off` skips masking secrets before sending (default on) |
| `XDG_STATE_HOME` | capture | Where capture offsets live (default `~/.local/state`) |
| `CODEX_HOME` | capture | Where Codex keeps its rollouts (default `~/.codex`) |
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
| `SWEEP_SECONDS` | How often the redaction sweep runs (default 30) |

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
the scanner caught. Every redaction starts through a per-conversation
ledger, so one event never gets two tasks; a sweep every `SWEEP_SECONDS`
starts tasks for members Go added without the agent (session messages).
Payloads over 4 KB reach the conversation as pointers. Tests use pi-ai's faux provider;
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
internal/capture/    the transcript tailer behind `lifestream capture`, one parser per harness
internal/transcript/ each harness's message JSON -> the common format (pure functions)
internal/scan/       the deterministic secret scanner (twin of agent/src/scanner.ts and capture/strands' scan.py)
internal/bus/        embedded NATS (in-process), backs the diagnostics samples
internal/metrics/    process metrics, /metrics
internal/web/        dashboard handlers, templates, assets/components.js (d3 + Rocket)
agent/src/           the Pi Durable agent: main, agent (harness, per-owner conversations),
                     collections (extension: tools + sections), goapi, server
agent/test/          faux-model tests, scanner tests, and the end-to-end test
testdata/secrets.json  fake secrets shared by every scanner (TypeScript, Go, Python)
testdata/claude-code/  a synthetic Claude Code transcript and hook inputs
testdata/pi/           a synthetic pi session file
testdata/codex/        a synthetic Codex rollout and notify payload
capture/claude-code/   the Claude Code plugin (Stop and SessionEnd hooks); .claude-plugin/ is its marketplace
capture/pi/            the pi extension (turn_end, session_shutdown); tested from agent/test
capture/strands/       the Strands hook provider (Python, uv; pytest, ruff)
deploy/              deploy.sh and k8s/ (kustomize; Deployment, PVC, Service, Ingress, Litestream config)
```
