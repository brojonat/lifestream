# Pruned

On 2026-10-19 the project refocused on its core: an always-on Pi Durable
agent that receives every event and turns it into structured data (see
[VISION.md](VISION.md)). Everything about selling the data was cut so it can
come back later as explicit features, not as part of the agent.

**Everything below is intact at commit `3697500`** (tag `pre-prune`), the
last commit before the prune; the prune itself is `2b55e9d`. To look at or
bring back a piece:

```
git show 3697500:VISION.md                     # the old monetization-first spec
git show 3697500:internal/store/topics.go      # any one file
git checkout 3697500 -- internal/store/topics.go internal/store/topics_test.go
                                               # restore files into the tree
git show fb9a1e0                               # the commit that built a feature
```

Restoring a whole feature is usually simplest by reverting the prune commit
for just those paths, or by cherry-picking the commits listed below and
resolving conflicts.

## Code

| Feature | What it did | Built in | Files at `3697500` |
| --- | --- | --- | --- |
| Paid notifications (topics) | Topics with one-time or monthly price, subscriptions, a manual send form with a confirmed subscriber count, history start per topic. Events `topic.created/amended/subscribed/unsubscribed`, `notification.sent` | `fb9a1e0`, `eca121e`, `2260616` | `internal/store/topics.go`, `topics_test.go`, `internal/web/topics.go`, `templates/topics.html`; migration 6 (`topics`, `subscriptions`, `notifications`) and the `notForAgent` map in `internal/store/store.go` / `events.go` |
| Artifacts | `artifact.saved` events (uploaded HTML or a claude.ai link) with payload validation, and an `/artifacts` page | `7bdbd99` | `internal/store/artifacts.go`, `artifacts_test.go`, `internal/web/artifacts.go`, `templates/artifacts.html`; validation in `internal/ingest/events.go` and `store/events.go` |
| Chapter order | `collection.ordered`: the owner's member order for a collection (a book's chapters), up/down buttons on `/c/{id}` | `da568d8` | `internal/store/projections.go` (`CollectionOrdered`, `member_order` in `MemberEvents`), migration 7 in `store.go`, `internal/web/collections.go` (`moveMember`) |
| Redaction | A `redaction.created` copy of every labeled event: deterministic scanner, then a model review in 12 KB chunks against the rules doc, as a background task with a per-conversation ledger, plus a timer sweep for session members (`GET /api/unredacted`) | `26d7faa`, `eb24771` | `agent/src/redaction.ts`, `agent/src/scanner.ts`, `agent/test/scanner.test.ts`; redaction hooks in `agent/src/agent.ts`, `collections.ts`, `lookback.ts`, `main.ts` (`SWEEP_SECONDS`); `Unredacted` in `internal/store/sessions.go`; the route in `internal/api/api.go`; the "redacted copy" badge in the inbox |

Kept because they serve the data handler: the Go secret scanner
(`internal/scan`) and its Python twin, which mask secrets in capture before
anything leaves the machine, and the shared corpus `testdata/secrets.json`.
The TypeScript scanner was the third twin and went with redaction.

Migrations 6 and 7 were removed instead of reversed with a new migration:
no database had run them (no data dir, no deployment). If you restore
them, append them after whatever migrations exist by then. Don't put them
back at their old positions.

## Docs and plans

| What | Where at `3697500` |
| --- | --- |
| "Patreon for data" product spec: products and channels, network effects, trust/privacy/licensing (provenance, watermarking, model-provider terms), demand risks, five-phase plan to paid products, HN launch plan with a $0 Stripe checkout | `VISION.md` (whole file) |
| Rex / Sprite environments as products: per-buyer Firecracker VMs built from a `mise.toml` recipe, OpenRouter keys with spend limits, rex over Tailscale | `VISION.md` "Environments as products (rex)"; `TODO.md` "Rex environments" (R1–R9, R-D1, R-D2). Moved to its own project on 2026-10-17 |
| Artifact textbooks: books as ordered collections, hosted viewer with watermark, claude.ai link delivery, launch checkout | `VISION.md` "Claude artifacts as textbooks"; `TODO.md` A1–A8 |
| Paid notifications design: caps, first-N slot reservation, subscriber inbox | `VISION.md` "Paid notifications"; `TODO.md` N1–N6 |
| Research on the above: rex has no public docs, Sprite checkpoints can't seed new Sprites, Claude Code only runs Anthropic models, OpenRouter management keys, Stripe Checkout has no stock, claude.ai artifact sharing by email | `LEARNINGS.md` (the matching entries) |

## Capture clients and sessions (2026-10-20)

Clients are not the server's concern: many clients will push events, and
the server's contract is `POST /events`. The capture adapters moved to
their own repo, `~/projects/lifestream-clients` (created from this repo at
`b7b5060`), which ships them as first-class clients with their own tests.
The server dropped everything that knew about harness sessions; clients now
choose their own granularity (one event per turn, say) and send their own
`idempotency_key`.

Everything below is intact at `b7b5060`.

| What | Where at `b7b5060` | Now |
| --- | --- | --- |
| Claude Code plugin, pi extension, Strands hook provider, plugin marketplace | `capture/`, `.claude-plugin/`, `agent/test/pi-capture.test.ts` | lifestream-clients |
| `lifestream capture <harness>`: transcript tailer with saved offsets | `cmd/lifestream/capture.go`, `internal/capture/` | lifestream-clients, as `lifestream-capture` |
| Deterministic secret scanner (Go and Python twins, shared corpus) | `internal/scan/`, `capture/strands/.../scan.py`, `testdata/secrets.json` | lifestream-clients |
| Harness fixtures | `testdata/claude-code`, `codex`, `pi` | lifestream-clients |
| Sessions in the server: `llm.message` keyed by `(session_id, sequence)`, only a session's first 3 events dispatched, the rest joining its collections by projection | `internal/store/sessions.go`, `store/events.go` (session ref, dispatch rank), `ingest/events.go`, `projections.go` (`applySession`, `addSession`, `removeSession`) | removed; a later migration drops `session_events` |
| Common-format view of sessions across harnesses, `lifestream sessions` | `internal/transcript/`, `session_messages` table, `cmd/lifestream/sessions.go` | removed; a later migration drops `session_messages`. The agent can build views with `query` |

Old `llm.message` events stay in the log as ordinary events.
