package bus

import (
	"context"
	"testing"
	"time"
)

func TestKVRoundTripAndWatch(t *testing.T) {
	b, err := Start(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	kv, err := b.KV(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	w, err := kv.WatchAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Stop()
	if e := <-w.Updates(); e != nil {
		t.Fatalf("expected nil end-of-replay marker on an empty bucket, got %v", e)
	}
	if _, err := kv.Put(ctx, "k", []byte("v")); err != nil {
		t.Fatal(err)
	}
	select {
	case e := <-w.Updates():
		if e == nil || string(e.Value()) != "v" {
			t.Fatalf("bad update %v", e)
		}
	case <-ctx.Done():
		t.Fatal("no update delivered")
	}
	got, err := kv.Get(ctx, "k")
	if err != nil || string(got.Value()) != "v" {
		t.Fatalf("get: %v %v", got, err)
	}
}
