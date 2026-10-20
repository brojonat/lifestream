package store

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"
)

// QueryResult is what a read-only query returned. Truncated says rows were
// cut at the limit; long text cells are cut too and say how much is missing.
type QueryResult struct {
	Columns   []string `json:"columns"`
	Rows      [][]any  `json:"rows"`
	Truncated bool     `json:"truncated"`
}

// QueryError is a query SQLite refused (bad SQL, a write, a timeout): the
// caller's mistake, not the server's.
type QueryError struct{ Msg string }

func (e *QueryError) Error() string { return "query: " + e.Msg }

// MaxCell is how much of one text cell a query returns; substr() reads the rest.
const MaxCell = 8192

// Query runs one SQL statement on a read-only connection, so it can read
// anything and change nothing. Writes go through events.
func (s *Store) Query(ctx context.Context, query string, args []any, limit int) (QueryResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rows, err := s.ro.QueryContext(ctx, query, args...)
	if err != nil {
		return QueryResult{}, &QueryError{err.Error()}
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return QueryResult{}, fmt.Errorf("query: %w", err)
	}
	out := QueryResult{Columns: cols, Rows: [][]any{}}
	for rows.Next() {
		if len(out.Rows) == limit {
			out.Truncated = true
			break
		}
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return QueryResult{}, fmt.Errorf("query: %w", err)
		}
		for i, v := range vals {
			if b, ok := v.([]byte); ok {
				v = string(b)
			}
			if t, ok := v.(string); ok && len(t) > MaxCell {
				v = fmt.Sprintf("%s…[%d more bytes]", t[:MaxCell], len(t)-MaxCell)
			}
			vals[i] = v
		}
		out.Rows = append(out.Rows, vals)
	}
	if err := rows.Err(); err != nil {
		return QueryResult{}, &QueryError{err.Error()}
	}
	return out, nil
}

// Schema is the database as CREATE statements, for the agent's prompt.
// users is left out: it holds token hashes and nothing the agent needs.
func (s *Store) Schema(ctx context.Context) (string, error) {
	rows, err := s.ro.QueryContext(ctx, `SELECT sql FROM sqlite_master
		WHERE type IN ('table', 'view') AND sql IS NOT NULL
		AND name NOT IN ('users', 'sqlite_sequence') ORDER BY rowid`)
	if err != nil {
		return "", fmt.Errorf("reading schema: %w", err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var stmt string
		if err := rows.Scan(&stmt); err != nil {
			return "", fmt.Errorf("reading schema: %w", err)
		}
		b.WriteString(stmt + ";\n")
	}
	return b.String(), rows.Err()
}

// EventDoc describes one event type the agent may append: its payload's
// shape (from the Go struct, so it can't drift) and what it does.
type EventDoc struct {
	Type    string `json:"type"`
	Payload string `json:"payload"`
	Does    string `json:"does"`
}

// agentEvents are the event types the agent may append, in prompt order.
var agentEvents = []struct {
	typ     string
	payload any
	does    string
}{
	{TypeCollectionStarted, CollectionStarted{}, "starts a collection (kind set or metric) and looks back lookback_days (default 7, 0 for none) in the background"},
	{TypeCollectionAmended, CollectionAmended{}, "exactly one of: add an include rule (looks back), add an exclude rule (re-checks members), or remove_rule (collection_rules.event_id of a rule an amendment added)"},
	{TypeCollectionStopped, CollectionRef{}, "stops a collection: no new labels"},
	{TypeCollectionResumed, CollectionRef{}, "reopens a stopped collection"},
	{TypeLabelApplied, LabelApplied{}, "puts event_id into a collection, at its current definition_version"},
	{TypeLabelRemoved, LabelRemoved{}, "takes event_id out of a collection"},
	{TypeMetricObserved, MetricObserved{}, "records one number in a metric collection; replaces supersedes a value (metric_values.event_id)"},
	{TypeMetricRemoved, MetricRemoved{}, "withdraws a value (metric_values.event_id)"},
	{TypeRecordSaved, RecordSaved{}, "keeps data of any shape (data is a JSON object) under a kind you choose; replaces supersedes a record (records.event_id)"},
	{TypeRecordRemoved, RecordRemoved{}, "withdraws a record (records.event_id)"},
	{TypeRuleAdded, RuleAdded{}, "writes down a standing instruction from the owner"},
	{"review.flagged", struct {
		Question string `json:"question"`
	}{}, "asks the owner a question on the dashboard; the answer comes back as review.answered"},
}

// AgentEventTypes is the set the internal API accepts from the agent.
func AgentEventTypes() map[string]bool {
	m := map[string]bool{}
	for _, d := range agentEvents {
		m[d.typ] = true
	}
	return m
}

// EventDocs describes every event type the agent may append.
func EventDocs() []EventDoc {
	out := make([]EventDoc, len(agentEvents))
	for i, d := range agentEvents {
		out[i] = EventDoc{Type: d.typ, Payload: shape(reflect.TypeOf(d.payload)), Does: d.does}
	}
	return out
}

// shape renders a payload struct as {field: type, optional?: type}.
func shape(t reflect.Type) string {
	var fields []string
	for i := range t.NumField() {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		name, opts, _ := strings.Cut(tag, ",")
		if name == "" || name == "-" {
			continue
		}
		if strings.Contains(opts, "omitempty") {
			name += "?"
		}
		fields = append(fields, name+": "+typeName(f.Type))
	}
	return "{" + strings.Join(fields, ", ") + "}"
}

func typeName(t reflect.Type) string {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch {
	case t == reflect.TypeFor[time.Time]():
		return "RFC 3339 time"
	case t == reflect.TypeFor[json.RawMessage]():
		return "JSON object"
	}
	switch t.Kind() {
	case reflect.String:
		return "string"
	case reflect.Int, reflect.Int64:
		return "integer"
	case reflect.Float64:
		return "number"
	case reflect.Bool:
		return "boolean"
	}
	return t.String()
}
