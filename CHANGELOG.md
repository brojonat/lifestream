# Changelog

All notable changes to this project are recorded here, newest first. The
format follows [Keep a Changelog](https://keepachangelog.com/). Entries are
grouped by date; nothing is versioned yet.

## 2026-10-20, evening: the live agent panel

### Added

- `/agent`: what the agent is doing right now, live. Working or idle, the
  event it's on, streamed thinking and text, the script it's running or ran
  last with each call (status, timing, errors) and its output, queued
  inputs, spend, and look-back subagents nested under it. The agent
  publishes snapshots on `GET /live?owner=` (SSE, from Pi Durable's
  `viewState()` and `taskGraph()`); Go relays them, reconnecting when the
  agent restarts. TODO 35 done.

## 2026-10-20, later: the agent becomes a steward

### Added

- The agent acts through Pi Durable's code mode: the model writes
  JavaScript (QuickJS sandbox, durable across restarts) that calls `query`
  (one read-only SQL statement over the whole store, `POST /api/query` on a
  `mode=ro`, `query_only` connection) and `append` (any event type Go
  accepts). `GET /api/schema` gives the tables and the appendable event
  types with payload shapes reflected from the Go structs; the prompt is
  built from it.
- Records: `record.saved` / `record.removed` keep data of any shape (a kind
  plus a JSON object) in a `records` projection, shown in the inbox.
- Readings are fully editable: any number per event, `observed_at` from what
  the owner says, `replaces` to supersede one and `metric.removed` to
  withdraw one. `GET /api/collections/{id}/values`.
- `review.withdrawn`: the agent takes back a question later events settled.
- `<local-time>`: dashboard times in the viewer's timezone; the chart uses
  local time. The inbox says what a correction changed.
- TODO 37 (weekly check on record kinds, corrections and cost) and 38
  (records on the dashboard).

### Changed

- Model default is `anthropic/claude-sonnet-5-5`; an existing conversation
  moves to `AGENT_MODEL` on its next event.
- Look-backs run as background subagents: a task owns a child conversation
  that starts as a copy of the steward and gets the job as one message, so
  old events are filed like new ones. It reports back a one-line summary.
- A set collection with a unit or metric is refused (422).
- The prompt leaves backfills to the look-back and has it clean up records
  and questions a new collection settles.
- VISION.md rewritten around the event stream and its durable steward;
  README, TODO and LEARNINGS updated to match.

### Removed

- The agent's twelve bespoke tools (`label_event`, `log_value`,
  `add_rule`, ...) and the look-back's fixed JSON judge and retry helpers.
- The capture clients: adapters, `lifestream capture`, secret scanners and
  fixtures moved to `~/projects/lifestream-clients`.
- Server-side sessions: `llm.message` is an ordinary event; no
  `(session_id, sequence)` keys, session-head dispatch, session projections
  or `lifestream sessions`. A migration drops `session_events` and
  `session_messages`. Clients send their own `idempotency_key`. See
  PRUNED.md.

## 2026-10-20

### Added

- Live in prod at `https://lifestream.brojonat.com` on the `firekeeper`
  k3s cluster. The first real texts ("Start tracking my weight",
  "Weight 174.4") were filed by the real agent, locally and in prod.
- `mise run tunnel` (`scripts/tunnel.sh`): ngrok with a traffic policy
  that lets only `/twilio/sms` through, rewrites `LIFESTREAM_PUBLIC_URL`
  in `.env`, points the Twilio webhook at the tunnel and restores it on
  exit.
- The prod dashboard is public behind Traefik basic auth
  (`deploy/k8s/dashboard.yaml`), pinned to the lowest router priority so
  `/events` and `/twilio/sms` stay on their own auth.
- R2 bucket `lifestream` for Litestream, with a token scoped to it.
- TODO 35: live agent panel from Pi Durable's watch API. TODO 36: rotate
  `firekeeper`'s k3s certs before 2027-02-15.

### Changed

