# TODO

Read `VISION.md` first: it is the product and design spec. This list covers
**phase 1** (the personal tracker) and the agent.

Status: `[ ]` open, `[x]` done, `[blocked: ...]` waiting on something,
`[pruned]` cut on 2026-10-19, `[moved]` moved to `~/projects/lifestream-clients`
on 2026-10-20 (both in PRUNED.md). `(after: N)` means the task depends on
item N.

**2026-10-19: refocused.** The agent is a data handler first: every event
in, structured data out. Paid topics, artifacts, chapter order and
redaction were pruned (PRUNED.md, tag `pre-prune`); the rex, artifact
textbook and paid notification tracks that used to sit at the bottom of
this file live there too.

## Next up: use it (TODO 34)

**2026-10-20: live, and the agent is a steward.** Texts to +1 567-399-1269
reach `https://lifestream.brojonat.com`. The agent (Sonnet 5.5) writes code
against the store: it queries with read-only SQL and appends events, keeps
records of any shape, dates and corrects readings, and backfills new
collections through background subagents (VISION.md "The agent").
Smoke-tested against the real model in a temp stack (timezones, two
readings per text, a workout record, corrections, a sleep look-back) at
about a cent per text.

- [ ] 34. Two weeks of daily texts, no spreadsheet (the phase 1 gate, below). Text more than weight (workouts, meals, sleep) to see what records the steward invents.
- [ ] F3. Click through the inbox, collections, rules and log pages on the phone with real data.
- [ ] 37. Weekly check while 34 runs: `SELECT kind, count(*) FROM records GROUP BY kind` for duplicate kinds, corrections per week, and the model bill (Anthropic console). If kinds drift, tighten the prompt or add a rule.
- [ ] 38. Show records on the dashboard: today they only appear as chips in the inbox. A page per kind, a table from the JSON, and a chart when a field is numeric. (after: 34)
- [ ] 36. Restart k3s on `firekeeper` before 2027-02-15, when its control-plane certs expire (the node warns `CertificateExpirationWarning`; a restart rotates them). Then check the local kubeconfig still works.

## Decisions already made (don't re-litigate)

- Go service owns the event store, ingest, projections and dashboard. The Pi
  Durable agent is a separate TypeScript process. It writes back only through
  Go's HTTP API; it never touches Go's database.
- SQLite for everything. No Absurd and no Postgres in phase 1.
- Twilio SMS, inbound only. The system never texts back; feedback is the live
  dashboard.
- Allowlist users by phone number in a `users` table. Single user now, but
  every row is scoped by `owner` so more users are possible later.
- Runs in the owner's Kubernetes cluster and is reachable from the internet.
- Datastar (Tao of Datastar: server owns state, SSE out, POST in) plus d3
  charts as Datastar Rocket components.
- A collection is the core concept; a metric (e.g. weight) is a collection
  with a value extractor and a chart. Steering commands are events. Starting
  a collection or adding an include looks back 7 days by default.
- The agent is a data handler first (2026-10-19): it files every event and
  never sells, publishes or sends. Monetization comes later as explicit
  features that read derived data, never as agent behavior.
- The agent acts through Pi Durable's code mode, not bespoke tools
  (2026-10-20): scripts call `query` (read-only SQL over the whole store)
  and `append` (any event type Go accepts). Writes stay events, so they're
  traced, idempotent and reversible. The prompt is built from Go's schema.
- The agent runs on Sonnet 5.5 (`anthropic/claude-sonnet-5-5`, the code
  default; D2, 2026-10-20). About a cent per text; cheap next to the work.
- Look-backs are background subagents: a copy of the steward given the job
  as one message, never a fixed judge (2026-10-20).
- Clients aren't the server's concern (2026-10-20): the contract is
  `POST /events`; clients choose their granularity and idempotency keys.
  First-class clients live in `~/projects/lifestream-clients`.
- Single user for now; multi-tenancy isn't a goal. `query` reads every
  owner's rows, which is fine until a second user exists.
- Prod lives on the `firekeeper` k3s cluster as `lifestream.brojonat.com`;
  the dashboard is public behind basic auth until login (D1), not on the
  tailnet (2026-10-20).

## Open decisions (ask the user)

- [ ] D1. Login for the dashboard: Google OAuth, email magic link, or passkeys? Hellō (hello.coop) is a candidate. Traefik basic auth stands in until then.
- [x] D2. Model: Sonnet 5.5 (see "Decisions already made"); Haiku 5.5 ran the first day.
- [ ] D3. Product name ("Lifestream" is a placeholder; repo dir is `tracker`).

## Bootstrap

