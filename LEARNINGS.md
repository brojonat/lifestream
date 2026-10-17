# Learnings

Pitfalls found the hard way. Each entry: what happened, why it was
surprising, how to avoid it.

## Rex has no public docs a web search can find

**What happened:** searching for rex (`rex server start --tailscale`, etc.)
turned up only generic Tailscale pages. Everything VISION.md says about rex
came from docs the user pasted in.

**Why it's surprising:** it's a product we're building around, so you'd
expect to look it up.

**How to avoid:** don't guess rex behavior from search results. Use what
VISION.md records (as of 2026-10-16) and ask the user for current docs.
Known gotchas so far: hosts added with `rex hosts` don't appear in the Mac
app, and plain HTTP needs `--allow-anonymous`, which must never be used for
buyers.

## A Sprite checkpoint can't start a new Sprite

**What happened:** the first design built one "golden" environment and
forked every buyer from its checkpoint. As far as we know, checkpoints only
roll back the Sprite that made them.

**Why it's surprising:** checkpoint/restore sounds like an image you can
clone.

**How to avoid:** build every buyer's Sprite from the recipe
(`mise run setup`), then checkpoint it as that buyer's reset point.

## Claude Code only runs Anthropic models

**What happened:** the testing plan named one cheap model (`gpt-6-luna`)
for every harness.

**Why it's surprising:** Codex and pi take models from several providers,
so it's easy to assume all three do.

**How to avoid:** pick a model per harness. Claude Code needs an Anthropic
model (Haiku 5.5 while testing).

## OpenRouter's per-buyer keys need a separate management key

**What happened:** OpenRouter can create keys with a USD spend limit
(`limit`, with daily/weekly/monthly reset) through its key management API.

**Why it's surprising:** the management key used to create them can't call
models, and older docs call these "provisioning" keys.

**How to avoid:** keep two kinds of key: one management key on the platform
side, and per-buyer keys inside Sprites. Docs:
https://openrouter.ai/docs/guides/overview/auth/management-api-keys

## Stripe Checkout Sessions don't track stock

**What happened:** paid topics can be capped to the first N subscribers.
Stripe Payment Links can stop after N completed purchases
(`restrictions.completed_sessions.limit`), but Checkout Sessions, which we
create ourselves to pre-apply the 100%-off coupon, have no inventory at all.

**Why it's surprising:** Payment Links are built on Checkout, so you'd
expect the limit to carry over.

**How to avoid:** own the stock count. Reserve a slot when creating the
session, give the session an expiry, and release the slot when it expires
unpaid, so the last slot can't sell twice. Docs:
https://docs.stripe.com/docs/payment-links/customize

## pi's npm scope was renamed

**What happened:** pi's packages moved from `@mariozechner/*` to
`@earendil-works/*`.

**Why it's surprising:** most pi tutorials and examples written before the
change still use the old scope, and nothing in them says it moved.

**How to avoid:** import from `@earendil-works/*`, and treat older pi
examples as possibly out of date.

## Codex hooks are off by default

**What happened:** Codex reads hooks from `~/.codex/hooks.json` (and
`<repo>/.codex/hooks.json`), but hooks only run once they're enabled in
`~/.codex/config.toml`.

**Why it's surprising:** a correct `hooks.json` silently does nothing.

**How to avoid:** the Codex capture install step has to enable hooks in
`config.toml` as well as writing `hooks.json`. Check that a test turn
actually reaches the server.

## Claude Code waits for Stop hooks and reads their stdout

**What happened:** the capture hook runs on `Stop` and `SessionEnd`.
Claude Code waits for a Stop hook to finish, parses its stdout as a
decision, and treats exit 2 as "don't stop". SessionEnd hooks share a
1.5 s budget.

**Why it's surprising:** it looks like a fire-and-forget notification.

**How to avoid:** print nothing on stdout, never exit 2, keep HTTP
timeouts short, and exit 0 when the service is unreachable (the transcript
is the retry buffer). Docs: https://code.claude.com/docs/en/hooks.md

## Claude Code transcripts hold file backups

**What happened:** besides `user`/`assistant`/`system` lines, transcripts
carry `file-history-delta` lines with file backups, plus titles and modes.

