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
