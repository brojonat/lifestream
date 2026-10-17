# TODO

Read `VISION.md` first: it is the product and design spec. This list covers
**phase 1** (the personal tracker), the phase 2 **capture adapters**, and
three separate tracks at the bottom: **rex environments**, **artifact
textbooks** and **paid notifications**. The rest of phases 2–5 is in
VISION.md and not tracked here yet.

Status: `[ ]` open, `[x]` done, `[blocked: ...]` waiting on something.

**2026-10-17: parked.** The Sprite environments product (the rex track
below) moves to a new, narrower project: Sprite "halo maps" with ratings
and a leaderboard, built from `~/projects/game-show`'s sprites code. This
repo stays as a parts bin (event store, capture adapters, scanners,
redaction, deploy). Open items here are not being worked.
`(after: N)` means the task depends on item N.

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
  a collection or adding an include looks back 7 days by default. Every
  collection member is redacted as soon as it's labeled.

## Open decisions (ask the user)

- [ ] D1. Login (owners and, later, buyers and subscribers): Google OAuth, email magic link, or passkeys? Hellō (hello.coop) is a candidate to look into: it can show a creator is who they say.
- [ ] D2. Model for the agent: Claude Haiku 4.5 is the suggestion; confirm.
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
- [x] 14. Agent-facing API (internal only, not exposed by ingress): `GET /api/collections`, `GET /api/events/{id}` (full payload, or a byte range for large ones), `POST /api/events` for derived events (`collection.*`, `label.applied`, `metric.observed`, `review.flagged`, `redaction.created`). Idempotency key = Pi Durable tool-call id. (after: 13)
- [x] 15. Dispatcher: POST each undelivered raw event to the agent with `requestId = event id`; mark delivered on ack; retry with backoff. (after: 9, 14, 18)

## Agent (TypeScript sidecar, `agent/`)

