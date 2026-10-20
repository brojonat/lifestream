package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

type fx struct {
	t     *testing.T
	s     *Store
	owner string
}

func (f fx) raw(body string, at time.Time) Event {
	f.t.Helper()
	p, _ := json.Marshal(map[string]string{"Body": body})
	e, _, err := f.s.Append(context.Background(), Event{Owner: f.owner, Source: "twilio.sms", Type: "message.received", OccurredAt: at, Payload: p})
	if err != nil {
		f.t.Fatal(err)
	}
	return e
}

// derive appends an agent event and returns the error, for checking
// rejections.
func (f fx) derive(typ, parent string, payload any) (Event, error) {
	p, _ := json.Marshal(payload)
	e, _, err := f.s.Append(context.Background(), Event{Owner: f.owner, Source: SourceAgent, Type: typ, ParentID: parent, Payload: p})
	return e, err
}

func (f fx) must(typ, parent string, payload any) Event {
	f.t.Helper()
	e, err := f.derive(typ, parent, payload)
	if err != nil {
		f.t.Fatalf("%s: %v", typ, err)
	}
	return e
}

func (f fx) mustReject(typ string, payload any) {
	f.t.Helper()
	_, err := f.derive(typ, "", payload)
	var inv *InvalidEventError
	if !errors.As(err, &inv) {
		f.t.Fatalf("%s %v: want InvalidEventError, got %v", typ, payload, err)
	}
}

func float(v float64) *float64 { return &v }

// weightStory is the phase 1 story: start a weight metric, text values,
// steer the collection, correct a label.
func weightStory(t *testing.T) (fx, map[string]string) {
	s, _ := open(t)
	f := fx{t: t, s: s, owner: addUser(t, s, "+1").ID}
	ids := map[string]string{}
	day := time.Date(2026, 10, 1, 7, 0, 0, 0, time.UTC)

	cmd := f.raw("start tracking my weight", day)
	weight := f.must(TypeCollectionStarted, cmd.ID, CollectionStarted{Name: "weight", Kind: KindMetric, Unit: "lb", Include: "texts that report my body weight"})
	ids["weight"] = weight.ID
	for i, v := range []float64{175, 174.5, 174} {
		m := f.raw(fmt.Sprintf("weight %v", v), day.AddDate(0, 0, i+1))
		f.must(TypeLabelApplied, m.ID, LabelApplied{EventID: m.ID, CollectionID: weight.ID, DefinitionVersion: 1, Reason: "reports weight"})
		f.must(TypeMetricObserved, m.ID, MetricObserved{CollectionID: weight.ID, Value: float(v)})
		ids[fmt.Sprintf("m%d", i)] = m.ID
	}
	f.must(TypeCollectionAmended, "", CollectionAmended{CollectionID: weight.ID, Include: "w <number> also means weight"})
	f.must(TypeCollectionAmended, "", CollectionAmended{CollectionID: weight.ID, Exclude: "my dog's weight"})
	f.must(TypeLabelCorrected, "", LabelCorrected{EventID: ids["m1"], CollectionID: weight.ID, Action: "remove", Note: "typo day"})

	notes := f.must(TypeCollectionStarted, "", CollectionStarted{Name: "pg migration", Include: "anything about the postgres migration"})
	ids["notes"] = notes.ID
	f.must(TypeCollectionStopped, "", CollectionRef{CollectionID: notes.ID})
	f.must(TypeCollectionResumed, "", CollectionRef{CollectionID: notes.ID})
	f.must(TypeLabelCorrected, "", LabelCorrected{EventID: cmd.ID, CollectionID: notes.ID, Action: "add"})

	f.must(TypeRuleAdded, cmd.ID, RuleAdded{Text: "w means weight"})
	gone := f.must(TypeRuleAdded, "", RuleAdded{Text: "temporary"})
	f.must(TypeRuleRemoved, "", RuleRemoved{RuleID: gone.ID})
	f.must(TypeRuleAdded, "", RuleAdded{Text: "never sell anything tagged client"})
	return f, ids
}

