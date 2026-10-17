# Changelog

All notable changes to this project are recorded here, newest first. The
format follows [Keep a Changelog](https://keepachangelog.com/). No code has
shipped yet, so entries are grouped by date.

## 2026-10-17

### Changed

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
