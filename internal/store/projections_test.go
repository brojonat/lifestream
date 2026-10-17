package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
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

func TestProjectionRejects(t *testing.T) {
	f, ids := weightStory(t)
	w := ids["weight"]
	other := fx{t: t, s: f.s, owner: addUser(t, f.s, "+2").ID}
	otherMsg := other.raw("hi", time.Now())

	f.mustReject(TypeCollectionStarted, CollectionStarted{Name: "x"})
	f.mustReject(TypeCollectionStarted, CollectionStarted{Name: "x", Include: "y", Kind: "bogus"})
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
}
