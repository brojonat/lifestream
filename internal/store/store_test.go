package store

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func open(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func addUser(t *testing.T, s *Store, phone string) User {
	t.Helper()
	u, _, err := s.AddUser(context.Background(), "test", phone)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func appendN(t *testing.T, s *Store, owner string, n int) []Event {
	t.Helper()
	var out []Event
	for i := range n {
		e, _, err := s.Append(context.Background(), Event{
			Owner: owner, Source: "cli", Type: "note",
			Payload: json.RawMessage(`{"text": "note ` + string(rune('a'+i)) + `"}`),
		})
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

func TestAppendChainsPerOwner(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	a, b := addUser(t, s, "+1"), addUser(t, s, "+2")
	ea := appendN(t, s, a.ID, 3)
	eb := appendN(t, s, b.ID, 2)

	if ea[0].PrevHash != "" || eb[0].PrevHash != "" {
		t.Fatal("first event of each owner should start a new chain")
	}
	if ea[1].PrevHash != ea[0].Hash || ea[2].PrevHash != ea[1].Hash {
		t.Fatal("owner a's events are not chained")
	}
	if eb[1].PrevHash != eb[0].Hash {
		t.Fatal("owner b's chain picked up owner a's events")
	}
	if string(ea[0].Payload) != `{"text":"note a"}` {
		t.Fatalf("payload not compacted: %s", ea[0].Payload)
	}
	for _, u := range []User{a, b} {
		if err := s.Verify(ctx, u.ID); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Event(ctx, ea[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Hash != ea[1].Hash || !got.OccurredAt.Equal(ea[1].OccurredAt) {
		t.Fatalf("round trip changed the event: %+v vs %+v", got, ea[1])
	}
}

func TestEventsAreAppendOnly(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	u := addUser(t, s, "+1")
	e := appendN(t, s, u.ID, 1)[0]
	for _, q := range []string{
		`UPDATE events SET payload = '{}' WHERE id = ?`,
		`DELETE FROM events WHERE id = ?`,
	} {
		_, err := s.db.ExecContext(ctx, q, e.ID)
		if err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Fatalf("%s: want append-only error, got %v", q, err)
		}
	}
}

func TestTamperingBreaksTheChain(t *testing.T) {
	cases := map[string]string{
		"payload": `UPDATE events SET payload = '{"text":"forged"}' WHERE id = ?`,
		"type":    `UPDATE events SET type = 'metric.observed' WHERE id = ?`,
		"time":    `UPDATE events SET occurred_at = '2020-01-01T00:00:00Z' WHERE id = ?`,
	}
	for name, q := range cases {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s, _ := open(t)
			u := addUser(t, s, "+1")
			evs := appendN(t, s, u.ID, 4)
			// Get around the trigger, as someone editing the file would.
			if _, err := s.db.ExecContext(ctx, `DROP TRIGGER events_no_update`); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.ExecContext(ctx, q, evs[1].ID); err != nil {
				t.Fatal(err)
			}
			var ce *ChainError
			if err := s.Verify(ctx, u.ID); !errors.As(err, &ce) || ce.EventID != evs[1].ID {
				t.Fatalf("want chain break at %s, got %v", evs[1].ID, err)
			}
		})
	}

	t.Run("rehashed event breaks the next link", func(t *testing.T) {
		ctx := context.Background()
		s, _ := open(t)
		u := addUser(t, s, "+1")
		evs := appendN(t, s, u.ID, 3)
		forged := evs[1]
		forged.Payload = json.RawMessage(`{"text":"forged"}`)
		forged.Hash = chainHash(forged.PrevHash, forged)
		s.db.ExecContext(ctx, `DROP TRIGGER events_no_update`)
		if _, err := s.db.ExecContext(ctx, `UPDATE events SET payload = ?, hash = ? WHERE id = ?`,
			string(forged.Payload), forged.Hash, forged.ID); err != nil {
			t.Fatal(err)
		}
		var ce *ChainError
		if err := s.Verify(ctx, u.ID); !errors.As(err, &ce) || ce.EventID != evs[2].ID {
			t.Fatalf("want chain break at %s, got %v", evs[2].ID, err)
		}
	})
}

func TestIdempotencyKey(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	a, b := addUser(t, s, "+1"), addUser(t, s, "+2")
	e := Event{Owner: a.ID, Source: "twilio.sms", Type: "message.received", IdempotencyKey: "SM123", Payload: json.RawMessage(`{"body":"weight 175"}`)}
	first, created, err := s.Append(ctx, e)
	if err != nil || !created {
		t.Fatalf("first append: created=%v err=%v", created, err)
	}
	again, created, err := s.Append(ctx, e)
	if err != nil || created || again.ID != first.ID {
		t.Fatalf("retry should return the stored event: created=%v id=%s err=%v", created, again.ID, err)
	}
	e.Owner = b.ID
	if _, created, err := s.Append(ctx, e); err != nil || !created {
		t.Fatalf("keys are per owner: created=%v err=%v", created, err)
	}
}

func TestAppendValidates(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	u := addUser(t, s, "+1")
	if _, _, err := s.Append(ctx, Event{Owner: u.ID, Source: "cli"}); err == nil {
		t.Fatal("missing type should fail")
	}
	if _, _, err := s.Append(ctx, Event{Owner: u.ID, Source: "cli", Type: "note", Payload: json.RawMessage(`{nope`)}); err == nil {
		t.Fatal("bad JSON should fail")
	}
	if _, _, err := s.Append(ctx, Event{Owner: "nobody", Source: "cli", Type: "note"}); err == nil {
		t.Fatal("unknown owner should fail")
	}
}

func TestUndeliveredSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	s, path := open(t)
	u := addUser(t, s, "+1")
	raw := appendN(t, s, u.ID, 3)
	if _, _, err := s.Append(ctx, Event{Owner: u.ID, Source: SourceAgent, Type: "label.applied", ParentID: raw[0].ID}); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkDelivered(ctx, raw[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkDelivered(ctx, raw[0].ID); err != nil {
		t.Fatalf("marking twice: %v", err)
	}
	s.Close()

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.Undelivered(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != raw[1].ID || got[1].ID != raw[2].ID {
		t.Fatalf("want raw[1], raw[2] (agent events never dispatch), got %v", ids(got))
	}
}

func TestChangedFiresOnAppend(t *testing.T) {
	s, _ := open(t)
	u := addUser(t, s, "+1")
	ch := s.Changed()
	appendN(t, s, u.ID, 1)
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("append did not signal")
	}
}

func TestUserLookups(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	u, token, err := s.AddUser(ctx, "me", "+15555550100")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.UserByToken(ctx, token); err != nil || got.ID != u.ID {
		t.Fatalf("by token: %v %v", got, err)
	}
	if got, err := s.UserByPhone(ctx, "+15555550100"); err != nil || got.ID != u.ID {
		t.Fatalf("by phone: %v %v", got, err)
	}
	if _, err := s.UserByToken(ctx, "wrong"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bad token: %v", err)
	}
	if _, _, err := s.AddUser(ctx, "dup", "+15555550100"); err == nil {
		t.Fatal("duplicate phone should fail")
	}
}

func ids(evs []Event) []string {
	var out []string
	for _, e := range evs {
		out = append(out, e.ID)
	}
	return out
}

func TestIDsSortInAppendOrder(t *testing.T) {
	s, _ := open(t)
	u := addUser(t, s, "+1")
	evs := appendN(t, s, u.ID, 50)
	for i := 1; i < len(evs); i++ {
		if evs[i].ID <= evs[i-1].ID {
			t.Fatalf("id %d (%s) does not sort after %s", i, evs[i].ID, evs[i-1].ID)
		}
	}
}