- [x] 1. `git init`; `.gitignore` with `logs/`, `.env`, `*.db*`, `node_modules/`, `bin/`.
- [x] 2. Write `README.md` for phase 1: what it does, how to run/test, HTTP routes, env vars. Link VISION.md.
- [x] 3. Write `install-skills.sh` per `~/.claude/CLAUDE.md` (one `-s` skill per line: Go service, urfave-cli, datastar, sqlite, systematic-debugging, TDD; `npx skills find` for Pi/TypeScript). Run it, commit `skills-lock.json`. (after: 2)
- [x] 4. Generate the Go side from `~/projects/llmsrules/project-templates/go-local-app` (cookiecutter into a temp dir, then copy in). Keep its bus/SSE/web structure; merge its TODO/CHANGELOG/LEARNINGS into ours rather than overwriting. (after: 2)
- [x] 5. Replace the template's Makefile with tasks in the existing `mise.toml` (keep `[tools]`): `setup`, `build`, `test`, `lint`, `dev`, `dev-agent`, `deploy`. Dev tasks tee to `logs/serve.log` and `logs/agent.log`. (after: 4)
- [x] 6. `CHANGELOG.md` and `LEARNINGS.md` exist (created 2026-10-16). Add the template's entries to them, and copy the Datastar/Rocket entries from `python-warehouse-dashboard/{{cookiecutter.project_slug}}/LEARNINGS.md` (open shadow root, `id` on every host, `renderOnPropChange: false`, load `datastar-rocket.js` instead of `datastar.js`, emit objects not strings). (after: 4)

## Event store (Go)

- [x] 7. Schema + migrations: `users`, `events` (envelope from VISION.md: ULID id, owner, source, type, occurred_at, received_at, parent_id, payload JSON, hash chained per owner). Events are append-only: no UPDATE/DELETE paths. (after: 4)
- [x] 8. `Append(ctx, event)` with hash chaining and a test that tampering breaks the chain. (after: 7)
- [x] 9. Dispatch state: track which events have been delivered to the agent (`delivered_at` or a cursor table); redeliver undelivered events on startup. (after: 7)

## Ingest

- [x] 10. `POST /twilio/sms`: verify `X-Twilio-Signature`, drop unknown senders (log them), append `message.received`, return empty TwiML 200. Never block on the agent. (after: 8)
- [x] 11. `POST /events` (authenticated, per-user token) for generic JSON events, plus a tiny CLI `push` that reads JSON on stdin. (after: 8)
- [x] 12. Test fixtures: a recorded Twilio webhook body + signature for handler tests.

## Collections and projections (Go)

- [x] 13. Projections built from events: `collections` (current definition = fold of `collection.started/amended/stopped/resumed`, with version), `collection_members` (from `label.applied`/`label.corrected`, recording `definition_version`), `metric_values` (from `metric.observed`). Must be rebuildable from scratch; add a `rebuild` command and a test that rebuild == incremental. (after: 8)
- [x] 14. Agent-facing API (internal only, not exposed by ingress): `GET /api/collections`, `GET /api/events/{id}` (full payload, or a byte range for large ones), `POST /api/events` for derived events (`collection.*`, `label.applied`, `metric.observed`, `review.flagged`). Idempotency key = Pi Durable tool-call id. (after: 13)
- [x] 15. Dispatcher: POST each undelivered raw event to the agent with `requestId = event id`; mark delivered on ack; retry with backoff. (after: 9, 14, 18)

## Agent (TypeScript sidecar, `agent/`)

