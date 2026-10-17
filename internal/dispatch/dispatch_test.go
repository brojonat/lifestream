package dispatch

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/brojonat/lifestream/internal/store"
)

// fakeAgent records submissions and fails the first `failures` requests.
type fakeAgent struct {
	mu       sync.Mutex
	failures int
	got      []Submission
	calls    int
}

func (a *fakeAgent) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	if r.URL.Path != "/submit" {
		http.NotFound(w, r)
		return
	}
	if a.failures > 0 {
		a.failures--
		http.Error(w, "busy", http.StatusServiceUnavailable)
		return
	}
	var s Submission
	json.NewDecoder(r.Body).Decode(&s)
	a.got = append(a.got, s)
	w.WriteHeader(http.StatusAccepted)
}

func (a *fakeAgent) ids() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for _, s := range a.got {
		out = append(out, s.Event.ID)
	}
	return out
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestDispatchInOrderWithRetries(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	u, _, _ := st.AddUser(ctx, "me", "+1")

	var want []string
	appendRaw := func() {
		e, _, err := st.Append(ctx, store.Event{Owner: u.ID, Source: "cli", Type: "note", Payload: json.RawMessage(`{}`)})
		if err != nil {
			t.Fatal(err)
		}
		want = append(want, e.ID)
	}
	// A backlog from before the dispatcher started (as after a crash).
	appendRaw()
	appendRaw()
	// Agent-written events never go back to the agent.
	if _, _, err := st.Append(ctx, store.Event{Owner: u.ID, Source: store.SourceAgent, Type: "review.flagged", ParentID: want[0]}); err != nil {
		t.Fatal(err)
	}

	agent := &fakeAgent{failures: 2}
	srv := httptest.NewServer(agent)
	defer srv.Close()
	d := New(st, srv.URL, srv.Client(), quiet, Options{MinBackoff: 10 * time.Millisecond, MaxBackoff: 20 * time.Millisecond, Poll: time.Hour})
	go d.Run(ctx)

	waitFor(t, "backlog", func() bool { return len(agent.ids()) == 2 })
	appendRaw() // a live append wakes the loop; Poll is an hour
	waitFor(t, "live event", func() bool { return len(agent.ids()) == 3 })

	got := agent.ids()
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("delivered %v, want %v (in log order)", got, want)
		}
	}
	if agent.got[0].Owner != u.ID {
		t.Fatalf("owner not sent: %+v", agent.got[0])
	}
	if left, _ := st.Undelivered(ctx, 10); len(left) != 0 {
		t.Fatalf("%d events still undelivered", len(left))
	}
}

func TestUndeliveredWaitsForAgent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	u, _, _ := st.AddUser(ctx, "me", "+1")
	st.Append(ctx, store.Event{Owner: u.ID, Source: "cli", Type: "note"})

	down := &fakeAgent{failures: 1 << 30}
	srv := httptest.NewServer(down)
	d := New(st, srv.URL, srv.Client(), quiet, Options{MinBackoff: 5 * time.Millisecond, MaxBackoff: 5 * time.Millisecond})
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()
	waitFor(t, "a few attempts", func() bool { down.mu.Lock(); defer down.mu.Unlock(); return down.calls >= 3 })
	cancel()
	<-done
	srv.Close()

	if left, _ := st.Undelivered(context.Background(), 10); len(left) != 1 {
		t.Fatalf("failed deliveries must stay pending, got %d pending", len(left))
	}
}
