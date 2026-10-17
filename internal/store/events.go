package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
)

// SourceAgent marks events the agent wrote. They are never dispatched back
// to it; everything else is.
const SourceAgent = "agent"

// Event is the envelope every event shares (see VISION.md "The event
// stream"). Payload is arbitrary JSON, stored compacted.
type Event struct {
	Seq            int64           `json:"seq"`
	ID             string          `json:"id"`
	Owner          string          `json:"owner"`
	Source         string          `json:"source"`
	Type           string          `json:"type"`
	OccurredAt     time.Time       `json:"occurred_at"`
	ReceivedAt     time.Time       `json:"received_at"`
	ParentID       string          `json:"parent_id,omitempty"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
	Payload        json.RawMessage `json:"payload"`
	PrevHash       string          `json:"prev_hash"`
	Hash           string          `json:"hash"`
}

// ErrNotFound is returned for an unknown event id.
var ErrNotFound = errors.New("not found")

// NewID returns a ULID for the given time.
func NewID(t time.Time) string {
	return ulid.MustNew(ulid.Timestamp(t), rand.Reader).String()
}

// Append validates e, assigns id, received_at and the chain hash, and
// stores it. If e carries an idempotency key the owner has already used,
// the stored event is returned with created == false and nothing is
// written.
func (s *Store) Append(ctx context.Context, e Event) (stored Event, created bool, err error) {
	if e.Owner == "" || e.Source == "" || e.Type == "" {
		return Event{}, false, fmt.Errorf("appending event: owner, source and type are required")
	}
	if len(e.Payload) == 0 {
		e.Payload = json.RawMessage("{}")
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, e.Payload); err != nil {
		return Event{}, false, fmt.Errorf("appending event: payload is not JSON: %w", err)
	}
	e.Payload = compact.Bytes()
	now := time.Now().UTC()
	if e.ReceivedAt.IsZero() {
		e.ReceivedAt = now
	}
	if e.OccurredAt.IsZero() {
		e.OccurredAt = e.ReceivedAt
	}
	e.OccurredAt, e.ReceivedAt = e.OccurredAt.UTC(), e.ReceivedAt.UTC()

	s.mu.Lock()
	defer s.mu.Unlock()
	if e.ID == "" {
		e.ID = ulid.MustNew(ulid.Timestamp(e.ReceivedAt), s.entropy).String()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Event{}, false, fmt.Errorf("appending event: %w", err)
	}
	defer tx.Rollback()

	if e.IdempotencyKey != "" {
		prior, err := scanEvent(tx.QueryRowContext(ctx, selectEvent+` WHERE owner = ? AND idempotency_key = ?`, e.Owner, e.IdempotencyKey))
		if err == nil {
			return prior, false, nil
		}
		if !errors.Is(err, ErrNotFound) {
			return Event{}, false, fmt.Errorf("checking idempotency key: %w", err)
		}
	}

	err = tx.QueryRowContext(ctx, `SELECT hash FROM events WHERE owner = ? ORDER BY seq DESC LIMIT 1`, e.Owner).Scan(&e.PrevHash)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Event{}, false, fmt.Errorf("reading chain head: %w", err)
	}
	e.Hash = chainHash(e.PrevHash, e)

	res, err := tx.ExecContext(ctx, `INSERT INTO events
		(id, owner, source, type, occurred_at, received_at, parent_id, idempotency_key, payload, dispatch, prev_hash, hash)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, e.Owner, e.Source, e.Type, fmtTime(e.OccurredAt), fmtTime(e.ReceivedAt),
		nullable(e.ParentID), nullable(e.IdempotencyKey), string(e.Payload), e.Source != SourceAgent,
		e.PrevHash, e.Hash)
	if err != nil {
		return Event{}, false, fmt.Errorf("inserting event: %w", err)
	}
	if e.Seq, err = res.LastInsertId(); err != nil {
		return Event{}, false, fmt.Errorf("reading event seq: %w", err)
	}
	if err := apply(ctx, tx, e); err != nil {
		return Event{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return Event{}, false, fmt.Errorf("committing event: %w", err)
	}
	s.notify()
	return e, true, nil
}

// Event returns one event by id.
func (s *Store) Event(ctx context.Context, id string) (Event, error) {
	return scanEvent(s.db.QueryRowContext(ctx, selectEvent+` WHERE id = ?`, id))
}

// Events returns an owner's events after seq (0 for all), oldest first, at
// most limit of them.
func (s *Store) Events(ctx context.Context, owner string, afterSeq int64, limit int) ([]Event, error) {
	return s.queryEvents(ctx, selectEvent+` WHERE owner = ? AND seq > ? ORDER BY seq LIMIT ?`, owner, afterSeq, limit)
}