**Why it's surprising:** "send the transcript" sounds like sending the
conversation.

**How to avoid:** the Claude Code parser sends only `user`, `assistant`
and `system` lines. Check a real transcript's line types (`jq .type`)
before adding a harness.

## Codex notify passes JSON as the last argument, without a transcript path

**What happened:** with `notify = ["lifestream", "capture", "codex"]`,
Codex 0.56 appends `{"type":"agent-turn-complete","thread-id",
"turn-id","input-messages","last-assistant-message"}` as argv, not stdin,
and gives no transcript path or cwd. Rollouts are plain JSONL (not zstd)
under `~/.codex/sessions/YYYY/MM/DD/rollout-*-<thread-id>.jsonl`. The
model is only named in `turn_context` lines, and preamble messages come
before the first one.

**Why it's surprising:** every other harness hands over the transcript
path on stdin.

**How to avoid:** take hook JSON from an argument too, find the rollout by
thread id under `$CODEX_HOME`, read cwd from `session_meta`, and carry the
model across lines in a per-run parser.

## pi's `exec` can't pass stdin; the installed pi may still be the old scope

**What happened:** pi writes sessions as JSONL under
`~/.pi/agent/sessions/--<cwd>--/`, so the extension calls the tailer.
`pi.exec(command, args, options)` has no stdin, and the pi on this machine
was `@mariozechner/pi-coding-agent` 0.67.6 while npm has
`@earendil-works/*` 1.3.0.

**How to avoid:** spawn with `node:child_process` to pipe the hook JSON,
and declare the few pi types locally instead of importing them, so the
extension loads under either scope.

## A `.ts` file outside a `"type": "module"` package typechecks as CommonJS

**What happened:** `agent/test` imported `capture/pi/lifestream.ts`; Node
ran it fine (syntax detection), but `tsc` said the default export "has no
call signatures".

**How to avoid:** give the directory a `package.json` with
`{"type": "module"}`.

## Go's regexp has no lookbehind, and decoding JSON into maps reorders keys

**What happened:** porting the scanner, the phone rule
`(?<![\w+])\+...` doesn't compile in RE2. Scanning a message by decoding
it into `map[string]any` and re-encoding would have sorted its keys, and
`json.Decoder.Token` returns plain `io.EOF` on a truncated document.

**How to avoid:** capture the preceding character as its own group and
keep it; walk JSON token by token, rewriting string values only; reject
input that ends with containers still open. One corpus
(`testdata/secrets.json`) pins the TypeScript, Go and Python scanners.

## html/template treats `data-on:*` attributes as JavaScript

**What happened:** a topic name goes inside
`data-on:submit="confirm('… {{.Name}}?') && @post(…)"`. html/template
strips the `data-` prefix, sees `on…`, and escapes the value for a JS
string (`'` becomes `\u0027`).

**Why it's surprising:** you'd expect `data-*` to be plain attributes.

**How to avoid:** rely on it, and keep a test that a quote in user text
can't break out (`TestTopicNameIsEscapedInsideTheConfirm`).

## New projections start empty on a database that already has events

**What happened:** migrations add `session_messages`, `topics`,
`member_order` and the rest as empty tables; `apply` only fills them for
new events.

**How to avoid:** after deploying a migration that adds a projection,
run `lifestream rebuild`.

## Members added by a projection never pass through the agent

**What happened:** session messages join their session's collections in
Go, with no model call, so no tool call ever started their redaction.

**How to avoid:** the agent sweeps `GET /api/unredacted` on a timer, and
every redaction task starts through a per-conversation ledger so the tool
path, the look-back and the sweep never start two for one event.

## Every non-agent event is dispatched unless excluded

**What happened:** topic, notification and ordering events from the
dashboard went to the agent like SMS, each costing a model call and
showing up in the inbox.

**How to avoid:** add types the agent has no part in to `notForAgent` in
`internal/store/events.go`.

## Pi Durable: task ids, faulting phases, background reports

