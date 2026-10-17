# Lifestream

Read `README.md` (interface), then `TODO.md` (work in flight), then
`LEARNINGS.md` (what has bitten people). `VISION.md` is the product spec;
TODO.md's "Decisions already made" are settled, don't re-litigate them.

`mise tasks` lists every command: `setup`, `dev`, `dev-agent`, `test`,
`lint`, `build`, `stop`. Dev output is teed to `logs/`.

Rules of the road:

- Events are append-only. Never write an UPDATE or DELETE against `events`.
  Corrections are new events; views are projections rebuilt from events.
- The agent writes only through the internal API. Nothing in `agent/` opens
  the Go database.
- The server owns state. Pages render from the store; writes mutate and
  answer 204; the read stream repaints. Don't render from a write.
- Tests use temp dirs. Never point a test at a real data dir.
