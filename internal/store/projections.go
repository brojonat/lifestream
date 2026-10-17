package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Event types the projections understand. Everything else is stored and
// dispatched but projects nothing.
const (
	TypeCollectionStarted = "collection.started"
	TypeCollectionAmended = "collection.amended"
	TypeCollectionStopped = "collection.stopped"
	TypeCollectionResumed = "collection.resumed"
	TypeLabelApplied      = "label.applied"
	TypeLabelCorrected    = "label.corrected"
	TypeMetricObserved    = "metric.observed"
)

// Collection kinds: a plain set of events, or a metric whose members also
// carry a number and get a chart.
const (
	KindSet    = "set"
	KindMetric = "metric"
)

// Payloads of the derived events. A collection's id is the id of the
// collection.started event that created it.
type (
	CollectionStarted struct {
		Name         string `json:"name"`
		Kind         string `json:"kind,omitempty"` // default "set"
		Include      string `json:"include"`
		Metric       string `json:"metric,omitempty"` // metric kind only; default Name
		Unit         string `json:"unit,omitempty"`
		LookbackDays *int   `json:"lookback_days,omitempty"`
	}
	CollectionAmended struct {
		CollectionID string `json:"collection_id"`
		Include      string `json:"include,omitempty"`
		Exclude      string `json:"exclude,omitempty"`
	}
	CollectionRef struct {
		CollectionID string `json:"collection_id"`
	}
	LabelApplied struct {
		EventID           string `json:"event_id"`
		CollectionID      string `json:"collection_id"`
		DefinitionVersion int    `json:"definition_version"`
		Reason            string `json:"reason"`
	}
	LabelCorrected struct {
		EventID      string `json:"event_id"`
		CollectionID string `json:"collection_id"`
		Action       string `json:"action"` // "add" or "remove"
		Note         string `json:"note,omitempty"`
	}
	MetricObserved struct {
		CollectionID string     `json:"collection_id"`
		Metric       string     `json:"metric,omitempty"` // default: the collection's
		Value        *float64   `json:"value"`
		Unit         string     `json:"unit,omitempty"` // default: the collection's
		ObservedAt   *time.Time `json:"observed_at,omitempty"`
	}
)

