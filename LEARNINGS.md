# Learnings

Pitfalls found the hard way. Each entry: what happened, why it was
surprising, how to avoid it. Entries about the capture adapters and
harnesses moved with them to `~/projects/lifestream-clients` (2026-10-20).

## pi's npm scope was renamed

**What happened:** pi's packages moved from `@mariozechner/*` to
`@earendil-works/*`.

**Why it's surprising:** most pi tutorials and examples written before the
change still use the old scope, and nothing in them says it moved.

**How to avoid:** import from `@earendil-works/*`, and treat older pi
examples as possibly out of date.

## html/template treats `data-on:*` attributes as JavaScript

**What happened:** a topic name (since pruned) went inside
`data-on:submit="confirm('… {{.Name}}?') && @post(…)"`. html/template
strips the `data-` prefix, sees `on…`, and escapes the value for a JS
string (`'` becomes `\u0027`).

**Why it's surprising:** you'd expect `data-*` to be plain attributes.

**How to avoid:** rely on it, and when user text goes inside a `data-on:*`
attribute, add a test that a quote can't break out. The original,
`TestTopicNameIsEscapedInsideTheConfirm`, is at tag `pre-prune`.

## New projections start empty on a database that already has events

**What happened:** migrations add projection tables (`records`, and earlier
`topics` and `member_order`) empty; `apply` only fills them for new
events.

**How to avoid:** after deploying a migration that adds a projection,
run `lifestream rebuild`.

## Every non-agent event is dispatched unless excluded

**What happened:** topic, notification and ordering events from the
dashboard went to the agent like SMS, each costing a model call and
showing up in the inbox. The fix, a `notForAgent` map, went with those
types in the prune; today every event not written by the agent is
dispatched.

**How to avoid:** when you add an event type the agent has no part in,
give `Append` in `internal/store/events.go` a way to skip it (the old map
is at tag `pre-prune`).

## Pi Durable: task ids, faulting phases, background reports

**What happened:** provider tool-call ids repeat, so they can't be
idempotency keys; a task phase that throws faults the task for good;
background tasks report back by submitting a follow-up input to the
conversation (`requestId` keeps it single); and the 1.x API changed twice
before 1.3.0.