- Agent model default is `anthropic/claude-haiku-5-5` (was Haiku 4.5).
- `mise run dev` serves on all interfaces (`.air.toml`), so the tailnet
  reaches the local dashboard.
- Ingress uses traefik and cert-manager `letsencrypt-prod`; the pod pulls
  with `regcred`, like the cluster's other apps.
- Dev tasks set `pipefail`, so a crash under `tee` is a failure.
- mise `[tools]` no longer installs `claude` / `claude-code`.

### Fixed

- The Strands adapter's real-service test inherited `.env` through mise
  and dispatched its events to the running dev agent. It now starts the
  server without `LIFESTREAM_*` / `TWILIO_*` variables.
- `firekeeper` ran out of inotify instances (128, the default), which broke
  `kubectl logs -f`. Raised to 8192 instances and 524288 watches in
  `/etc/sysctl.d/90-inotify.conf`.

## 2026-10-19

### Changed

- Refocused: the agent is a data handler first. It receives every event
  and turns it into structured data or leaves it stored; it never sells,
  publishes or sends. VISION.md rewritten to match (the old
  monetization-first spec is at tag `pre-prune`).
- TODO.md: "Next up" is texting the real Twilio number and watching the
  real agent file it (33a–g), local behind a tunnel first, then deploy.

### Removed

- Paid-notification topics: events, projections, `/topics` page and their
  migration. (Reverts fb9a1e0, eca121e, 2260616.)
- Artifacts: `artifact.saved` validation and the `/artifacts` page.
  (Reverts 7bdbd99.)
- Chapter order: `collection.ordered`, `POST /c/{id}/move` and its
  migration. (Reverts da568d8.)
- Redaction: the agent's redaction task and TypeScript scanner, the sweep
  and `SWEEP_SECONDS`, `GET /api/unredacted`, the inbox's "redacted copy".
  Capture's Go and Python scanners stay.
- TODO tracks for rex environments, artifact textbooks and paid
  notifications, and the learnings that only served them.

### Added

- PRUNED.md: what was cut, the commits that built it, and how to bring it
  back. Tag `pre-prune` marks the last commit before the prune (3697500).

## 2026-10-17

### Changed

- Parked. The Sprite environments product moves to a new project built on
  `~/projects/game-show`'s sprites code; this repo is a parts bin
  (TODO.md note at the top).
- VISION.md, from the owner: the platform for selling subscriptions to
  data streams is the product, and "two products in one" is the biggest
  risk. Environments are one product, marketed like custom Halo maps.
  Artifact collections are premium lab notebooks and textbooks. Network
  effects are the moat (ratings, "you might also enjoy", leaderboards).
  exe.dev is a possible Sprites stand-in; Hellō a login option.
- VISION.md: correction. An entry said every product is delivered as an
  environment and briefly that buyers queue or bid; both were walked back
  the same day. Environments are one product; pricing stays Stripe
  Products.
- Topic, notification and ordering events are no longer dispatched to the
  agent (they cost a model call each and filled the inbox).
- The inbox shows only a session's head, so one captured session can't
  bury everything else.

### Added

- Chapter order (`collection.ordered`, `POST /c/{id}/move`): set
  collections get up/down buttons; members show in the owner's order, a
  book's chapters. (TODO A2)
- Topics page (`/topics`): create topics, set how far back new subscribers
  can read, send notifications with a confirmed subscriber count (409 when
  it changed). (TODO N4, N6)
- Paid-notification events and projections: `topic.created|amended|
  subscribed|unsubscribed`, `notification.sent`; subscription periods; the
  subscriber inbox computed on read. The agent can never send. (TODO N1)
- Artifacts: `artifact.saved` from the dashboard (`/artifacts`: HTML upload
  or a claude.ai link) or `lifestream push`; HTML is stored, never rendered
  on the dashboard origin. (TODO A1)
- Common-format sessions: `internal/transcript` reads Claude Code, pi,
  Codex and Strands messages into `{role, parts}`; a rebuildable
  `session_messages` projection; `lifestream sessions ls|show`. (TODO C7)
