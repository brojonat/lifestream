package store

import (
	"context"
	"fmt"
	"strings"
)

// InboxItem is a raw event and what it became.
type InboxItem struct {
	Event     Event
	Delivered bool          // the agent has it
	Derived   []Event       // events whose parent is this one, oldest first
	Members   []Membership  // collections it is currently in
	Values    []MetricValue // numbers extracted from it
}

// Membership names a collection an event belongs to.
type Membership struct {
	CollectionID string
	Name         string
	Reason       string
}

// Inbox returns an owner's most recent raw (non-agent) events, newest
// first, each with its derived events and current memberships.
func (s *Store) Inbox(ctx context.Context, owner string, limit int) ([]InboxItem, error) {
	evs, err := s.queryEvents(ctx, selectEvent+` WHERE owner = ? AND source != ? ORDER BY seq DESC LIMIT ?`, owner, SourceAgent, limit)
	if err != nil {
		return nil, err
	}
	if len(evs) == 0 {
		return nil, nil
	}
	items := make([]InboxItem, len(evs))
	index := map[string]*InboxItem{}
	ids := make([]any, len(evs))
	for i, e := range evs {
		items[i].Event = e
		index[e.ID] = &items[i]
		ids[i] = e.ID
	}
	in := "(" + strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",") + ")"

	derived, err := s.queryEvents(ctx, selectEvent+` WHERE parent_id IN `+in+` ORDER BY seq`, ids...)
	if err != nil {
		return nil, err
	}
	for _, d := range derived {
		index[d.ParentID].Derived = append(index[d.ParentID].Derived, d)
	}

	rows, err := s.db.QueryContext(ctx, `SELECT m.event_id, c.id, c.name, m.reason FROM collection_members m
		JOIN collections c ON c.id = m.collection_id WHERE m.event_id IN `+in+` ORDER BY c.name`, ids...)
	if err != nil {
		return nil, fmt.Errorf("reading memberships: %w", err)
	}
	for rows.Next() {
		var id string
		var m Membership
		if err := rows.Scan(&id, &m.CollectionID, &m.Name, &m.Reason); err != nil {
			rows.Close()
			return nil, fmt.Errorf("reading memberships: %w", err)
		}
		index[id].Members = append(index[id].Members, m)
	}
	rows.Close()

	vrows, err := s.db.QueryContext(ctx, `SELECT event_id, owner, collection_id, source_event_id, metric, value,
		COALESCE(unit, ''), observed_at FROM metric_values WHERE source_event_id IN `+in+` ORDER BY event_id`, ids...)
	if err != nil {
		return nil, fmt.Errorf("reading metric values: %w", err)
	}
	for vrows.Next() {
		var v MetricValue
		var at string
		if err := vrows.Scan(&v.EventID, &v.Owner, &v.CollectionID, &v.SourceEventID, &v.Metric, &v.Value, &v.Unit, &at); err != nil {
			vrows.Close()
			return nil, fmt.Errorf("reading metric values: %w", err)
		}
		if v.ObservedAt, err = parseTime(at); err != nil {
			vrows.Close()
			return nil, err
		}
		index[v.SourceEventID].Values = append(index[v.SourceEventID].Values, v)
	}
	vrows.Close()

	rows, err = s.db.QueryContext(ctx, `SELECT event_id FROM deliveries WHERE event_id IN `+in, ids...)
	if err != nil {
		return nil, fmt.Errorf("reading deliveries: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("reading deliveries: %w", err)
		}
		index[id].Delivered = true
	}
	return items, rows.Err()
}