func TestCollectionLifecycle(t *testing.T) {
	ctx := context.Background()
	f, ids := weightStory(t)

	w, err := f.s.Collection(ctx, ids["weight"])
	if err != nil {
		t.Fatal(err)
	}
	if w.Kind != KindMetric || w.Metric != "weight" || w.Unit != "lb" || w.Status != "open" || w.Version != 3 {
		t.Fatalf("weight definition: %+v", w)
	}
	if len(w.Rules) != 3 || w.Rules[1].Kind != "include" || w.Rules[1].Version != 2 || w.Rules[2].Kind != "exclude" {
		t.Fatalf("weight rules: %+v", w.Rules)
	}
	members, _ := f.s.Members(ctx, w.ID)
	if len(members) != 2 || members[0].EventID != ids["m0"] || members[1].EventID != ids["m2"] {
		t.Fatalf("members after removing m1: %+v", members)
	}
	vals, _ := f.s.MetricValues(ctx, w.ID, time.Time{})
	if len(vals) != 3 || vals[0].Value != 175 || vals[0].Unit != "lb" || vals[0].ObservedAt.Day() != 2 {
		t.Fatalf("values: %+v", vals)
	}

	n, _ := f.s.Collection(ctx, ids["notes"])
	if n.Kind != KindSet || n.Status != "open" || n.Version != 3 {
		t.Fatalf("notes definition: %+v", n)
	}
	if m, _ := f.s.Members(ctx, n.ID); len(m) != 1 || m[0].DefinitionVersion != 3 {
		t.Fatalf("owner-added member: %+v", m)
	}

	open, _ := f.s.Collections(ctx, f.owner, "open")
	if len(open) != 2 {
		t.Fatalf("want 2 open collections, got %d", len(open))
	}
}

func TestMetricValuesDatedReplacedRemoved(t *testing.T) {
	ctx := context.Background()
	f, ids := weightStory(t)
	w := ids["weight"]
	sent := time.Date(2026, 10, 20, 16, 31, 0, 0, time.UTC)
	day := func(d int) *time.Time { t := time.Date(2026, 10, d, 19, 0, 0, 0, time.UTC); return &t }
	recent := func() []MetricValue {
		t.Helper()
		vals, err := f.s.MetricValues(ctx, w, sent.AddDate(0, 0, -5))
		if err != nil {
			t.Fatal(err)
		}
		return vals
	}

	// One text, two readings.
	m := f.raw("177.4 yesterday, 176 the day before", sent)
	y := f.must(TypeMetricObserved, m.ID, MetricObserved{CollectionID: w, Value: float(177.4), ObservedAt: day(19)})
	b := f.must(TypeMetricObserved, m.ID, MetricObserved{CollectionID: w, Value: float(176), ObservedAt: day(18)})
	if v := recent(); len(v) != 2 || v[0].Value != 176 || v[1].Value != 177.4 {
		t.Fatalf("two readings from one text: %+v", v)
	}

	// A later text corrects the number: the date carries over.
	fix := f.raw("the 176 was 175", sent.Add(time.Hour))
	f.must(TypeMetricObserved, fix.ID, MetricObserved{CollectionID: w, Value: float(175), Replaces: b.ID})
	if v := recent(); len(v) != 2 || v[0].Value != 175 || !v[0].ObservedAt.Equal(*day(18)) || v[0].SourceEventID != fix.ID {
		t.Fatalf("after replacing: %+v", v)
	}

	// Removing a value, then removing it again, is fine; others stay.
	f.must(TypeMetricRemoved, fix.ID, MetricRemoved{ValueID: y.ID, Reason: "owner says it was a typo"})
	f.must(TypeMetricRemoved, fix.ID, MetricRemoved{ValueID: y.ID})
	if v := recent(); len(v) != 1 || v[0].Value != 175 {
		t.Fatalf("after removing: %+v", v)
	}
	if all, _ := f.s.MetricValues(ctx, w, time.Time{}); len(all) != 4 {
		t.Fatalf("other events' values untouched: want 4, got %d", len(all))
	}

	f.mustReject(TypeMetricRemoved, MetricRemoved{ValueID: m.ID})                                        // not a value
	f.mustReject(TypeMetricObserved, MetricObserved{CollectionID: w, Value: float(1), Replaces: "nope"}) // unknown
}

