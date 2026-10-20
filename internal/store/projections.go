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
	TypeLabelRemoved      = "label.removed"
	TypeMetricObserved    = "metric.observed"
	TypeMetricRemoved     = "metric.removed"
	TypeRuleAdded         = "rule.added"
	TypeRuleRemoved       = "rule.removed"
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
		RemoveRule   string `json:"remove_rule,omitempty"` // event id of the amendment that added it
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
	// LabelRemoved is the agent taking an event out of a collection, e.g.
	// after an exclude rule (the owner's equivalent is LabelCorrected).
	LabelRemoved struct {
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
	RuleAdded struct {
		Text string `json:"text"`
	}
	RuleRemoved struct {
		RuleID string `json:"rule_id"`
	}
	MetricObserved struct {
		CollectionID string     `json:"collection_id"`
		Metric       string     `json:"metric,omitempty"` // default: the collection's
		Value        *float64   `json:"value"`
		Unit         string     `json:"unit,omitempty"` // default: the collection's
		ObservedAt   *time.Time `json:"observed_at,omitempty"`
		// Replaces is the metric.observed event whose value this one
		// supersedes (a corrected number or date). Without observed_at, the
		// replacement keeps the old value's time.
		Replaces string `json:"replaces,omitempty"`
	}
	// MetricRemoved withdraws a value: ValueID is its metric.observed event.
	MetricRemoved struct {
		ValueID string `json:"value_id"`
		Reason  string `json:"reason"`
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
		set := 0
		for _, f := range []string{p.Include, p.Exclude, p.RemoveRule} {
			if f != "" {
				set++
			}
		}
		if set != 1 {
			return invalid("%s needs exactly one of include, exclude or remove_rule", e.Type)
		}
		c, err := collectionTx(ctx, tx, e.Owner, p.CollectionID)
		if err != nil {
			return err
		}
		if p.RemoveRule == c.ID {
			return invalid("the rule a collection started with can't be removed; stop the collection instead")
		}
		v, err := bump(ctx, tx, c, e, c.Status)
		if err != nil {
			return err
		}
		if p.RemoveRule != "" {
			res, err := tx.ExecContext(ctx, `DELETE FROM collection_rules WHERE collection_id = ? AND event_id = ?`, c.ID, p.RemoveRule)
			if err != nil {
				return fmt.Errorf("projecting %s: %w", e.Type, err)
			}
			if n, _ := res.RowsAffected(); n == 0 {
				return invalid("collection %s has no rule %q", c.ID, p.RemoveRule)
			}
			return nil
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
		if err := upsertMember(ctx, tx, c.ID, p.EventID, p.DefinitionVersion, p.Reason, e); err != nil {
			return err
		}
		return addSession(ctx, tx, c.ID, p.EventID, p.DefinitionVersion, e)

	case TypeLabelRemoved:
		var p LabelRemoved
		if err := decode(e, &p); err != nil {
			return err
		}
		c, err := collectionTx(ctx, tx, e.Owner, p.CollectionID)
		if err != nil {
			return err
		}
		if p.DefinitionVersion < 1 || p.DefinitionVersion > c.Version {
			return invalid("definition_version %d is not a version of collection %s (current %d)", p.DefinitionVersion, c.ID, c.Version)
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM collection_members WHERE collection_id = ? AND event_id = ?`, c.ID, p.EventID)
		if err != nil {
			return fmt.Errorf("projecting %s: %w", e.Type, err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return invalid("event %s is not in collection %s", p.EventID, c.ID)
		}
		return removeSession(ctx, tx, c.ID, p.EventID, e)

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
			if err := upsertMember(ctx, tx, c.ID, p.EventID, c.Version, "added by owner: "+p.Note, e); err != nil {
				return err
			}
			return addSession(ctx, tx, c.ID, p.EventID, c.Version, e)
		case "remove":
			_, err := tx.ExecContext(ctx, `DELETE FROM collection_members WHERE collection_id = ? AND event_id = ?`, c.ID, p.EventID)
			if err != nil {
				return fmt.Errorf("projecting %s: %w", e.Type, err)
			}
			return removeSession(ctx, tx, c.ID, p.EventID, e)
		default:
			return invalid("%s action must be add or remove, got %q", e.Type, p.Action)
		}

	case TypeRuleAdded:
		var p RuleAdded
		if err := decode(e, &p); err != nil {
			return err
		}
		if strings.TrimSpace(p.Text) == "" {
			return invalid("%s needs text", e.Type)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO rules (id, owner, text, source, created_at) VALUES (?, ?, ?, ?, ?)`,
			e.ID, e.Owner, strings.TrimSpace(p.Text), e.Source, fmtTime(e.OccurredAt)); err != nil {
			return fmt.Errorf("projecting %s: %w", e.Type, err)
		}

	case TypeRuleRemoved:
		var p RuleRemoved
		if err := decode(e, &p); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM rules WHERE id = ? AND owner = ?`, p.RuleID, e.Owner)
		if err != nil {
			return fmt.Errorf("projecting %s: %w", e.Type, err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return invalid("no rule %q for this owner", p.RuleID)
		}

	case TypeLLMMessage:
		return applySession(ctx, tx, e)

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
		var replaced *time.Time
		if p.Replaces != "" {
			if replaced, err = dropValue(ctx, tx, e.Owner, p.Replaces, c.ID); err != nil {
				return err
			}
		}
		observed := e.OccurredAt
		if p.ObservedAt != nil {
			observed = *p.ObservedAt
		} else if replaced != nil {
			observed = *replaced
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

	case TypeMetricRemoved:
		var p MetricRemoved
		if err := decode(e, &p); err != nil {
			return err
		}
		if _, err := dropValue(ctx, tx, e.Owner, p.ValueID, ""); err != nil {
			return err
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
	for _, t := range []string{"collections", "collection_rules", "collection_members", "metric_values", "rules", "session_events", "session_messages"} {
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

// CollectionHistory returns the events that shaped a collection's
// definition (started, amended, stopped, resumed), oldest first.
func (s *Store) CollectionHistory(ctx context.Context, id string) ([]Event, error) {
	return s.queryEvents(ctx, selectEvent+` WHERE id = ?
		OR (type IN (?, ?, ?) AND json_extract(payload, '$.collection_id') = ?) ORDER BY seq`,
		id, TypeCollectionAmended, TypeCollectionStopped, TypeCollectionResumed, id)
}

// MemberEvent is a member with the event it refers to.
type MemberEvent struct {
	Member
	Event Event
}

// MemberEvents returns a collection's members with their events, newest
// event first, at most limit.
func (s *Store) MemberEvents(ctx context.Context, collectionID string, limit int) ([]MemberEvent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT m.definition_version, m.reason, m.label_event_id, m.labeled_at, e.seq, e.id, e.owner,
		e.source, e.type, e.occurred_at, e.received_at, COALESCE(e.parent_id, ''), COALESCE(e.idempotency_key, ''), e.payload,
		e.prev_hash, e.hash FROM collection_members m JOIN events e ON e.id = m.event_id
		WHERE m.collection_id = ? ORDER BY e.seq DESC LIMIT ?`, collectionID, limit)
	if err != nil {
		return nil, fmt.Errorf("listing member events: %w", err)
	}
	defer rows.Close()
	var out []MemberEvent
	for rows.Next() {
		var m MemberEvent
		var labeled, occurred, received, payload string
		e := &m.Event
		if err := rows.Scan(&m.DefinitionVersion, &m.Reason, &m.LabelEventID, &labeled, &e.Seq, &e.ID, &e.Owner,
			&e.Source, &e.Type, &occurred, &received, &e.ParentID, &e.IdempotencyKey, &payload, &e.PrevHash, &e.Hash); err != nil {
			return nil, fmt.Errorf("listing member events: %w", err)
		}
		m.CollectionID, m.EventID, e.Payload = collectionID, e.ID, []byte(payload)
		if m.LabeledAt, err = parseTime(labeled); err != nil {
			return nil, err
		}
		if e.OccurredAt, err = parseTime(occurred); err != nil {
			return nil, err
		}
		if e.ReceivedAt, err = parseTime(received); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// MemberCounts returns how many members each of an owner's collections has.
func (s *Store) MemberCounts(ctx context.Context, owner string) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT m.collection_id, count(*) FROM collection_members m
		JOIN collections c ON c.id = m.collection_id WHERE c.owner = ? GROUP BY m.collection_id`, owner)
	if err != nil {
		return nil, fmt.Errorf("counting members: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, fmt.Errorf("counting members: %w", err)
		}
		out[id] = n
	}
	return out, rows.Err()
}

// OwnerRule is one entry in the owner's rules doc.
type OwnerRule struct {
	ID        string    `json:"id"`
	Text      string    `json:"text"`
	Source    string    `json:"source"` // who added it: agent or dashboard
	CreatedAt time.Time `json:"created_at"`
}

// Rules returns the owner's rules doc, oldest entry first.
func (s *Store) Rules(ctx context.Context, owner string) ([]OwnerRule, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, text, source, created_at FROM rules WHERE owner = ? ORDER BY created_at, id`, owner)
	if err != nil {
		return nil, fmt.Errorf("listing rules: %w", err)
	}
	defer rows.Close()
	out := []OwnerRule{}
	for rows.Next() {
		var r OwnerRule
		var at string
		if err := rows.Scan(&r.ID, &r.Text, &r.Source, &at); err != nil {
			return nil, fmt.Errorf("listing rules: %w", err)
		}
		if r.CreatedAt, err = parseTime(at); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SteeringLog returns an owner's collection.* events, newest first.
func (s *Store) SteeringLog(ctx context.Context, owner string, limit int) ([]Event, error) {
	return s.queryEvents(ctx, selectEvent+` WHERE owner = ? AND type IN (?, ?, ?, ?) ORDER BY seq DESC LIMIT ?`,
		owner, TypeCollectionStarted, TypeCollectionAmended, TypeCollectionStopped, TypeCollectionResumed, limit)
}

// HasRule reports whether the amendment eventID's rule is still part of the
// collection's definition.
func (s *Store) HasRule(ctx context.Context, collectionID, eventID string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM collection_rules WHERE collection_id = ? AND event_id = ?`, collectionID, eventID).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("reading rule: %w", err)
	}
	return n > 0, nil
}

// OpenFlags returns an owner's review.flagged events that have no
// review.answered child yet, oldest first.
func (s *Store) OpenFlags(ctx context.Context, owner string) ([]Event, error) {
	return s.queryEvents(ctx, selectEvent+` WHERE owner = ? AND type = 'review.flagged'
		AND NOT EXISTS (SELECT 1 FROM events a WHERE a.parent_id = events.id AND a.type = 'review.answered')
		ORDER BY seq`, owner)
}

// dropValue deletes the value a metric.observed event recorded, after
// checking the event is one (the owner's, in collectionID when that's set).
// It returns the value's time, or nil when it was already gone: removing or
// replacing a value twice is harmless.
func dropValue(ctx context.Context, tx *sql.Tx, owner, valueID, collectionID string) (*time.Time, error) {
	v, err := scanEvent(tx.QueryRowContext(ctx, selectEvent+` WHERE id = ?`, valueID))
	if errors.Is(err, ErrNotFound) || err == nil && (v.Type != TypeMetricObserved || v.Owner != owner) {
		return nil, invalid("%s is not a value (a metric.observed event)", valueID)
	}
	if err != nil {
		return nil, fmt.Errorf("reading value %s: %w", valueID, err)
	}
	var in, at string
	err = tx.QueryRowContext(ctx, `SELECT collection_id, observed_at FROM metric_values WHERE event_id = ?`, valueID).Scan(&in, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading value %s: %w", valueID, err)
	}
	if collectionID != "" && in != collectionID {
		return nil, invalid("value %s is in collection %s, not %s", valueID, in, collectionID)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM metric_values WHERE event_id = ?`, valueID); err != nil {
		return nil, fmt.Errorf("removing value %s: %w", valueID, err)
	}
	t, err := parseTime(at)
	if err != nil {
		return nil, fmt.Errorf("reading value %s: %w", valueID, err)
	}
	return &t, nil
}