- [x] 16. Read the Pi Durable docs/source before writing code (https://earendil.com/posts/pi-durable/, `@earendil-works/pi-durable`). It shipped 2026-10-01 and is experimental; record API surprises in LEARNINGS.md. In particular: can a background task call tools, how does it report back to the conversation, and what does it cost?
- [x] 17. Harness with SQLite storage at `$DATA_DIR/agent.db`; one root conversation per owner, created on first event. (after: 16)
- [x] 18. HTTP endpoint `POST /submit {owner, event}` → `conversation.submit({type:"input", content, requestId: event.id})`. Steering commands use `whenBusy: "steer"`. (after: 17)
- [x] 19. `collections` extension: system-prompt section that fetches open collection definitions from Go before every request; tools `create_collection`, `amend_collection`, `stop_collection`, `resume_collection`, `label_event`, `log_value`, `flag_for_review`, `read_event` (all call Go's API). Prompt tells it to classify each text as a command or data, and to record anything the owner teaches (e.g. "w means weight") as a rules-doc entry, not just remember it. (after: 14, 17)
- [x] 20. Look-back task: on `collection.started` or an include, a background Pi Durable task labels events from the last 7 days (overridable in the command). On an exclude, re-check current members. Must resume after a crash. (after: 19)
- [pruned] 21. `redaction` extension: on every `label.applied`, run a deterministic secret scan + model review and emit `redaction.created` (a derived, redacted copy). Unit-test the scanner against fake keys/tokens. (after: 19)
- [x] 22. `rules` extension: prompt section from a per-owner rules doc editable on the dashboard. (after: 19)
- [x] 23a. Large events as pointers: the dispatcher submits `{id, type, title, url, size}` instead of the payload above a size threshold. (after: 15, 20)
- [x] 23. Fake model provider for tests, so the agent's flows run in CI without API calls. (after: 17)
- [x] 39. Readings are fully editable: several per event, `observed_at` from what the owner says, `replaces` and `metric.removed` for corrections. (2026-10-20)
- [x] 40. The steward: code mode with `query` and `append` replaces the tools of 19 and 22; `/api/query` (read-only connection), `/api/schema` (payload shapes reflected from the Go structs); the model moves to `AGENT_MODEL` on its next event. (2026-10-20)
- [x] 41. Records: `record.saved` / `record.removed`, a kind plus a JSON object, in the `records` projection, shown in the inbox. (2026-10-20)
- [x] 42. Look-back as a background subagent (replaces the judge of 20); `review.withdrawn`; a set collection with a unit is refused. (2026-10-20)

## Dashboard (Go + Datastar + Rocket)

- [blocked: D1] 24. Login, mapping a login to a `users` row. Everything below is per-owner.
- [x] 25. Live SSE page: metric tiles (latest value, week-over-week change) and a d3 Rocket time-series chart per metric collection with a weekly-average line. Port the components from `python-warehouse-dashboard/.../static/components.js`. (after: 13)
- [x] 26. Inbox: recent raw events with what each became (labels, values, flags); unclear ones highlighted. (after: 13)
- [x] 27. Collections view: definitions with version history, members, add/remove member (emits `label.corrected` → sent to agent). (after: 13, 15)
- [x] 28. Rule log with undo for every steering command; review queue where answers become events submitted to the agent. (after: 27)
- [x] 29. Rules doc editor for the `rules` extension. (after: 22)
- [x] 30. Font size 16px on all inputs (iOS zoom), works at phone width.
- [x] 30a. Times in the viewer's timezone (`<local-time>`, chart in local time); the inbox shows what a correction changed. (2026-10-20)
- [ ] 35. Live agent panel: what the agent is doing right now (the event it's on, streamed thinking/text, the script it's running and its nested calls, running look-back subagents, queued inputs) and running model cost. Pi Durable's `conversation.watch()` (or the experimental `watchEvents()`) on the agent, streamed over a new endpoint on its loopback API; Go relays it to the dashboard's SSE stream, since only Go is reachable. Cost from the view's `docs["pi.usage"]`. See the pi-durable README "Watching a Conversation" and "Agent Events". (after: 25)

## Production basics

- [x] 31. `/healthz` and `/metrics` (Prometheus) on Go; `/healthz` on the agent; slog JSON in prod, `LOG_LEVEL`; graceful SIGTERM on both.
- [x] 32. Dockerfiles for both; k8s manifests: one Deployment, 1 replica, `strategy: Recreate`, two containers sharing a PVC; Litestream sidecar backing up both SQLite files; Ingress exposes only `/twilio/sms` and the dashboard. Secrets (Twilio auth token, model API key) from env. (after: 31)
- [x] 32a. Deployed 2026-10-20: traefik + letsencrypt ingress, `regcred` pulls, dashboard behind basic auth, R2 bucket `lifestream` with a bucket-scoped token.
- [x] 33. Text `weight 175` end to end and watch it appear on the dashboard. Done 2026-10-20, locally (ngrok, `mise run tunnel`) then in prod. (after: 10, 15, 19, 25, 32)

## Follow-ups found 2026-10-17

- [moved] F1. Run the pi extension inside a live pi (lifestream-clients TODO 3).
- [moved] F2. Install the Claude Code plugin and check a real Stop hook (lifestream-clients TODO 4).
- [ ] F3. Click through the inbox, collections, rules and log pages in a browser with real data; so far only handler tests. Now possible in prod (see "Next up"). (after: 33)

## Phase 1 exit gate

- [ ] 34. Two weeks of daily texts with no spreadsheet. Then update VISION.md and start phase 2 (other streams through first-class clients).

## Capture adapters

[moved] C0–C7 (session handling in the server, the capture CLI, the Claude
Code, pi, Strands and Codex adapters, the secret scanners, the
common-format view) were done by 2026-10-17. On 2026-10-20 the adapters
moved to `~/projects/lifestream-clients` (its TODO 1: one event per turn)
and the server-side session handling was removed; PRUNED.md has the map.