**What happened:** provider tool-call ids repeat, so they can't be
idempotency keys; a task phase that throws faults the task for good;
background tasks report back by submitting a follow-up input to the
conversation (`requestId` keeps it single); and the 1.x API changed twice
before 1.3.0.

**How to avoid:** use the Pi Durable task id (`api.taskId`) as the
idempotency key, catch errors in phases and retry with a persisted backoff
(`agent/src/retry.ts`), and pin the version exactly.

## Tests that wait on a conversation don't wait for its background tasks

**What happened:** the look-back test waited for labels and for the
conversation to go idle, then checked metrics the background task hadn't
written yet. It failed once under load. A later test counted redaction
copies an earlier test's tasks were still writing.

**How to avoid:** wait for the writes themselves, and filter assertions to
the events the test made.

## Strands' message index isn't a stable sequence

**What happened:** conversation managers trim `agent.messages`, so its
index shifts and would collide `(session_id, sequence)` keys.

**How to avoid:** the hook provider keeps its own counter and one session
id per provider.

## claude.ai can share an artifact with specific people

**What happened:** (TODO A4) the owner can invite people by email; they
open it signed in to Claude with that address. On Pro and Max the share
options are "Only you" or "Anyone with the link", plus invites. Team and
Enterprise can limit sharing to the organization; Enterprise has invites
outside the organization off by default. No API for invites was found.

**How to avoid:** link delivery can be enforced by inviting the buyer's
checkout email, by hand for now. Docs:
https://support.claude.com/en/articles/9547008-share-artifacts

## Inherited from the go-local-app template

**Embedded NATS with no TCP port:** `server.Options{DontListen: true}` plus
`nats.Connect("", nats.InProcessServer(srv))`. Set `ServerName`; JetStream
wants one.

**`kv.WatchAll(ctx, jetstream.UpdatesOnly())` is the read-loop shape:**
render current state first, then block on `Updates()`. Without
`UpdatesOnly` the watcher replays the bucket and sends a nil marker.

**`kv.ListKeys` on an empty bucket returns `jetstream.ErrNoKeysFound`,** not
an empty list.

**Every KV bucket is a stream (`KV_<name>`),** so stream counts include them.

**JetStream `AccountInfo().Limits` reports unlimited** for the default
account even when the server was started with a max store; carry the
configured ceiling from the bus.

**A textarea the user types into must be `data-ignore-morph`,** or every
re-render replaces what they are typing. Save with a debounced `@put` and
let "saved 3s ago" come from the server.

**Wrapping `http.ResponseWriter` for logging breaks SSE** unless the wrapper
implements `http.Flusher`.

**Inputs that carry values to the backend should submit as form fields**
(`{contentType: 'form'}`), not signals: a shared signal name makes every
input on the page converge on the last typed value.

**`curl -sN -m 2 .../stream` exits 28.** That is the stream working.

**If Air is killed hard, its child keeps the port.** `mise run stop` kills
whatever listens on 8080.

## Inherited from python-warehouse-dashboard: Datastar and Rocket

**Use an open shadow root for anything d3 draws.** Datastar's morph syncs a
host element's attributes and then morphs its *light-DOM* children toward
the server's markup. The server sends an empty `<chart-x ...></chart-x>`, so
in Rocket's `light` mode every patch would morph away d3's SVG. In
`mode: 'open'` the chart lives in the shadow root and the morph only updates
attributes, which flow through the prop codec to `observeProps` and d3.

**Put an `id` on every component host** so the morph keeps the same element
instance and transitions run, and **set `renderOnPropChange: false`**
(otherwise Rocket re-runs `render()` and replaces the skeleton d3 draws
into).

**`datastar-rocket.js` is a complete Datastar bundle plus Rocket,** not an
add-on. Load it instead of `datastar.js`, never both.

**Rocket's `emit(type, 'CA')` sends no detail.** A string second argument is
treated as another event name. Always emit an object:
`emit('chart-state', { state: 'CA' })`.

**Clearing a form after `@post` is racy unless you use `payload`.** With
`{contentType: 'form'}` the FormData is built inside the action's async
flow, so `el.reset()` right after can empty it first. `{payload: {...}}` is
evaluated synchronously. Pair it with `data-ignore-morph` on inputs.