**How to avoid:** use the Pi Durable task id (`api.taskId`) as the
idempotency key (a nested call from a script has its own), catch errors in
phases that can fail transiently and retry with a persisted backoff (the
old look-back's `agent/src/retry.ts`, in git before 2026-10-20), and pin
the version exactly.

## Tests that wait on a conversation don't wait for its background tasks

**What happened:** the look-back test waited for labels and for the
conversation to go idle, then checked metrics the background task hadn't
written yet. It failed once under load. A later test counted redaction
copies an earlier test's tasks were still writing.

**How to avoid:** wait for the writes themselves, and filter assertions to
the events the test made.

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

## Pi Durable loads nothing implicitly

**What happened:** pi the coding agent reads `.pi/` and `AGENTS.md` from
its working directory. Pi Durable is only the harness under it: a
conversation gets exactly the model, instructions, extensions and tools
passed in code (Hugging Face's walkthrough, 2026-10-19).

**Why it's surprising:** it's "pi", so you'd expect pi's project files to
apply.

**How to avoid:** everything the agent should know goes in an extension
section or the rules doc (`agent/src/data.ts`). A markdown file in the repo
does not reach it.

## Features threaded through later commits don't revert cleanly

**What happened:** pruning topics, artifacts and chapter order was one
`git revert --no-commit` of their commits, with no conflicts. Redaction had
been wired into the label tool, the look-back and session handling by
later commits, so it had to come out by hand across nine files.

**How to avoid:** land a feature in its own commits and files, and reach
it through one seam (an extension, a route, an event type) so it can be
cut with a revert. Record anything you cut in PRUNED.md.

## The `pre-prune` tag shows up in build versions

**What happened:** `mise run build` stamps `git describe --tags --always`,
so after tagging `pre-prune` the version reads `pre-prune-N-g<sha>`
instead of a bare commit hash.

**How to avoid:** expect it, or tag a real version (`v0.1.0`) when phase 1
ships so describe uses that.

## Tests inherit `.env` through mise

**What happened:** after `.env` gained `LIFESTREAM_AGENT_URL`, `mise run
test` sent the Strands adapter test's events (that adapter is in
lifestream-clients now) to the running dev agent, which processed them with
the real model. The test started `lifestream serve` without `--agent-url`,
so the server read the variable from the environment.

**Why it's surprising:** the test uses a temp data dir and random ports,
so it looks isolated. But mise loads `.env` into every task, tests
included, and urfave/cli flags fall back to env vars.

**How to avoid:** anything a test spawns gets an environment without
`LIFESTREAM_*` / `TWILIO_*`, or passes every flag explicitly, as
`agent/test/e2e.test.ts` does. A real-model smoke stack does the same:
`env -i PATH=... HOME=...` plus explicit addresses.

## Traefik breaks route ties by rule length

**What happened:** with the dashboard ingress on `/` (Prefix, basic auth)
next to the ingest ingress on `/events` (Exact), `POST /events` got the
basic-auth 401. `/twilio/sms` on the same ingress worked.

**Why it's surprising:** Exact looks more specific than a `/` prefix.
Traefik doesn't care: a router's default priority is its rule's length,
and ``Path(`/events`)`` is exactly as long as ``PathPrefix(`/`)``.

**How to avoid:** give the catch-all router an explicit
`traefik.ingress.kubernetes.io/router.priority: "1"` (done in
`deploy/k8s/dashboard.yaml`). Test every public path after adding an
ingress, with and without credentials.

## `firekeeper` ran out of inotify instances

**What happened:** `kubectl logs -f` died at once with "failed to create
fsnotify watcher: too many open files"; plain `kubectl logs` worked.

**Why it's surprising:** it looks like a kubectl or API problem. It's the
node: `fs.inotify.max_user_instances` was the default 128, and root (k3s
plus one containerd shim per container) held nearly all of them.

**How to avoid:** the node now has `/etc/sysctl.d/90-inotify.conf`
(8192 instances, 524288 watches). There's no SSH key for it from this
machine; a privileged pod with `hostPID` and `/` mounted at `/host` plus
`chroot /host` does node admin through kubectl. Delete the pod after.

## Cloudflare: R2 buckets and scoped keys without the dashboard

**What happened:** the R2 keys in other projects' `.env.prod` are
bucket-scoped and can't create buckets, and the `CLOUDFLARE_CREATE_TOKEN`
in `~/projects/iybi-twc/.env` answered "Authentication error", which looked
like a dead token.

**Why it's surprising:** that token is a *user* token, so it fails on
`/accounts/{id}/tokens/verify` and works on `/user/tokens/...`. And R2's S3
credentials aren't a separate thing: any API token with R2 permissions is
one (access key id = token id, secret = sha256 hex of the token value).

**How to avoid:** create the bucket with `POST
/accounts/{acct}/r2/buckets` (iybi-twc's `CLOUDFLARE_API_TOKEN` can), then
`POST /user/tokens` with the create token, resource
`com.cloudflare.edge.r2.bucket.<acct>_default_<bucket>` and permission
group "Workers R2 Storage Bucket Item Write". Derive the two keys. That's
how `lifestream` got its bucket and token.

## `air` is per Go version under mise

**What happened:** `mise run dev` failed with "No version is set for shim:
air" although `air` was installed.

**Why it's surprising:** `go install` puts binaries in the bin of the Go
that ran it. `air` had been installed under Go 1.25; this project pins
1.27, so mise's shim found no `air` for it.

**How to avoid:** `mise run setup` (it runs `go install ...air@latest`
under the pinned Go) after changing the Go version, then `mise reshim`.

## A pipe into `tee` hides failures

**What happened:** `mise run tunnel` and `mise run dev` failed and mise
reported exit code 0.

**Why it's surprising:** a pipeline's status is its last command's, and
`tee` succeeds.

**How to avoid:** tasks that tee start with `set -o pipefail;` (dev,
dev-agent and tunnel do).

## Code mode: scripts get JSON only from tools that declare an output schema

**What happened:** with Pi Durable's code mode, a script calls a tool as
`await tools.query(...)`. A tool without `structuredOutputSchema` hands the
script its text output; one with a schema hands it the validated
`structuredOutput`, and a failed call rejects in the script with the
error text.

**Why it's surprising:** the same tool result reads fine to the model
either way, so the gap only shows when a script tries `r.rows`.

**How to avoid:** give every tool a script calls a `structuredOutputSchema`
and return `structuredOutput` (`agent/src/data.ts`). Mark them
`callers: ["tools"]` so the model is offered only `codemode`. To check what
the model is offered, read `conversation.agent(ctx).tools`: tool
definitions travel inside the transcript, not as a field of the provider
context a fake model sees.

## A conversation keeps the model it was created with

**What happened:** changing `AGENT_MODEL` from Haiku to Sonnet changed new
conversations only; the owner's existing one is stored with its own model
(its `pi.agent` document).

**How to avoid:** compare `conversation.agent(ctx).model` to the configured
one and `configure({model})` when they differ. `Agent.conversationFor`
does it once per owner per process.

## Background work can be a full agent: a task that owns a subagent

**What happened:** the look-back was a fixed JSON judge because a
background task can't call tools. But a task can own a conversation:
inside `runtime.commit`, `tx.createConversation({ownership: {kind: "task",
taskId}})` creates a child that starts as a copy of the task's
conversation's agent (model, extensions, tools, sections). Submit the job
to it with a fixed `requestId` and wait. The child has its own document
scope, so it needs its own owner doc.

**Why it's surprising:** `runtime.entry(id)` only sees entries visible from
the task's own conversation, so reading the child's answer by the
submission's `answer` id returned nothing. And the steward's prompt
mentions "Look-back job", so a test that routes fake-model replies by that
phrase sent every conversation down the child's path.

**How to avoid:** checkpoint the child's id in the same commit that creates
it; read its reply with `runtime.context(childId).messages`; in tests,
match the job message's exact opening.

## A capable model takes shortcuts the design didn't plan for

**What happened:** given `query`, Sonnet started a collection with
`lookback_days: 0` and backfilled in the same script, so the background
job never ran (fine for five texts, blocking for a thousand). It also
started a "Sleep" collection with a unit but no `kind: "metric"`, and the
look-back child repaired it by starting a second collection.

**How to avoid:** say what to leave to the background job in the prompt,
and refuse impossible states in Go (a set with a unit is a 422) so the
model corrects its call at once. Smoke-test prompt changes against the real
model in a temp stack; fakes only test plumbing.

## `pgrep -f` / `pkill -f` match the shell that runs them

**What happened:** `pkill -f 'ngrok http ...'` and `kill $(pgrep -f
'port-forward ...')` killed the very bash command running them (exit 144),
three times. Separately, a smoke-test agent left on a fixed port meant the
next run's agent failed to bind and the old one (old prompt) answered.

**How to avoid:** kill by `$!`, a pid file, `pgrep -x <name>`, or
`lsof -t -iTCP:<port> -sTCP:LISTEN`; check each fixed port is free before
starting a stack.