func TestProjectionRejects(t *testing.T) {
	f, ids := weightStory(t)
	w := ids["weight"]
	other := fx{t: t, s: f.s, owner: addUser(t, f.s, "+2").ID}
	otherMsg := other.raw("hi", time.Now())

	f.mustReject(TypeCollectionStarted, CollectionStarted{Name: "x"})
	f.mustReject(TypeCollectionStarted, CollectionStarted{Name: "x", Include: "y", Kind: "bogus"})
	f.mustReject(TypeCollectionStarted, CollectionStarted{Name: "sleep", Include: "hours slept", Unit: "hours"}) // a set with a unit
	f.mustReject(TypeCollectionAmended, CollectionAmended{CollectionID: w})
	f.mustReject(TypeCollectionAmended, CollectionAmended{CollectionID: w, Include: "a", Exclude: "b"})
	f.mustReject(TypeCollectionAmended, CollectionAmended{CollectionID: "nope", Include: "a"})
	f.mustReject(TypeCollectionResumed, CollectionRef{CollectionID: w}) // already open
	f.mustReject(TypeLabelApplied, LabelApplied{EventID: ids["m0"], CollectionID: w, DefinitionVersion: 9})
	f.mustReject(TypeLabelApplied, LabelApplied{EventID: otherMsg.ID, CollectionID: w, DefinitionVersion: 1})
	f.mustReject(TypeLabelCorrected, LabelCorrected{EventID: ids["m0"], CollectionID: w, Action: "maybe"})
	f.mustReject(TypeMetricObserved, MetricObserved{CollectionID: w})
	f.mustReject(TypeMetricObserved, MetricObserved{CollectionID: ids["notes"], Value: float(1)})

	// Another owner can't touch this owner's collections.
	other.mustReject(TypeCollectionStopped, CollectionRef{CollectionID: w})

	// A closed collection takes no new labels.
	f.must(TypeCollectionStopped, "", CollectionRef{CollectionID: w})
	f.mustReject(TypeLabelApplied, LabelApplied{EventID: ids["m1"], CollectionID: w, DefinitionVersion: 4})

	// Nothing rejected was stored.
	if err := f.s.Verify(context.Background(), f.owner); err != nil {
		t.Fatal(err)
	}
}

func dump(t *testing.T, s *Store) map[string][]map[string]any {
	t.Helper()
	out := map[string][]map[string]any{}
	for table, order := range map[string]string{
		"collections":        "id",
		"collection_rules":   "collection_id, event_id",
		"collection_members": "collection_id, event_id",
		"metric_values":      "event_id",
		"records":            "event_id",
		"rules":              "id",
	} {
		rows, err := s.db.Query("SELECT * FROM " + table + " ORDER BY " + order)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			rows.Scan(ptrs...)
			row := map[string]any{}
			for i, c := range cols {
				row[c] = vals[i]
			}
			out[table] = append(out[table], row)
		}
		rows.Close()
	}
	return out
}

func TestRebuildEqualsIncremental(t *testing.T) {
	f, _ := weightStory(t)
	before := dump(t, f.s)
	if len(before["collections"]) != 2 || len(before["metric_values"]) != 3 {
		t.Fatalf("story produced too little to compare: %v", before)
	}
	if _, err := f.s.db.Exec("UPDATE collections SET name = 'scribbled'"); err != nil {
		t.Fatal(err) // projections are mutable; rebuild must restore them
	}
	if err := f.s.Rebuild(context.Background()); err != nil {
		t.Fatal(err)
	}
	if after := dump(t, f.s); !reflect.DeepEqual(before, after) {
		t.Fatalf("rebuild differs from incremental:\nbefore %v\nafter  %v", before, after)
	}
}

func TestParentMustBelongToOwner(t *testing.T) {
	s, _ := open(t)
	a := fx{t: t, s: s, owner: addUser(t, s, "+1").ID}
	b := fx{t: t, s: s, owner: addUser(t, s, "+2").ID}
	msg := a.raw("hi", time.Now())
	var inv *InvalidEventError
	if _, err := b.derive("review.flagged", msg.ID, map[string]string{}); !errors.As(err, &inv) {
		t.Fatalf("cross-owner parent: want InvalidEventError, got %v", err)
	}
	if _, err := a.derive("review.flagged", "missing", map[string]string{}); !errors.As(err, &inv) {
		t.Fatalf("unknown parent: want InvalidEventError, got %v", err)
	}
	if _, err := a.derive("review.flagged", msg.ID, map[string]string{}); err != nil {
		t.Fatal(err)
	}
}

