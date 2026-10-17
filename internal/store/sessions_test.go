package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"
)

// msg appends one llm.message of session sid.
func (f fx) msg(sid string, seq int64) (Event, bool) {
	f.t.Helper()
	p, _ := json.Marshal(map[string]any{"session_id": sid, "sequence": seq, "harness": "pi",
		"message": map[string]any{"role": "user", "content": "hi"}})
	e, created, err := f.s.Append(context.Background(), Event{Owner: f.owner, Source: "pi", Type: TypeLLMMessage, Payload: p})
	if err != nil {
		f.t.Fatal(err)
	}
	return e, created
}

func memberIDs(t *testing.T, s *Store, collectionID string) []string {
	t.Helper()
	ms, err := s.Members(context.Background(), collectionID)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, m := range ms {
		out = append(out, m.EventID)
	}
	return out
}

func TestSessionIdempotencyAndValidation(t *testing.T) {
	s, _ := open(t)
	f := fx{t: t, s: s, owner: addUser(t, s, "+1").ID}
	a, created := f.msg("s1", 0)
	if !created || a.IdempotencyKey != "llm.message:s1:0" {
		t.Fatalf("first send: created %v key %q", created, a.IdempotencyKey)
	}
	if again, created := f.msg("s1", 0); created || again.ID != a.ID {
		t.Fatalf("resend made a new event: %v %s", created, again.ID)
	}
	for _, payload := range []string{`{"sequence":1}`, `{"session_id":"s1"}`, `{"session_id":"s1","sequence":-1}`, `{"session_id":" ","sequence":2}`} {
		_, _, err := s.Append(context.Background(), Event{Owner: f.owner, Source: "pi", Type: TypeLLMMessage, Payload: json.RawMessage(payload)})
		var inv *InvalidEventError
		if !errors.As(err, &inv) {
			t.Errorf("%s: want InvalidEventError, got %v", payload, err)
		}
	}
	_, _, err := s.Append(context.Background(), Event{Owner: f.owner, Source: "pi", Type: TypeLLMMessage, IdempotencyKey: "mine",
		Payload: json.RawMessage(`{"session_id":"s1","sequence":3}`)})
	var inv *InvalidEventError
	if !errors.As(err, &inv) {
		t.Errorf("a conflicting idempotency key: want InvalidEventError, got %v", err)
	}
}

func TestOnlyASessionsHeadIsDispatched(t *testing.T) {
	s, _ := open(t)
	f := fx{t: t, s: s, owner: addUser(t, s, "+1").ID}
	var want []string
	for seq := range int64(SessionHead + 2) {
		e, _ := f.msg("s1", seq)
		if seq < SessionHead {
			want = append(want, e.ID)
		}
	}
	other, _ := f.msg("s2", 7) // a session seen mid-stream starts its own head
	want = append(want, other.ID)
	got, err := s.Undelivered(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ids(got), want) {
		t.Fatalf("undelivered %v, want %v", ids(got), want)
	}
}

func TestLabelingOneMessageLabelsTheSession(t *testing.T) {
	s, _ := open(t)
	f := fx{t: t, s: s, owner: addUser(t, s, "+1").ID}
	c := f.must(TypeCollectionStarted, "", CollectionStarted{Name: "pg migration", Include: "postgres migration work"})
	m0, _ := f.msg("s1", 0)
	m1, _ := f.msg("s1", 1)
	unrelated, _ := f.msg("s2", 0)

	// The label lands after m1: m1 joins with it.
	f.must(TypeLabelApplied, m0.ID, LabelApplied{EventID: m0.ID, CollectionID: c.ID, DefinitionVersion: 1, Reason: "migration"})
	// Later messages join on arrival, with no label of their own.
	m2, _ := f.msg("s1", 2)
	if got, want := memberIDs(t, s, c.ID), []string{m0.ID, m1.ID, m2.ID}; !reflect.DeepEqual(slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(want))) {
		t.Fatalf("members %v, want %v (not %s)", got, want, unrelated.ID)
	}
	ms, _ := s.Members(context.Background(), c.ID)
	for _, m := range ms {
		if m.DefinitionVersion != 1 || m.LabelEventID == "" {
			t.Errorf("member %s: version %d label %q", m.EventID, m.DefinitionVersion, m.LabelEventID)
		}
	}

	// A closed collection takes no new messages.
	f.must(TypeCollectionStopped, "", CollectionRef{CollectionID: c.ID})
	f.msg("s1", 3)
	if n := len(memberIDs(t, s, c.ID)); n != 3 {
		t.Fatalf("closed collection grew to %d members", n)
	}
	f.must(TypeCollectionResumed, "", CollectionRef{CollectionID: c.ID})

	// Removing one message removes the session, and it stops inheriting.
	f.must(TypeLabelRemoved, "", LabelRemoved{EventID: m1.ID, CollectionID: c.ID, DefinitionVersion: 3, Reason: "excluded"})
	f.msg("s1", 4)
	if got := memberIDs(t, s, c.ID); len(got) != 0 {
		t.Fatalf("after removing the session: members %v", got)
	}

	// The owner adding one message back adds the session back.
	f.must(TypeLabelCorrected, "", LabelCorrected{EventID: m2.ID, CollectionID: c.ID, Action: "add"})
	if n := len(memberIDs(t, s, c.ID)); n != 5 {
		t.Fatalf("after the owner's add: %d members, want the whole session (5)", n)
	}

	unredacted, err := s.Unredacted(context.Background(), f.owner, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(unredacted) != 5 {
		t.Fatalf("unredacted %v, want the 5 session members", unredacted)
	}
	f.must("redaction.created", m0.ID, map[string]any{"payload": map[string]any{}})
	if unredacted, _ = s.Unredacted(context.Background(), f.owner, 100); len(unredacted) != 4 || unredacted[0] == m0.ID {
		t.Fatalf("after redacting m0: unredacted %v", unredacted)
	}
}

func TestSessionRebuildEqualsIncremental(t *testing.T) {
	s, _ := open(t)
	f := fx{t: t, s: s, owner: addUser(t, s, "+1").ID}
	c := f.must(TypeCollectionStarted, "", CollectionStarted{Name: "x", Include: "x"})
	m0, _ := f.msg("s1", 0)
	f.msg("s1", 1)
	f.must(TypeLabelApplied, m0.ID, LabelApplied{EventID: m0.ID, CollectionID: c.ID, DefinitionVersion: 1, Reason: "x"})
	f.msg("s1", 2)
	f.raw("unrelated", time.Now())
	before := dump(t, s)
	if len(before["collection_members"]) != 3 || len(before["session_events"]) != 3 {
		t.Fatalf("story produced too little to compare: %v", before)
	}
	if err := s.Rebuild(context.Background()); err != nil {
		t.Fatal(err)
	}
	if after := dump(t, s); !reflect.DeepEqual(before, after) {
		t.Fatalf("rebuild differs from incremental:\nbefore %v\nafter  %v", before, after)
	}
}