// Collection is a collection's current definition: the fold of its
// collection.* events.
type Collection struct {
	ID           string    `json:"id"`
	Owner        string    `json:"owner"`
	Name         string    `json:"name"`
	Kind         string    `json:"kind"`
	Metric       string    `json:"metric,omitempty"`
	Unit         string    `json:"unit,omitempty"`
	Status       string    `json:"status"` // "open" or "closed"
	Version      int       `json:"version"`
	LookbackDays *int      `json:"lookback_days,omitempty"`
	Rules        []Rule    `json:"rules"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// Rule is one include or exclude rule, in plain English.
type Rule struct {
	Kind    string `json:"kind"` // "include" or "exclude"
	Text    string `json:"text"`
	Version int    `json:"version"` // definition version that added it
	EventID string `json:"event_id"`
}

// Member is one event's membership in a collection.
type Member struct {
	CollectionID      string    `json:"collection_id"`
	EventID           string    `json:"event_id"`
	DefinitionVersion int       `json:"definition_version"`
	Reason            string    `json:"reason"`
	LabelEventID      string    `json:"label_event_id"`
	LabeledAt         time.Time `json:"labeled_at"`
}

// MetricValue is one extracted number.
type MetricValue struct {
	EventID       string    `json:"event_id"` // the metric.observed event
	Owner         string    `json:"owner"`
	CollectionID  string    `json:"collection_id"`
	SourceEventID string    `json:"source_event_id"`
	Metric        string    `json:"metric"`
	Value         float64   `json:"value"`
	Unit          string    `json:"unit,omitempty"`
	ObservedAt    time.Time `json:"observed_at"`
}

// InvalidEventError means an event contradicts the current projections
// (an unknown collection, a label on a closed one, ...). Append rejects
// such events, so the API can answer 400 instead of 500.
type InvalidEventError struct{ Msg string }

func (e *InvalidEventError) Error() string { return "invalid event: " + e.Msg }

func invalid(format string, args ...any) error {
	return &InvalidEventError{fmt.Sprintf(format, args...)}
}

// apply updates the projections for one event, inside the append (or
// rebuild) transaction. Both paths run this same function, which is what
// makes rebuild equal incremental.
func apply(ctx context.Context, tx *sql.Tx, e Event) error {
	switch e.Type {
	case TypeCollectionStarted:
		var p CollectionStarted
		if err := decode(e, &p); err != nil {
			return err
		}
		if strings.TrimSpace(p.Name) == "" || strings.TrimSpace(p.Include) == "" {
			return invalid("%s needs name and include", e.Type)
		}
		switch p.Kind {
		case "":
			p.Kind = KindSet
		case KindSet, KindMetric:
		default:
			return invalid("unknown collection kind %q", p.Kind)
		}
		if p.Kind == KindMetric && p.Metric == "" {
			p.Metric = p.Name
		}
		at := fmtTime(e.OccurredAt)
		if _, err := tx.ExecContext(ctx, `INSERT INTO collections
			(id, owner, name, kind, metric, unit, status, version, lookback_days, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, 'open', 1, ?, ?, ?)`,
			e.ID, e.Owner, p.Name, p.Kind, nullable(p.Metric), nullable(p.Unit), p.LookbackDays, at, at); err != nil {
			return fmt.Errorf("projecting %s: %w", e.Type, err)
		}
		return addRule(ctx, tx, e.ID, 1, "include", p.Include, e.ID)

	case TypeCollectionAmended:
		var p CollectionAmended
		if err := decode(e, &p); err != nil {
			return err
		}
		if (p.Include == "") == (p.Exclude == "") {
			return invalid("%s needs exactly one of include or exclude", e.Type)
		}
		c, err := collectionTx(ctx, tx, e.Owner, p.CollectionID)
		if err != nil {
			return err
		}
		v, err := bump(ctx, tx, c, e, c.Status)
		if err != nil {
			return err
		}
		if p.Include != "" {
			return addRule(ctx, tx, c.ID, v, "include", p.Include, e.ID)
		}
		return addRule(ctx, tx, c.ID, v, "exclude", p.Exclude, e.ID)

	case TypeCollectionStopped, TypeCollectionResumed:
		var p CollectionRef
		if err := decode(e, &p); err != nil {
			return err
		}
		c, err := collectionTx(ctx, tx, e.Owner, p.CollectionID)
		if err != nil {
			return err
		}
		status := "closed"
		if e.Type == TypeCollectionResumed {
			status = "open"
		}
		if c.Status == status {
			return invalid("collection %s is already %s", c.ID, status)
		}
		_, err = bump(ctx, tx, c, e, status)
		return err

	case TypeLabelApplied:
		var p LabelApplied
		if err := decode(e, &p); err != nil {
			return err
		}
		c, err := collectionTx(ctx, tx, e.Owner, p.CollectionID)
		if err != nil {
			return err
		}
		if c.Status != "open" {
			return invalid("collection %s is closed", c.ID)
		}
		if p.DefinitionVersion < 1 || p.DefinitionVersion > c.Version {
			return invalid("definition_version %d is not a version of collection %s (current %d)", p.DefinitionVersion, c.ID, c.Version)
		}
		if err := ownedEvent(ctx, tx, e.Owner, p.EventID); err != nil {
			return err
		}
		return upsertMember(ctx, tx, c.ID, p.EventID, p.DefinitionVersion, p.Reason, e)

	case TypeLabelCorrected:
		var p LabelCorrected
		if err := decode(e, &p); err != nil {
			return err
		}
		c, err := collectionTx(ctx, tx, e.Owner, p.CollectionID)
		if err != nil {
			return err
		}
		if err := ownedEvent(ctx, tx, e.Owner, p.EventID); err != nil {
			return err
		}
		switch p.Action {
		case "add":
			return upsertMember(ctx, tx, c.ID, p.EventID, c.Version, "added by owner: "+p.Note, e)
		case "remove":
			_, err := tx.ExecContext(ctx, `DELETE FROM collection_members WHERE collection_id = ? AND event_id = ?`, c.ID, p.EventID)
			if err != nil {
				return fmt.Errorf("projecting %s: %w", e.Type, err)
			}
			return nil
		default:
			return invalid("%s action must be add or remove, got %q", e.Type, p.Action)
		}

	case TypeMetricObserved:
		var p MetricObserved
		if err := decode(e, &p); err != nil {
			return err
		}
		if p.Value == nil {
			return invalid("%s needs a numeric value", e.Type)
		}
		c, err := collectionTx(ctx, tx, e.Owner, p.CollectionID)
		if err != nil {
			return err
		}
		if c.Kind != KindMetric {
			return invalid("collection %s is not a metric", c.ID)
		}
		if p.Metric == "" {
			p.Metric = c.Metric
		}
		if p.Unit == "" {
			p.Unit = c.Unit
		}
		observed := e.OccurredAt
		if p.ObservedAt != nil {
			observed = *p.ObservedAt
		} else if e.ParentID != "" {
			parent, err := scanEvent(tx.QueryRowContext(ctx, selectEvent+` WHERE id = ?`, e.ParentID))
			if err != nil {
				return fmt.Errorf("reading parent of %s: %w", e.ID, err)
			}
			observed = parent.OccurredAt
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO metric_values
			(event_id, owner, collection_id, source_event_id, metric, value, unit, observed_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			e.ID, e.Owner, c.ID, nullable(e.ParentID), p.Metric, *p.Value, nullable(p.Unit), fmtTime(observed)); err != nil {
			return fmt.Errorf("projecting %s: %w", e.Type, err)
		}
	}
	return nil
}

func decode(e Event, v any) error {
	if err := json.Unmarshal(e.Payload, v); err != nil {
		return invalid("%s payload: %v", e.Type, err)
	}
	return nil
}

func addRule(ctx context.Context, tx *sql.Tx, collectionID string, version int, kind, text, eventID string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO collection_rules (collection_id, version, kind, text, event_id) VALUES (?, ?, ?, ?, ?)`,
		collectionID, version, kind, text, eventID)
	if err != nil {
		return fmt.Errorf("adding rule: %w", err)
	}
	return nil
}

