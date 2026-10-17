package state

import (
	"context"
	"testing"
	"time"

	"github.com/brojonat/lifestream/internal/bus"
)

func newStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	b, err := bus.Start(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	kv, err := b.KV(ctx, "app")
	if err != nil {
		t.Fatal(err)
	}
	return New(kv), ctx
}

func TestItems(t *testing.T) {
	s, ctx := newStore(t)
	if items, err := s.Items(ctx); err != nil || len(items) != 0 {
		t.Fatalf("empty: %v %v", items, err)
	}
	a, err := s.Put(ctx, Item{Title: "first"})
	if err != nil || a.ID == "" || a.Created.IsZero() {
		t.Fatalf("put: %+v %v", a, err)
	}
	time.Sleep(2 * time.Millisecond)
	b, _ := s.Put(ctx, Item{Title: "second", Starred: true})
	items, _ := s.Items(ctx)
	if len(items) != 2 || items[0].ID != b.ID {
		t.Fatalf("newest first: %+v", items)
	}
	a.Note = "edited"
	if _, err := s.Put(ctx, a); err != nil {
		t.Fatal(err)
	}
	got, ok, _ := s.Item(ctx, a.ID)
	if !ok || got.Note != "edited" || !got.Created.Equal(a.Created) {
		t.Errorf("update should keep id and created: %+v", got)
	}
	if err := s.Delete(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Item(ctx, a.ID); ok {
		t.Error("deleted item still present")
	}
	if err := s.Delete(ctx, "nope"); err != nil {
		t.Errorf("deleting a missing id should not error: %v", err)
	}
}

func TestPreferenceStampAndWatch(t *testing.T) {
	s, ctx := newStore(t)
	if on, _ := s.StarredOnly(ctx); on {
		t.Fatal("preference should default off")
	}
	w, err := s.Watch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Stop()
	s.SetStarredOnly(ctx, true)
	s.Touch(ctx, "fp1")
	for i := 0; i < 2; i++ {
		select {
		case e := <-w.Updates():
			if e == nil {
				t.Fatal("unexpected nil marker with UpdatesOnly")
			}
		case <-ctx.Done():
			t.Fatal("watch did not wake")
		}
	}
	if on, _ := s.StarredOnly(ctx); !on {
		t.Error("preference did not persist")
	}
	if fp, _ := s.Fingerprint(ctx); fp != "fp1" {
		t.Errorf("fingerprint %q", fp)
	}
}