- [x] 16. Read the Pi Durable docs/source before writing code (https://earendil.com/posts/pi-durable/, `@earendil-works/pi-durable`). It shipped 2026-10-01 and is experimental; record API surprises in LEARNINGS.md. In particular: can a background task call tools, how does it report back to the conversation, and what does it cost?
- [x] 17. Harness with SQLite storage at `$DATA_DIR/agent.db`; one root conversation per owner, created on first event. (after: 16)
- [x] 18. HTTP endpoint `POST /submit {owner, event}` → `conversation.submit({type:"input", content, requestId: event.id})`. Steering commands use `whenBusy: "steer"`. (after: 17)
- [x] 19. `collections` extension: system-prompt section that fetches open collection definitions from Go before every request; tools `create_collection`, `amend_collection`, `stop_collection`, `resume_collection`, `label_event`, `log_value`, `flag_for_review`, `read_event` (all call Go's API). Prompt tells it to classify each text as a command or data, and to record anything the owner teaches (e.g. "w means weight") as a rules-doc entry, not just remember it. (after: 14, 17)
- [x] 20. Look-back task: on `collection.started` or an include, a background Pi Durable task labels events from the last 7 days (overridable in the command). On an exclude, re-check current members. Must resume after a crash. (after: 19)
- [x] 21. `redaction` extension: on every `label.applied`, run a deterministic secret scan + model review and emit `redaction.created` (a derived, redacted copy). Unit-test the scanner against fake keys/tokens. (after: 19)
- [x] 22. `rules` extension: prompt section from a per-owner rules doc editable on the dashboard. (after: 19)
- [x] 23a. Large events as pointers: the dispatcher submits `{id, type, title, url, size}` instead of the payload above a size threshold; redaction of large events runs as a background task (same mechanism as 20) that writes derived events back and posts a one-line summary. (after: 15, 20, 21)
- [x] 23. Fake model provider for tests, so the agent's flows run in CI without API calls. (after: 17)

## Dashboard (Go + Datastar + Rocket)

- [blocked: D1] 24. Login, mapping a login to a `users` row. Everything below is per-owner.
- [x] 25. Live SSE page: metric tiles (latest value, week-over-week change) and a d3 Rocket time-series chart per metric collection with a weekly-average line. Port the components from `python-warehouse-dashboard/.../static/components.js`. (after: 13)
- [x] 26. Inbox: recent raw events with what each became (labels, values, flags); unclear ones highlighted. (after: 13)
- [x] 27. Collections view: definitions with version history, members, add/remove member (emits `label.corrected` → sent to agent). (after: 13, 15)
- [x] 28. Rule log with undo for every steering command; review queue where answers become events submitted to the agent. (after: 27)
- [x] 29. Rules doc editor for the `rules` extension. (after: 22)
- [x] 30. Font size 16px on all inputs (iOS zoom), works at phone width.

## Production basics

- [x] 31. `/healthz` and `/metrics` (Prometheus) on Go; `/healthz` on the agent; slog JSON in prod, `LOG_LEVEL`; graceful SIGTERM on both.
- [x] 32. Dockerfiles for both; k8s manifests: one Deployment, 1 replica, `strategy: Recreate`, two containers sharing a PVC; Litestream sidecar backing up both SQLite files; Ingress exposes only `/twilio/sms` and the dashboard. Secrets (Twilio auth token, model API key) from env. (after: 31)
- [ ] 33. Point the Twilio number's webhook at the deployed `/twilio/sms`; send `weight 175` end to end and watch it appear on the dashboard. (after: 10, 15, 19, 25, 32)

## Follow-ups found 2026-10-17

- [ ] F1. Run the pi extension (`capture/pi/lifestream.ts`) inside a live pi and check a turn reaches the server; so far only tested with a fake pi.
- [ ] F2. Install the Claude Code plugin and check a real Stop hook sends a turn.
- [ ] F3. Click through `/artifacts`, `/topics` and chapter ordering in a browser; so far only handler tests.

## Phase 1 exit gate

- [ ] 34. Two weeks of daily texts with no spreadsheet. Then update VISION.md and start phase 2 (private trace uploads).

## Capture adapters (phase 2, see VISION.md "Capture from harnesses")

Events stream as they happen (`llm.message`, with `session_id` and
sequence); a session is the events sharing a `session_id`. Payloads are the
harness's own JSON, unchanged. Config is env vars only (`LIFESTREAM_URL`,
`LIFESTREAM_TOKEN`, `LIFESTREAM_CAPTURE`, `LIFESTREAM_DENY`). Capture is off
by default: nothing is sent unless `LIFESTREAM_CAPTURE=on`.

- [x] C0. Session handling in Go: `POST /events` accepts a batch (JSON array or JSONL); `llm.message` events keyed by `(session_id, sequence)` for idempotency; projection rule so later events of a labeled session join its collections without a model call; the dispatcher submits only a session's first events to the agent. (after: 11, 13)
- [x] C1. `lifestream capture <harness>`: reads hook JSON on stdin, exits quietly unless `LIFESTREAM_CAPTURE=on`, skips `cwd` matching `LIFESTREAM_DENY`, reads the transcript from the saved offset (one offset file per session under `~/.local/state/lifestream/`), adds harness, model, `cwd` and git remote/branch/commit, sends the batch, then advances the offset. Per-harness line parsers plug in. Test: kill it mid-send, rerun, no duplicates and nothing missing. (after: C0)
- [x] C2. Claude Code: line parser plus a plugin that registers `lifestream capture claude-code` on `Stop` and `SessionEnd`. Fixture: a recorded transcript and hook inputs. (after: C1)
- [x] C3. pi: TypeScript extension on `turn_end` (and `session_start` to open the session) that POSTs the turn's message and tool results (only when `LIFESTREAM_CAPTURE=on`), keeping failed sends in memory for the next turn. First check whether pi writes session files; if it does, call the tailer from C1 instead. Learn pi's hooks while doing 16. (after: C0)
- [x] C4. Strands: Python `HookProvider` on `MessageAddedEvent` that POSTs each message, same capture switch and retry rule as C3. (after: C0)
- [x] C5. Codex: line parser for rollout files; register `lifestream capture codex` as the end-of-turn hook or notify program. Check whether rollout files are zstd-compressed; if so, re-read and skip lines already sent. (after: C1)
- [x] C6. Optional deterministic secret scan before sending, in C1 (Go) and in C3/C4. One shared test corpus of fake keys and tokens, used by these and by 21. (after: C1, C3)
- [x] C7. Derived common-format view of sessions across harnesses (for the replay viewer), rebuildable from the raw events. (after: C2, C3)

## Rex environments (separate track, see VISION.md "Environments as products")

Moving to a new project (2026-10-17); see the note at the top. Not
phase 1 work, and not blocked by it except where noted. Which phase it
ships in is open (R-D1).

Decisions already made (don't re-litigate):

- Each buyer gets their own Sprite (Firecracker VM), built fresh from the
  seller's recipe. Checkpoints can't seed a new Sprite, so there is no
  "fork the golden image."
- A recipe is a `mise.toml`: `[tools]` pins versions, a `setup` task builds
  the environment. An agent helps write it; provisioning a buyer is plain
  `mise run setup`, no model calls.
- The platform layer (Claude Code, Codex, pi) is on by default; sellers can
  opt out but are pushed not to.
- Model access is free to buyers, through OpenRouter, one key per buyer
  with a USD spend limit. While testing, use the cheapest reasonably good
  model: `gpt-6-luna` (high) for Codex and pi, Haiku 5.5 for Claude Code.
  Frontier models at launch.
- Rex is the way in, over Tailscale (`rex server start --tailscale`). Never
  `--allow-anonymous`. We're building ahead of rex's Linux server on purpose.

Open decisions (ask the user):

- [ ] R-D1. Which phase do environments ship in: 3 (free, invited visitors) or 4 (paid seats)?
- [ ] R-D2. Who pays for buyers' Sprites: the platform (in the price) or the seller's own Fly account?

Tasks:

- [ ] R1. Hand-write one recipe for a real environment (repo, database, sessions on disk) and get `mise run setup` to build it clean in a fresh Sprite.
- [ ] R2. Platform layer: install Claude Code, Codex and pi through mise after the recipe runs, each pointed at OpenRouter. Confirm the exact OpenRouter model IDs for `gpt-6-luna` and Haiku 5.5, and that Claude Code works through OpenRouter. (after: R1)
- [ ] R3. Per-buyer keys: create a key with a monthly USD limit through OpenRouter's key management API (needs a management key); delete it on revoke. (after: R2)
- [ ] R4. `AGENTS.md` tour-guide template for sellers, plus a `CLAUDE.md` that points to it. (after: R1)
- [ ] R5. Grant/Revoke adapter: Grant = create Sprite, `mise run setup`, platform layer, key, checkpoint. Revoke = warn the buyer, delete the key, destroy the Sprite. (after: R2, R3)
- [ ] R6. Until rex ships: give one friend a shell in a built Sprite (Sprites' own access or SSH over Tailscale) and watch them use it. Record cost per session. (after: R5)
- [blocked: rex Linux server] R7. Run `rex server start --tailscale` inside a Sprite; check Tailscale works there (likely userspace networking). Connect from the Mac app and `rex -S`. (after: R5)
- [ ] R8. Recipe helper: a startup agent in a scratch Sprite walks the seller through describing the environment until `mise run setup` builds it. (after: R1, done by hand first)
- [ ] R9. Test-build in the publish approval flow (seller walks the fresh build before it goes on sale), and a scheduled rebuild of every published recipe. (after: R5)

## Artifact textbooks (separate track, see VISION.md "Claude artifacts as textbooks")

Not phase 1 work. Needs the event store (7, 8) and collections (13).

Decisions already made (don't re-litigate):

- Both delivery modes: static artifacts are hosted by us behind our login;
  artifacts that need claude.ai runtime features are delivered as claude.ai
  links.
- Both units: books (ordered collections of artifacts) and single chapters.
- Books and rex environments are separate products; creators group them by
  namespace (`<creator>/<namespace>/<product>`).

Tasks:

- [x] A1. `artifact.saved` event: upload HTML (dashboard, `lifestream push`) or paste a claude.ai link. Payload: title, HTML if any, claude.ai URL. (after: 8)
- [x] A2. Chapter order for a book collection, set on the dashboard. (after: 13)
- [ ] A3. Hosted artifact viewer on a separate origin, artifacts in a sandboxed iframe; per-buyer watermark. (after: A1)
- [x] A4. Check whether claude.ai can share an artifact with specific people (would make link delivery enforceable). Yes, by email invite; see LEARNINGS.md.
- [ ] A5. Creator namespaces in product names. (after: 13)
- [ ] A6. Publish one free book of my own artifacts and see if anyone outside reads it. (after: A2, A3)
- [ ] A7. Launch checkout: Stripe Checkout on my own account (no Connect) with a 100%-off coupon pre-applied; on completion, grant access and record the buyer's email. Test that a $0 total completes. (after: A3)
- [ ] A8. HN launch with the artifact headline (VISION.md "Launch"): free summary on the landing page, artifact behind the free checkout. (after: A6, A7)

## Paid notifications (separate track, see VISION.md "Paid notifications")

Needs the event store (7, 8), login (24, blocked on D1, which now covers
buyers too) and the launch checkout (A7).

Decisions already made (don't re-litigate):

- Topics are independent of namespaces and products.
- Price per topic: one-time or monthly, creator's choice. $0 at launch via
  the 100%-off coupon.
- Unlimited by default; optional first-N cap that we enforce.
- Sending is manual only, from the dashboard. The agent never sends.
- History starts at subscription by default; the creator can set it
  further back.
- Delivery is an in-app inbox by default.

Tasks:

- [x] N1. Topic events (`topic.created`, `topic.amended`, `topic.subscribed`, `topic.unsubscribed`, `notification.sent`) and projections for topics and subscriptions. (after: 8, 13)
- [ ] N2. Checkout for topics: one-time or subscription mode, coupon pre-applied at launch; webhooks emit `topic.subscribed`, and cancel, refund or lapse emit `topic.unsubscribed`. Share the code with A7. (after: N1, A7)
- [ ] N3. First-N cap: reserve a slot per session with an expiry, release it on `checkout.session.expired`; test that two buyers can't both get the last slot. (after: N2)
- [x] N4. Send form on the dashboard: pick topic, write payload, confirm with the subscriber count. (after: N1)
- [ ] N5. Subscriber inbox: computed on read from subscriptions and history start, live over SSE with an unread count; payloads rendered as escaped text and links. (after: N1, 24)
- [x] N6. History start setting per topic (default: subscription date). (after: N1)