// bump sets the status and increments the definition version.
func bump(ctx context.Context, tx *sql.Tx, c Collection, e Event, status string) (int, error) {
	v := c.Version + 1
	_, err := tx.ExecContext(ctx, `UPDATE collections SET version = ?, status = ?, updated_at = ? WHERE id = ?`,
		v, status, fmtTime(e.OccurredAt), c.ID)
	if err != nil {
		return 0, fmt.Errorf("projecting %s: %w", e.Type, err)
	}
	return v, nil
}

func upsertMember(ctx context.Context, tx *sql.Tx, collectionID, eventID string, version int, reason string, e Event) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO collection_members
		(collection_id, event_id, definition_version, reason, label_event_id, labeled_at) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (collection_id, event_id) DO UPDATE SET
			definition_version = excluded.definition_version, reason = excluded.reason,
			label_event_id = excluded.label_event_id, labeled_at = excluded.labeled_at`,
		collectionID, eventID, version, reason, e.ID, fmtTime(e.OccurredAt))
	if err != nil {
		return fmt.Errorf("projecting %s: %w", e.Type, err)
	}
	return nil
}

func ownedEvent(ctx context.Context, tx *sql.Tx, owner, id string) error {
	var got string
	err := tx.QueryRowContext(ctx, `SELECT owner FROM events WHERE id = ?`, id).Scan(&got)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && got != owner) {
		return invalid("no event %s for this owner", id)
	}
	if err != nil {
		return fmt.Errorf("reading event %s: %w", id, err)
	}
	return nil
}

func collectionTx(ctx context.Context, tx *sql.Tx, owner, id string) (Collection, error) {
	c, err := scanCollection(tx.QueryRowContext(ctx, selectCollection+` WHERE id = ?`, id))
	if errors.Is(err, ErrNotFound) || (err == nil && c.Owner != owner) {
		return Collection{}, invalid("no collection %q for this owner", id)
	}
	return c, err
}

// Rebuild drops every projection and replays all events through apply.
// Writes are blocked while it runs.
func (s *Store) Rebuild(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("rebuilding: %w", err)
	}
	defer tx.Rollback()
	for _, t := range []string{"collections", "collection_rules", "collection_members", "metric_values"} {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+t); err != nil {
			return fmt.Errorf("rebuilding: clearing %s: %w", t, err)
		}
	}
	var after int64
	for {
		batch, err := queryEventsTx(ctx, tx, selectEvent+` WHERE seq > ? ORDER BY seq LIMIT 1000`, after)
		if err != nil {
			return fmt.Errorf("rebuilding: %w", err)
		}
		if len(batch) == 0 {
			break
		}
		for _, e := range batch {
			if err := apply(ctx, tx, e); err != nil {
				return fmt.Errorf("rebuilding at event %s (%s): %w", e.ID, e.Type, err)
			}
			after = e.Seq
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("rebuilding: commit: %w", err)
	}
	s.notify()
	return nil
}

func queryEventsTx(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]Event, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
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
	return out, rows.Err()
}

// ---- reads -------------------------------------------------------------

const selectCollection = `SELECT id, owner, name, kind, COALESCE(metric, ''), COALESCE(unit, ''),
	status, version, lookback_days, created_at, updated_at FROM collections`

func scanCollection(row scanner) (Collection, error) {
	var c Collection
	var lookback sql.NullInt64
	var created, updated string
	err := row.Scan(&c.ID, &c.Owner, &c.Name, &c.Kind, &c.Metric, &c.Unit, &c.Status, &c.Version, &lookback, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return Collection{}, ErrNotFound
	}
	if err != nil {
		return Collection{}, fmt.Errorf("reading collection: %w", err)
	}
	if lookback.Valid {
		n := int(lookback.Int64)
		c.LookbackDays = &n
	}
	if c.CreatedAt, err = parseTime(created); err != nil {
		return Collection{}, err
	}
	c.UpdatedAt, err = parseTime(updated)
	return c, err
}

// Collections returns an owner's collections with their rules, oldest
// first. status "" means all.
func (s *Store) Collections(ctx context.Context, owner, status string) ([]Collection, error) {
	q := selectCollection + ` WHERE owner = ?`
	args := []any{owner}
	if status != "" {
		q += ` AND status = ?`
		args = append(args, status)
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY created_at, id`, args...)
	if err != nil {
		return nil, fmt.Errorf("listing collections: %w", err)
	}
	var out []Collection
	for rows.Next() {
		c, err := scanCollection(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing collections: %w", err)
	}
	for i := range out {
		if out[i].Rules, err = s.rules(ctx, out[i].ID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Collection returns one collection with its rules.
func (s *Store) Collection(ctx context.Context, id string) (Collection, error) {
	c, err := scanCollection(s.db.QueryRowContext(ctx, selectCollection+` WHERE id = ?`, id))
	if err != nil {
		return Collection{}, err
	}
	c.Rules, err = s.rules(ctx, id)
	return c, err
}

func (s *Store) rules(ctx context.Context, collectionID string) ([]Rule, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT kind, text, version, event_id FROM collection_rules
		WHERE collection_id = ? ORDER BY version, event_id`, collectionID)
	if err != nil {
		return nil, fmt.Errorf("reading rules: %w", err)
	}
	defer rows.Close()
	out := []Rule{}
	for rows.Next() {
		var r Rule
		if err := rows.Scan(&r.Kind, &r.Text, &r.Version, &r.EventID); err != nil {
			return nil, fmt.Errorf("reading rules: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Members returns a collection's members, oldest label first.
func (s *Store) Members(ctx context.Context, collectionID string) ([]Member, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT collection_id, event_id, definition_version, reason, label_event_id, labeled_at
		FROM collection_members WHERE collection_id = ? ORDER BY labeled_at, event_id`, collectionID)
	if err != nil {
		return nil, fmt.Errorf("listing members: %w", err)
	}
	defer rows.Close()
	var out []Member
	for rows.Next() {
		var m Member
		var at string
		if err := rows.Scan(&m.CollectionID, &m.EventID, &m.DefinitionVersion, &m.Reason, &m.LabelEventID, &at); err != nil {
			return nil, fmt.Errorf("listing members: %w", err)
		}
		if m.LabeledAt, err = parseTime(at); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// MetricValues returns a collection's values observed at or after since,
// oldest first.
func (s *Store) MetricValues(ctx context.Context, collectionID string, since time.Time) ([]MetricValue, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT event_id, owner, collection_id, COALESCE(source_event_id, ''), metric, value,
		COALESCE(unit, ''), observed_at FROM metric_values WHERE collection_id = ? AND observed_at >= ?
		ORDER BY observed_at, event_id`, collectionID, fmtTime(since))
	if err != nil {
		return nil, fmt.Errorf("listing metric values: %w", err)
	}
	defer rows.Close()
	var out []MetricValue
	for rows.Next() {
		var v MetricValue
		var at string
		if err := rows.Scan(&v.EventID, &v.Owner, &v.CollectionID, &v.SourceEventID, &v.Metric, &v.Value, &v.Unit, &at); err != nil {
			return nil, fmt.Errorf("listing metric values: %w", err)
		}
		if v.ObservedAt, err = parseTime(at); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
