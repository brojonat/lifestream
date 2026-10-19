// Package store is the SQLite database behind the service: the append-only
// event log (hash-chained per owner), the users allowlist, dispatch state,
// and the projections rebuilt from events. One file, one writer.
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
	"net/url"
	"sync"

	"github.com/oklog/ulid/v2"
	_ "modernc.org/sqlite"
)

// migrations run in order; PRAGMA user_version records how many have run.
// Append new ones; never edit one that has shipped.
var migrations = []string{
	`CREATE TABLE users (
		id         TEXT PRIMARY KEY,
		name       TEXT NOT NULL,
		phone      TEXT NOT NULL UNIQUE,
		token_hash TEXT NOT NULL UNIQUE,
		created_at TEXT NOT NULL
	);

	CREATE TABLE events (
		seq             INTEGER PRIMARY KEY AUTOINCREMENT,
		id              TEXT NOT NULL UNIQUE,
		owner           TEXT NOT NULL REFERENCES users(id),
		source          TEXT NOT NULL,
		type            TEXT NOT NULL,
		occurred_at     TEXT NOT NULL,
		received_at     TEXT NOT NULL,
		parent_id       TEXT REFERENCES events(id),
		idempotency_key TEXT,
		payload         TEXT NOT NULL,
		dispatch        INTEGER NOT NULL,
		prev_hash       TEXT NOT NULL,
		hash            TEXT NOT NULL
	);
	CREATE UNIQUE INDEX events_idem ON events(owner, idempotency_key) WHERE idempotency_key IS NOT NULL;
	CREATE INDEX events_owner_seq ON events(owner, seq);
	CREATE INDEX events_parent ON events(parent_id) WHERE parent_id IS NOT NULL;

	CREATE TRIGGER events_no_update BEFORE UPDATE ON events
	BEGIN SELECT RAISE(ABORT, 'events are append-only'); END;
	CREATE TRIGGER events_no_delete BEFORE DELETE ON events
	BEGIN SELECT RAISE(ABORT, 'events are append-only'); END;

	CREATE TABLE deliveries (
		event_id     TEXT PRIMARY KEY REFERENCES events(id),
		delivered_at TEXT NOT NULL
	);`,

	// Projections: rebuilt from events by Rebuild, so no foreign keys.
	`CREATE TABLE collections (
		id            TEXT PRIMARY KEY, -- the collection.started event id
		owner         TEXT NOT NULL,
		name          TEXT NOT NULL,
		kind          TEXT NOT NULL,
		metric        TEXT,
		unit          TEXT,
		status        TEXT NOT NULL,
		version       INTEGER NOT NULL,
		lookback_days INTEGER,
		created_at    TEXT NOT NULL,
		updated_at    TEXT NOT NULL
	);
	CREATE INDEX collections_owner ON collections(owner, status);

	CREATE TABLE collection_rules (
		collection_id TEXT NOT NULL,
		version       INTEGER NOT NULL,
		kind          TEXT NOT NULL,
		text          TEXT NOT NULL,
		event_id      TEXT NOT NULL,
		PRIMARY KEY (collection_id, event_id)
	);

	CREATE TABLE collection_members (
		collection_id      TEXT NOT NULL,
		event_id           TEXT NOT NULL,
		definition_version INTEGER NOT NULL,
		reason             TEXT NOT NULL,
		label_event_id     TEXT NOT NULL,
		labeled_at         TEXT NOT NULL,
		PRIMARY KEY (collection_id, event_id)
	);
	CREATE INDEX collection_members_event ON collection_members(event_id);

	CREATE TABLE metric_values (
		event_id        TEXT PRIMARY KEY, -- the metric.observed event
		owner           TEXT NOT NULL,
		collection_id   TEXT NOT NULL,
		source_event_id TEXT,
		metric          TEXT NOT NULL,
		value           REAL NOT NULL,
		unit            TEXT,
		observed_at     TEXT NOT NULL
	);
	CREATE INDEX metric_values_collection ON metric_values(collection_id, observed_at);`,

	// The rules doc: the owner's standing instructions, one row per entry.
	`CREATE TABLE rules (
		id         TEXT PRIMARY KEY, -- the rule.added event id
		owner      TEXT NOT NULL,
		text       TEXT NOT NULL,
		source     TEXT NOT NULL,
		created_at TEXT NOT NULL
	);
	CREATE INDEX rules_owner ON rules(owner, created_at);`,

	// Sessions: which llm.message events share a session_id.
	`CREATE TABLE session_events (
		event_id   TEXT PRIMARY KEY,
		owner      TEXT NOT NULL,
		session_id TEXT NOT NULL,
		sequence   INTEGER NOT NULL
	);
	CREATE INDEX session_events_session ON session_events(owner, session_id);`,

	// The common-format view of sessions (internal/transcript), one row per
	// llm.message: rebuilt from the raw events whenever the format changes.
	`CREATE TABLE session_messages (
		event_id    TEXT PRIMARY KEY,
		owner       TEXT NOT NULL,
		session_id  TEXT NOT NULL,
		sequence    INTEGER NOT NULL,
		harness     TEXT NOT NULL,
		model       TEXT,
		cwd         TEXT,
		role        TEXT NOT NULL,
		parts       TEXT NOT NULL,
		occurred_at TEXT NOT NULL
	);
	CREATE INDEX session_messages_session ON session_messages(owner, session_id, sequence);`,
}

// Store is the open database. Writes are serialized by mu so the hash chain
// is read and extended atomically; SQLite would serialize them anyway.
type Store struct {
	db *sql.DB

	mu      sync.Mutex
	changed chan struct{}          // closed and replaced on every append
	entropy *ulid.MonotonicEntropy // event ids increase within a millisecond
}

// Open opens (creating if needed) the database at path and migrates it.
func Open(ctx context.Context, path string) (*Store, error) {
	q := url.Values{}
	for _, p := range []string{"journal_mode(WAL)", "busy_timeout(5000)", "foreign_keys(1)", "synchronous(NORMAL)"} {
		q.Add("_pragma", p)
	}
	q.Set("_txlock", "immediate")
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	s := &Store{db: db, changed: make(chan struct{}), entropy: ulid.Monotonic(rand.Reader, 0)}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("reading schema version: %w", err)
	}
	for i := version; i < len(migrations); i++ {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: setting version: %w", i+1, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("migration %d: commit: %w", i+1, err)
		}
	}
	return nil
}

// Changed returns a channel that is closed on the next append. Read it,
// render, then block on it; a close means "re-read".
func (s *Store) Changed() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.changed
}

func (s *Store) notify() {
	close(s.changed)
	s.changed = make(chan struct{})
}