- Strands adapter (`capture/strands`, Python/uv): a hook provider that
  POSTs each message, retries from memory, and scans with the Python twin
  of the scanner. (TODO C4)
- `internal/scan`: the Go twin of the agent's secret scanner, same corpus;
  capture masks secrets before sending unless `LIFESTREAM_SCAN=off`.
  (TODO C6)
- Codex capture through its notify program; the rollout is found by thread
  id. (TODO C5)
- pi capture: an extension that runs `lifestream capture pi` after every
  turn. (TODO C3)
- `lifestream capture <harness>`: the transcript tailer (saved offsets,
  batches, git and model metadata) and the Claude Code plugin on Stop and
  SessionEnd. (TODO C1, C2)
- Sessions in Go: `llm.message` keyed by `(session_id, sequence)`; only a
  session's first 3 events reach the agent; labeling one message labels
  the session and later ones join on arrival; the agent's redaction sweep
  (`GET /api/unredacted`) covers members that joined that way; the
  look-back judges only session heads. (TODO C0)
- Redaction: every labeled event gets one `redaction.created` copy from a
  background task (scanner, then a model review in chunks). (TODO 21, 23a)
- Look-back: starting a collection or adding an include labels the last 7
  days in the background; an exclude re-checks members. (TODO 20)
- Rule log with undo, and the review queue. (TODO 28)
- Rules doc: projection, dashboard editor, agent extension. (TODO 22, 29)
- Dockerfiles and Kubernetes manifests: one pod with the Go service, the
  agent and Litestream. `/healthz`, `/metrics`, JSON logs, SIGTERM.
  (TODO 31, 32)
- The agent: a Pi Durable sidecar (TypeScript) with the collections
  extension; faux-model tests and an end-to-end test against the real
  binary. (TODO 16-19, 23)
- Collections view with owner corrections. (TODO 27)
- Dashboard: live metrics page and inbox. (TODO 25, 26, 30)
- Dispatcher: raw events reach the agent in log order. (TODO 15)
- The agent's internal API on its own listener. (TODO 14)
- Projections (collections, members, metric values) and `rebuild`.
  (TODO 13)
- Ingest: Twilio SMS webhook, `POST /events`, `push`/`users`/`verify`.
  (TODO 10-12)
- Event store: append-only SQLite log, hash chain per owner, dispatch
  state. (TODO 7-9)
- Go service bootstrapped from the go-local-app template; mise tasks;
  per-skill install script. (TODO 1-6)

### Changed (docs, earlier the same day)

- VISION.md: public pitch is now "Patreon for data" (title and summary).
  OnlyFans stays recorded as the actual inspiration.

### Added

- VISION.md: large events (artifacts, traces) reach the agent as pointers
  and are read with a `read_event` tool; heavy work runs as Pi Durable
  background tasks; anything the owner teaches is stored in Go so
  background tasks follow it.
- TODO.md: `GET /api/events/{id}` (14), `read_event` and the "record what
  the owner teaches" rule (19), background-task questions (16), and 23a
  (large events as pointers).
- VISION.md: "Launch" section. The HN post is the first product (artifact
  headline first, dev-environment headline once rex ships); free through
  Stripe Checkout with a 100%-off coupon to collect real purchases and
  emails; free summary on the landing page; open question on self-hosting.
- TODO.md: A7 (free Stripe checkout) and A8 (HN launch).
- VISION.md: "Paid notifications" idea, with two open questions (what is
  capped, how subscribers are pinged).