func TestCollectionHistoryAndMembers(t *testing.T) {
	ctx := context.Background()
	f, ids := weightStory(t)
	hist, err := f.s.CollectionHistory(ctx, ids["weight"])
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	for _, e := range hist {
		types = append(types, e.Type)
	}
	want := []string{TypeCollectionStarted, TypeCollectionAmended, TypeCollectionAmended}
	if !reflect.DeepEqual(types, want) {
		t.Fatalf("history %v, want %v", types, want)
	}
	ms, err := f.s.MemberEvents(ctx, ids["weight"], 10)
	if err != nil || len(ms) != 2 || ms[0].Event.ID != ids["m2"] || ms[1].Event.ID != ids["m0"] {
		t.Fatalf("member events: %v %+v", err, ms)
	}
	counts, _ := f.s.MemberCounts(ctx, f.owner)
	if counts[ids["weight"]] != 2 || counts[ids["notes"]] != 1 {
		t.Fatalf("counts: %v", counts)
	}
}

func TestRulesDoc(t *testing.T) {
	ctx := context.Background()
	f, _ := weightStory(t)
	rules, err := f.s.Rules(ctx, f.owner)
	if err != nil || len(rules) != 2 || rules[0].Text != "w means weight" || rules[0].Source != SourceAgent {
		t.Fatalf("rules: %v %+v", err, rules)
	}
	f.mustReject(TypeRuleAdded, RuleAdded{Text: "  "})
	f.mustReject(TypeRuleRemoved, RuleRemoved{RuleID: "nope"})
	other := fx{t: t, s: f.s, owner: addUser(t, f.s, "+9").ID}
	other.mustReject(TypeRuleRemoved, RuleRemoved{RuleID: rules[0].ID})
}

func TestRemoveRuleAndFlags(t *testing.T) {
	ctx := context.Background()
	f, ids := weightStory(t)
	w := ids["weight"]
	c, _ := f.s.Collection(ctx, w)
	include := c.Rules[1] // "w <number> also means weight", added at v2
	f.mustReject(TypeCollectionAmended, CollectionAmended{CollectionID: w, RemoveRule: w})
	f.mustReject(TypeCollectionAmended, CollectionAmended{CollectionID: w, RemoveRule: "nope"})
	f.mustReject(TypeCollectionAmended, CollectionAmended{CollectionID: w, Include: "x", RemoveRule: include.EventID})
	f.must(TypeCollectionAmended, "", CollectionAmended{CollectionID: w, RemoveRule: include.EventID})
	c, _ = f.s.Collection(ctx, w)
	if c.Version != 4 || len(c.Rules) != 2 {
		t.Fatalf("after removing a rule: v%d %+v", c.Version, c.Rules)
	}
	if ok, _ := f.s.HasRule(ctx, w, include.EventID); ok {
		t.Fatal("rule should be gone")
	}
	log, _ := f.s.SteeringLog(ctx, f.owner, 50)
	if len(log) != 7 || log[0].Type != TypeCollectionAmended {
		t.Fatalf("steering log: %d events, newest %s", len(log), log[0].Type)
	}

	msg := f.raw("w 17", time.Now())
	flag := f.must("review.flagged", msg.ID, map[string]string{"question": "is 17 a weight?"})
	if open, _ := f.s.OpenFlags(ctx, f.owner); len(open) != 1 || open[0].ID != flag.ID {
		t.Fatalf("open flags: %v", open)
	}
	f.s.Append(ctx, Event{Owner: f.owner, Source: "dashboard", Type: "review.answered", ParentID: flag.ID, Payload: json.RawMessage(`{"answer":"no, typo"}`)})
	if open, _ := f.s.OpenFlags(ctx, f.owner); len(open) != 0 {
		t.Fatalf("answered flag still open: %v", open)
	}

	// The agent takes back a question later events settled.
	settled := f.must("review.flagged", msg.ID, map[string]string{"question": "is 177 a weight?"})
	f.must("review.withdrawn", settled.ID, map[string]string{"reason": "the owner started tracking weight"})
	if open, _ := f.s.OpenFlags(ctx, f.owner); len(open) != 0 {
		t.Fatalf("withdrawn flag still open: %v", open)
	}
}

func TestLabelRemoved(t *testing.T) {
	ctx := context.Background()
	f, ids := weightStory(t)
	w := ids["weight"]
	f.mustReject(TypeLabelRemoved, LabelRemoved{EventID: ids["m1"], CollectionID: w, DefinitionVersion: 3}) // not a member
	f.mustReject(TypeLabelRemoved, LabelRemoved{EventID: ids["m0"], CollectionID: w, DefinitionVersion: 99})
	f.must(TypeLabelRemoved, ids["m0"], LabelRemoved{EventID: ids["m0"], CollectionID: w, DefinitionVersion: 3, Reason: "the dog's weight"})
	if ms, _ := f.s.Members(ctx, w); len(ms) != 1 || ms[0].EventID != ids["m2"] {
		t.Fatalf("members: %+v", ms)
	}
}