// AllEvents returns every owner's events after seq, oldest first. It is
// the feed projections are built from.
func (s *Store) AllEvents(ctx context.Context, afterSeq int64, limit int) ([]Event, error) {
	return s.queryEvents(ctx, selectEvent+` WHERE seq > ? ORDER BY seq LIMIT ?`, afterSeq, limit)
}

// Undelivered returns events that still have to reach the agent, oldest
// first. On startup this is the redelivery backlog.
func (s *Store) Undelivered(ctx context.Context, limit int) ([]Event, error) {
	return s.queryEvents(ctx, selectEvent+` WHERE dispatch = 1
		AND NOT EXISTS (SELECT 1 FROM deliveries d WHERE d.event_id = events.id)
		ORDER BY seq LIMIT ?`, limit)
}

// MarkDelivered records that the agent acknowledged the event. Marking an
// event twice is not an error.
func (s *Store) MarkDelivered(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO deliveries (event_id, delivered_at) VALUES (?, ?)
		ON CONFLICT (event_id) DO NOTHING`, id, fmtTime(time.Now()))
	if err != nil {
		return fmt.Errorf("marking %s delivered: %w", id, err)
	}
	return nil
}

// ChainError says where an owner's hash chain breaks.
type ChainError struct {
	Owner   string
	EventID string
	Reason  string
}

func (e *ChainError) Error() string {
	return fmt.Sprintf("hash chain for owner %s breaks at event %s: %s", e.Owner, e.EventID, e.Reason)
}

// Verify recomputes an owner's whole chain. It returns a *ChainError at the
// first event whose stored hashes don't match its contents.
func (s *Store) Verify(ctx context.Context, owner string) error {
	prev := ""
	var after int64
	for {
		batch, err := s.Events(ctx, owner, after, 1000)
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			return nil
		}
		for _, e := range batch {
			if e.PrevHash != prev {
				return &ChainError{owner, e.ID, "prev_hash does not match the previous event's hash"}
			}
			if chainHash(prev, e) != e.Hash {
				return &ChainError{owner, e.ID, "hash does not match the event's contents"}
			}
			prev, after = e.Hash, e.Seq
		}
	}
}

// chainHash is SHA-256 over the previous hash and every envelope field,
// each length-prefixed, so changing any field (not only the payload)
// breaks the chain from that event on.
func chainHash(prev string, e Event) string {
	h := sha256.New()
	var n [8]byte
	for _, f := range []string{prev, e.ID, e.Owner, e.Source, e.Type, fmtTime(e.OccurredAt), fmtTime(e.ReceivedAt), e.ParentID, e.IdempotencyKey, string(e.Payload)} {
		binary.BigEndian.PutUint64(n[:], uint64(len(f)))
		h.Write(n[:])
		h.Write([]byte(f))
	}
	return hex.EncodeToString(h.Sum(nil))
}

const selectEvent = `SELECT seq, id, owner, source, type, occurred_at, received_at,
	COALESCE(parent_id, ''), COALESCE(idempotency_key, ''), payload, prev_hash, hash FROM events`

type scanner interface{ Scan(...any) error }

func scanEvent(row scanner) (Event, error) {
	var e Event
	var occurred, received, payload string
	err := row.Scan(&e.Seq, &e.ID, &e.Owner, &e.Source, &e.Type, &occurred, &received,
		&e.ParentID, &e.IdempotencyKey, &payload, &e.PrevHash, &e.Hash)
	if errors.Is(err, sql.ErrNoRows) {
		return Event{}, ErrNotFound
	}
	if err != nil {
		return Event{}, fmt.Errorf("reading event: %w", err)
	}
	if e.OccurredAt, err = parseTime(occurred); err != nil {
		return Event{}, err
	}
	if e.ReceivedAt, err = parseTime(received); err != nil {
		return Event{}, err
	}
	e.Payload = json.RawMessage(payload)
	return e, nil
}

func (s *Store) queryEvents(ctx context.Context, query string, args ...any) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying events: %w", err)
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("querying events: %w", err)
	}
	return out, nil
}

// timeFormat is fixed-width (always nine fractional digits, always Z) so
// stored times sort as text in time order. RFC3339Nano trims zeros and
// doesn't.
const timeFormat = "2006-01-02T15:04:05.000000000Z"

func fmtTime(t time.Time) string { return t.UTC().Format(timeFormat) }

func parseTime(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("parsing time %q: %w", s, err)
	}
	return t, nil
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