- TODO.md: "Ideas" section listing paid notifications.
- VISION.md: notifications subscribe to a topic (a creator namespace can be
  one), free at launch; third HN headline ("pay me ($0) to get my new
  stock").
- VISION.md: paid topics are unlimited by default with an optional first-N
  cap (enforced by us, not Stripe); notifications show up in the app by
  default.
- TODO.md: paid notifications idea updated to match (topics, in-app pings,
  optional first-N cap).
- VISION.md, TODO.md: topics are independent of namespaces (correcting the
  earlier entry that said a topic can be a creator namespace).
- VISION.md: plan for paid notifications. Per-topic one-time or monthly
  pricing, manual sends only, history from subscription by default with a
  creator-set start further back, in-app inbox computed on read, buyer
  logins.
- TODO.md: "Paid notifications" track (N1–N6), replacing the "Ideas"
  section; D1 (login) now covers buyers too.
- VISION.md: "Capture from harnesses". One ingest endpoint, thin adapters
  per harness (pi first, then a Claude Code `SessionEnd` hook plugin, then
  Strands, Codex to check); whole sessions on session end, not live; seller
  edits as derived copies.
- TODO.md: "Capture adapters" section (C1–C5), marked phase 2.
- VISION.md, TODO.md: correction to the two entries above. Capture is a
  stream, not an upload at session end: adapters push `llm.message` events
  as they happen, keyed by `(session_id, sequence)`; the agent labels a
  session once and later events inherit its collections. New C0 (session
  handling in Go); C1–C5 updated.
- VISION.md: why capture streams even though nothing is sold live (no
  reliance on a clean session end; mid-session steering applies), sending
  per turn and tool call, not per token.
- VISION.md: capture adapter plan. Payloads are the harness's own JSON;
  two kinds of adapter (transcript tailers for Claude Code and Codex
  through one `lifestream capture` command, in-process extensions for pi
  and Strands); per-harness hook details; source metadata; capture on by
  default with a local deny list; env-var config.
- TODO.md: capture tasks rewritten as C0–C7 (batch ingest, the shared
  tailer, one task per harness, secret scan, common-format view).
- LEARNINGS.md: pi's npm scope rename; Codex hooks are off by default.
- VISION.md, TODO.md: correction to the capture plan above. Capture is off
  by default and needs `LIFESTREAM_CAPTURE=on` (per project via mise
  `[env]`, or globally); `LIFESTREAM_DENY` stays as an optional guard.
- LEARNINGS.md: Stripe Checkout Sessions don't track stock.

## 2026-10-16

### Added

- VISION.md: "Environments as products (rex)". Buyers get a fully built
  environment to explore, like loading a Halo map: one Sprite (Firecracker
  VM) per buyer, built from the seller's mise recipe, reached over Tailscale
  through rex. Includes an agent that helps sellers write recipes,
  test-builds before publishing, checkpoint-as-reset, and "move me in"
  dotfiles for buyers.
- VISION.md: the platform installs Claude Code, Codex and pi in every
  environment by default. Model access is free to buyers through
  OpenRouter, one key per buyer with a spend limit. Testing uses the
  cheapest reasonably good models (`gpt-6-luna` high, Haiku 5.5); frontier
  models at launch.
- VISION.md: rex environment row in the channel table, new risks (rex timing,
  recipes that stop building, buyers running code, buyer credentials, model
  costs, token farming) and two open questions (which phase, who pays for
  Sprites).
- VISION.md: "Claude artifacts as textbooks", selling Claude artifacts as
  interactive textbooks, paired with rex environments as the lab.
- TODO.md: "Rex environments" track (R1–R9, R-D1, R-D2), and an "Ideas"
  section listing the artifacts idea.
- VISION.md: plan for artifact textbooks. Artifacts enter as `artifact.saved`
  events; books are ordered collections and chapters sell on their own;
  static artifacts are hosted by us on a separate sandboxed origin, and
  ones that need claude.ai's runtime are sold as claude.ai links.
- VISION.md: creator namespaces for all products
  (`<creator>/<namespace>/<product>`). Books and rex environments stay
  separate products; creators group related ones by namespace.
- VISION.md: channel table gains static artifacts in the hosted viewer and
  claude.ai artifact links.
- TODO.md: "Artifact textbooks" track (A1–A6), replacing the "Ideas"
  section.
- CHANGELOG.md and LEARNINGS.md.