func TestRecords(t *testing.T) {
	ctx := context.Background()
	f, _ := weightStory(t)
	sent := time.Date(2026, 10, 20, 16, 0, 0, 0, time.UTC)
	m := f.raw("ran 5k in 31 minutes yesterday, and lifted", sent)
	yesterday := sent.AddDate(0, 0, -1)
	run := f.must(TypeRecordSaved, m.ID, RecordSaved{Kind: "workout", Data: json.RawMessage(`{"type":"run","km":5,"minutes":31}`), ObservedAt: &yesterday})
	lift := f.must(TypeRecordSaved, m.ID, RecordSaved{Kind: "workout", Data: json.RawMessage(`{"type":"lift"}`)})

	rows := func() [][]any {
		t.Helper()
		res, err := f.s.Query(ctx, `SELECT json_extract(data, '$.type'), json_extract(data, '$.minutes'), observed_at FROM records
			WHERE owner = ? AND kind = 'workout' ORDER BY observed_at`, []any{f.owner}, 100)
		if err != nil {
			t.Fatal(err)
		}
		return res.Rows
	}
	if r := rows(); len(r) != 2 || r[0][0] != "run" || r[0][1] != int64(31) || r[1][2] != fmtTime(sent) {
		t.Fatalf("two records from one text: %v", r)
	}
	fix := f.raw("it was 29 minutes", sent.Add(time.Hour))
	f.must(TypeRecordSaved, fix.ID, RecordSaved{Kind: "workout", Data: json.RawMessage(`{"type":"run","km":5,"minutes":29}`), Replaces: run.ID})
	f.must(TypeRecordRemoved, fix.ID, RecordRemoved{RecordID: lift.ID, Reason: "didn't lift after all"})
	if r := rows(); len(r) != 1 || r[0][1] != int64(29) || r[0][2] != fmtTime(yesterday) {
		t.Fatalf("after replace and remove: %v", r)
	}

	f.mustReject(TypeRecordSaved, RecordSaved{Kind: "workout", Data: json.RawMessage(`[1]`)})
	f.mustReject(TypeRecordSaved, RecordSaved{Data: json.RawMessage(`{}`)})
	f.mustReject(TypeRecordRemoved, RecordRemoved{RecordID: m.ID})

	// Rebuilding from the log lands on the same rows.
	before := rows()
	if err := f.s.Rebuild(ctx); err != nil {
		t.Fatal(err)
	}
	if after := rows(); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Fatalf("rebuild: %v, incremental: %v", after, before)
	}
}

func TestQueryIsReadOnly(t *testing.T) {
	ctx := context.Background()
	f, _ := weightStory(t)
	var qe *QueryError
	for _, sql := range []string{
		`DELETE FROM events`,
		`UPDATE metric_values SET value = 0`,
		`INSERT INTO rules (id, owner, text, source, created_at) VALUES ('x', 'x', 'x', 'x', 'x')`,
		`CREATE TABLE sneaky (x)`,
		`SELECT nope FROM nowhere`,
	} {
		if _, err := f.s.Query(ctx, sql, nil, 10); !errors.As(err, &qe) {
			t.Errorf("%s: want QueryError, got %v", sql, err)
		}
	}
	res, err := f.s.Query(ctx, `SELECT id FROM events ORDER BY seq`, nil, 3)
	if err != nil || len(res.Rows) != 3 || !res.Truncated || res.Columns[0] != "id" {
		t.Fatalf("limit: %+v %v", res, err)
	}
	res, _ = f.s.Query(ctx, `SELECT printf('%.*c', 10000, 'x')`, nil, 1)
	if s := res.Rows[0][0].(string); !strings.HasSuffix(s, "…[1808 more bytes]") {
		t.Fatalf("long cell not cut: %d bytes", len(s))
	}

	schema, err := f.s.Schema(ctx)
	if err != nil || !strings.Contains(schema, "CREATE TABLE records") || strings.Contains(schema, "token_hash") {
		t.Fatalf("schema: %v\n%s", err, schema)
	}
	docs := map[string]string{}
	for _, d := range EventDocs() {
		docs[d.Type] = d.Payload
	}
	if got := docs[TypeMetricObserved]; got != "{collection_id: string, metric?: string, value: number, unit?: string, observed_at?: RFC 3339 time, replaces?: string}" {
		t.Fatalf("metric.observed shape: %s", got)
	}
}
