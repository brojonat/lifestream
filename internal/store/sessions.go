package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// TypeLLMMessage is one message, tool call or tool result from a harness
// session, pushed by a capture adapter as it happens (VISION.md "Capture
// from harnesses"). A session is the events that share a session_id.
const TypeLLMMessage = "llm.message"

// SessionHead is how many events of a session reach the agent. The agent
// labels a session from its first events; the rest join the session's
// collections through the projection, with no model call.
const SessionHead = 3

// SessionRef is the part of an llm.message payload the store reads. The
// adapter puts the harness's own message JSON, unchanged, beside it (in
// "message") along with where it came from (harness, model, cwd, git).
type SessionRef struct {
	SessionID string `json:"session_id"`
	Sequence  *int64 `json:"sequence"`
}

// ParseSessionRef validates an llm.message payload.
func ParseSessionRef(payload json.RawMessage) (SessionRef, error) {
	var ref SessionRef
	if err := json.Unmarshal(payload, &ref); err != nil {
		return SessionRef{}, invalid("%s payload: %v", TypeLLMMessage, err)
	}
	if strings.TrimSpace(ref.SessionID) == "" || ref.Sequence == nil || *ref.Sequence < 0 {
		return SessionRef{}, invalid("%s needs session_id and a sequence >= 0", TypeLLMMessage)
	}
	return ref, nil
}

// sessionKey is an llm.message's idempotency key, so an adapter can resend
// a batch without duplicates.
func sessionKey(ref SessionRef) string {
	return TypeLLMMessage + ":" + ref.SessionID + ":" + strconv.FormatInt(*ref.Sequence, 10)
}

// sessionRank is how many of the session's events are already stored.
func sessionRank(ctx context.Context, tx *sql.Tx, owner, sessionID string) (int, error) {
	var n int
	err := tx.QueryRowContext(ctx, `SELECT count(*) FROM session_events WHERE owner = ? AND session_id = ?`, owner, sessionID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("counting session events: %w", err)
	}
	return n, nil
}

// applySession records an llm.message in its session and adds it to every
// open collection the session already belongs to.
func applySession(ctx context.Context, tx *sql.Tx, e Event) error {
	ref, err := ParseSessionRef(e.Payload)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO session_events (event_id, owner, session_id, sequence) VALUES (?, ?, ?, ?)`,
		e.ID, e.Owner, ref.SessionID, *ref.Sequence); err != nil {
		return fmt.Errorf("projecting %s: %w", e.Type, err)
	}
	// One existing member per collection carries the label over: its
	// version and label event, the earliest one by event id.
	_, err = tx.ExecContext(ctx, `INSERT INTO collection_members
		(collection_id, event_id, definition_version, reason, label_event_id, labeled_at)
		SELECT m.collection_id, ?, m.definition_version, 'same session as ' || m.event_id, m.label_event_id, ?
		FROM collection_members m
		JOIN session_events s ON s.event_id = m.event_id
		JOIN collections c ON c.id = m.collection_id
		WHERE s.owner = ? AND s.session_id = ? AND s.event_id != ? AND c.status = 'open'
		AND m.event_id = (SELECT min(m2.event_id) FROM collection_members m2
			JOIN session_events s2 ON s2.event_id = m2.event_id
			WHERE m2.collection_id = m.collection_id AND s2.owner = s.owner AND s2.session_id = s.session_id)`,
		e.ID, fmtTime(e.OccurredAt), e.Owner, ref.SessionID, e.ID)
	if err != nil {
		return fmt.Errorf("projecting %s: inheriting collections: %w", e.Type, err)
	}
	return nil
}

// sessionOf returns the session an event belongs to, or "" for an event
// that isn't an llm.message.
func sessionOf(ctx context.Context, tx *sql.Tx, eventID string) (owner, sessionID string, err error) {
	err = tx.QueryRowContext(ctx, `SELECT owner, session_id FROM session_events WHERE event_id = ?`, eventID).Scan(&owner, &sessionID)
	if err == sql.ErrNoRows {
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("reading session of %s: %w", eventID, err)
	}
	return owner, sessionID, nil
}

// addSession is upsertMember for the rest of eventID's session: labeling
// one message labels the session.
func addSession(ctx context.Context, tx *sql.Tx, collectionID, eventID string, version int, e Event) error {
	owner, sid, err := sessionOf(ctx, tx, eventID)
	if err != nil || sid == "" {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO collection_members
		(collection_id, event_id, definition_version, reason, label_event_id, labeled_at)
		SELECT ?, event_id, ?, 'same session as ' || ?, ?, ? FROM session_events
		WHERE owner = ? AND session_id = ? AND event_id != ?
		ON CONFLICT (collection_id, event_id) DO NOTHING`,
		collectionID, version, eventID, e.ID, fmtTime(e.OccurredAt), owner, sid, eventID)
	if err != nil {
		return fmt.Errorf("projecting %s: adding session: %w", e.Type, err)
	}
	return nil
}

// removeSession takes the rest of eventID's session out of a collection,
// so later messages stop joining it.
func removeSession(ctx context.Context, tx *sql.Tx, collectionID, eventID string, e Event) error {
	owner, sid, err := sessionOf(ctx, tx, eventID)
	if err != nil || sid == "" {
		return err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM collection_members WHERE collection_id = ?
		AND event_id IN (SELECT event_id FROM session_events WHERE owner = ? AND session_id = ?)`,
		collectionID, owner, sid)
	if err != nil {
		return fmt.Errorf("projecting %s: removing session: %w", e.Type, err)
	}
	return nil
}

// Unredacted returns the ids of an owner's collection members that have no
// redaction.created child yet, oldest event first. Members that joined
// through their session never passed through the agent, so it sweeps
// these.
func (s *Store) Unredacted(ctx context.Context, owner string, limit int) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT e.id FROM events e WHERE e.owner = ?
		AND EXISTS (SELECT 1 FROM collection_members m WHERE m.event_id = e.id)
		AND NOT EXISTS (SELECT 1 FROM events r WHERE r.parent_id = e.id AND r.type = 'redaction.created')
		ORDER BY e.seq LIMIT ?`, owner, limit)
	if err != nil {
		return nil, fmt.Errorf("listing unredacted members: %w", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("listing unredacted members: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
