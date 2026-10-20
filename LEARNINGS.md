# Learnings

Pitfalls found the hard way. Each entry: what happened, why it was
surprising, how to avoid it.

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

**What happened:** a topic name (since pruned) went inside
`data-on:submit="confirm('… {{.Name}}?') && @post(…)"`. html/template
strips the `data-` prefix, sees `on…`, and escapes the value for a JS
string (`'` becomes `\u0027`).

**Why it's surprising:** you'd expect `data-*` to be plain attributes.

**How to avoid:** rely on it, and when user text goes inside a `data-on:*`
attribute, add a test that a quote can't break out. The original,
`TestTopicNameIsEscapedInsideTheConfirm`, is at tag `pre-prune`.

## New projections start empty on a database that already has events

**What happened:** migrations add `session_messages`, `topics`,
`member_order` and the rest as empty tables; `apply` only fills them for
new events.

**How to avoid:** after deploying a migration that adds a projection,
run `lifestream rebuild`.

## Members added by a projection never pass through the agent

**What happened:** session messages join their session's collections in
Go, with no model call, so no tool call ever started the (since pruned)
redaction of those members.

**How to avoid:** per-member agent work can't hang off `label_event`
alone. Sweep for members missing the work on a timer, and start each task
through a ledger so the tool path, the look-back and the sweep never start
two for one event. The redaction sweep at tag `pre-prune` is the worked
example.

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
section or the rules doc (`agent/src/collections.ts`, `rules.ts`). A
markdown file in the repo does not reach it.

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
test` sent the Strands adapter test's events to the running dev agent,
which processed them with the real model. The test starts `lifestream
serve` without `--agent-url`, so the server read the variable from the
environment.

**Why it's surprising:** the test uses a temp data dir and random ports,
so it looks isolated. But mise loads `.env` into every task, tests
included, and urfave/cli flags fall back to env vars.

**How to avoid:** anything a test spawns gets an environment without
`LIFESTREAM_*` / `TWILIO_*` (see `capture/strands/tests/test_capture.py`),
or passes every flag explicitly, as `agent/test/e2e.test.ts` does.

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
