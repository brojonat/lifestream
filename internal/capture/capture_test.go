package capture

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/brojonat/lifestream/internal/ingest"
	"github.com/brojonat/lifestream/internal/store"
)

type rig struct {
	t      *testing.T
	st     *store.Store
	owner  string
	cfg    Config
	stdin  []byte
	path   string // the transcript, a copy of the fixture
	posts  atomic.Int32
	failAt func(n int32, w http.ResponseWriter, r *http.Request, real http.Handler) bool
}

func newRig(t *testing.T) *rig {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(context.Background(), filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	u, token, err := st.AddUser(context.Background(), "me", "+1")
	if err != nil {
		t.Fatal(err)
	}
	g := &rig{t: t, st: st, owner: u.ID, path: filepath.Join(dir, "transcript.jsonl")}
	fixture, err := os.ReadFile("../../testdata/claude-code/transcript.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(g.path, fixture, 0o600); err != nil {
		t.Fatal(err)
	}
	hook, _ := os.ReadFile("../../testdata/claude-code/stop.json")
	g.stdin = []byte(strings.Replace(string(hook), "TRANSCRIPT", g.path, 1))

	real := ingest.Events(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := g.posts.Add(1)
		if g.failAt != nil && g.failAt(n, w, r, real) {
			return
		}
		real.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	g.cfg = Config{URL: srv.URL, Token: token, On: true, StateDir: filepath.Join(dir, "state")}
	return g
}

func (g *rig) run() (Result, error) {
	return Run(context.Background(), g.cfg, ClaudeCode, g.stdin)
}

func (g *rig) messages() []store.Event {
	g.t.Helper()
	evs, err := g.st.Events(context.Background(), g.owner, 0, 1000)
	if err != nil {
		g.t.Fatal(err)
	}
	return evs
}

type sent struct {
	SessionID string          `json:"session_id"`
	Sequence  int64           `json:"sequence"`
	Harness   string          `json:"harness"`
	Model     string          `json:"model"`
	Cwd       string          `json:"cwd"`
	Message   json.RawMessage `json:"message"`
}

func sequences(t *testing.T, evs []store.Event) []int64 {
	t.Helper()
	var out []int64
	for _, e := range evs {
		var p sent
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatal(err)
		}
		out = append(out, p.Sequence)
	}
	return out
}

// The fixture's messages are lines 1, 2, 3, 6 and 7; the rest is bookkeeping.
var want = []int64{1, 2, 3, 6, 7}

func TestClaudeCodeTranscript(t *testing.T) {
	g := newRig(t)
	res, err := g.run()
	if err != nil || res.Sent != len(want) {
		t.Fatalf("first run: %+v %v", res, err)
	}
	evs := g.messages()
	if got := sequences(t, evs); !slices.Equal(got, want) {
		t.Fatalf("sequences %v, want %v", got, want)
	}
	var p sent
	json.Unmarshal(evs[1].Payload, &p)
	if evs[1].Type != store.TypeLLMMessage || evs[1].Source != "claude-code" || p.SessionID != "0b7e-fixture" ||
		p.Harness != "claude-code" || p.Model != "claude-opus-5-5" || p.Cwd != "/home/me/src/pgmig" {
		t.Fatalf("event: %+v payload %+v", evs[1], p)
	}
	if got := evs[1].OccurredAt.Format("15:04:05"); got != "15:00:04" {
		t.Fatalf("occurred_at %s, want the line's timestamp", got)
	}
	lines := strings.Split(string(mustRead(t, g.path)), "\n")
	if string(p.Message) != lines[2] {
		t.Fatalf("message is not the line as written:\n%s\n%s", p.Message, lines[2])
	}

	// Nothing new: nothing sent, nothing posted.
	before := g.posts.Load()
	if res, err := g.run(); err != nil || res.Sent != 0 || g.posts.Load() != before {
		t.Fatalf("rerun: %+v %v (%d posts)", res, err, g.posts.Load()-before)
	}

	// A new line and a half-written one: only the complete line goes.
	f, _ := os.OpenFile(g.path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"type":"user","timestamp":"2026-10-17T15:01:00Z","message":{"role":"user","content":"thanks"}}` + "\n" + `{"type":"assistant","mess`)
	f.Close()
	if res, err := g.run(); err != nil || res.Sent != 1 {
		t.Fatalf("after appending: %+v %v", res, err)
	}
	if got := sequences(t, g.messages()); !slices.Equal(got, append(want, 8)) {
		t.Fatalf("sequences %v", got)
	}
}

func TestKilledMidSendLosesNothingAndDuplicatesNothing(t *testing.T) {
	g := newRig(t)
	// The service stores the batch, then the connection drops before the
	// answer, as if capture were killed mid-send.
	g.failAt = func(n int32, w http.ResponseWriter, r *http.Request, real http.Handler) bool {
		if n != 1 {
			return false
		}
		real.ServeHTTP(httptest.NewRecorder(), r)
		conn, _, _ := w.(http.Hijacker).Hijack()
		conn.Close()
		return true
	}
	_, err := g.run()
	var te *TransportError
	if !errors.As(err, &te) {
		t.Fatalf("want a TransportError, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(g.cfg.StateDir, "claude-code", "0b7e-fixture.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("offset saved after a failed send: %v", err)
	}
	if res, err := g.run(); err != nil || res.Sent != len(want) {
		t.Fatalf("rerun: %+v %v", res, err)
	}
	if got := sequences(t, g.messages()); !slices.Equal(got, want) {
		t.Fatalf("sequences %v, want each of %v once", got, want)
	}
}

func TestOffsetAdvancesPerBatch(t *testing.T) {
	g := newRig(t)
	g.cfg.MaxBatch = 1 // every message is its own POST
	g.failAt = func(n int32, w http.ResponseWriter, _ *http.Request, _ http.Handler) bool {
		if n == 3 {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return true
		}
		return false
	}
	res, err := g.run()
	var te *TransportError
	if !errors.As(err, &te) || res.Sent != 2 {
		t.Fatalf("want 2 sent then a TransportError, got %+v %v", res, err)
	}
	if res, err := g.run(); err != nil || res.Sent != 3 {
		t.Fatalf("rerun sends the other 3: %+v %v", res, err)
	}
	if got := sequences(t, g.messages()); !slices.Equal(got, want) {
		t.Fatalf("sequences %v", got)
	}
}

func TestQuietUnlessOnAndNotDenied(t *testing.T) {
	g := newRig(t)
	g.cfg.On = false
	if res, err := g.run(); err != nil || res.Skipped == "" {
		t.Fatalf("capture off: %+v %v", res, err)
	}
	g.cfg.On, g.cfg.Deny = true, []string{"/elsewhere", "/home/me/src"}
	if res, err := g.run(); err != nil || !strings.Contains(res.Skipped, "/home/me/src") {
		t.Fatalf("denied: %+v %v", res, err)
	}
	if g.posts.Load() != 0 || len(g.messages()) != 0 {
		t.Fatal("something was sent")
	}
	g.cfg.Deny, g.cfg.Token = nil, ""
	if _, err := g.run(); err == nil {
		t.Fatal("capture on without a token should fail")
	}
}

func TestDenied(t *testing.T) {
	home, _ := os.UserHomeDir()
	for _, c := range []struct {
		patterns []string
		cwd      string
		want     string
	}{
		{[]string{"~/work"}, home + "/work/api/cmd", "~/work"},
		{[]string{"~/work*"}, home + "/workshop", "~/work*"},
		{[]string{"/srv/*/secret"}, "/srv/a/secret/x", "/srv/*/secret"},
		{[]string{"~/work"}, home + "/play", ""},
		{[]string{"", " "}, "/x", ""},
		{[]string{"/x"}, "", ""},
	} {
		if got := Denied(c.patterns, c.cwd); got != c.want {
			t.Errorf("Denied(%v, %q) = %q, want %q", c.patterns, c.cwd, got, c.want)
		}
	}
}

func TestStripCredentials(t *testing.T) {
	for in, want := range map[string]string{
		"https://me:ghp_secret@github.com/me/repo.git": "https://github.com/me/repo.git",
		"git@github.com:me/repo.git":                   "git@github.com:me/repo.git",
		"https://github.com/me/repo":                   "https://github.com/me/repo",
	} {
		if got := stripCredentials(in); got != want {
			t.Errorf("%s: got %s", in, got)
		}
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestGitInfo(t *testing.T) {
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "trunk"},
		{"-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "x"},
		{"remote", "add", "origin", "https://me:ghp_secret@github.com/me/repo.git"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Skipf("git unavailable: %v %s", err, out)
		}
	}
	g := gitInfo(context.Background(), filepath.Join(dir))
	if g == nil || g.Branch != "trunk" || len(g.Commit) != 40 || g.Remote != "https://github.com/me/repo.git" {
		t.Fatalf("gitInfo: %+v", g)
	}
	if gitInfo(context.Background(), t.TempDir()) != nil {
		t.Fatal("not a repo: want nil")
	}
}

func TestPiSession(t *testing.T) {
	g := newRig(t)
	stdin, _ := json.Marshal(map[string]string{"session_id": "019e-fixture", "transcript_path": "../../testdata/pi/session.jsonl", "cwd": "/home/me/src/pgmig"})
	res, err := Run(context.Background(), g.cfg, Pi, stdin)
	if err != nil || res.Sent != 4 {
		t.Fatalf("run: %+v %v", res, err)
	}
	evs := g.messages()
	if got := sequences(t, evs); !slices.Equal(got, []int64{2, 3, 4, 6}) {
		t.Fatalf("sequences %v: want the four message entries", got)
	}
	var p sent
	json.Unmarshal(evs[1].Payload, &p)
	if evs[1].Source != "pi" || p.Model != "claude-haiku-4-5-20251001" {
		t.Fatalf("assistant event: %+v %+v", evs[1], p)
	}
}

func TestCodexRolloutFoundByThreadID(t *testing.T) {
	g := newRig(t)
	home := t.TempDir()
	day := filepath.Join(home, "sessions", "2026", "10", "17")
	os.MkdirAll(day, 0o700)
	os.WriteFile(filepath.Join(day, "rollout-2026-10-17T17-00-00-019f-fixture.jsonl"), mustRead(t, "../../testdata/codex/rollout.jsonl"), 0o600)
	notify := mustRead(t, "../../testdata/codex/notify.json")
	res, err := Run(context.Background(), g.cfg, Codex(home), notify)
	if err != nil || res.Sent != 4 {
		t.Fatalf("run: %+v %v", res, err)
	}
	evs := g.messages()
	if got := sequences(t, evs); !slices.Equal(got, []int64{2, 4, 5, 6}) {
		t.Fatalf("sequences %v: want the four response_item lines", got)
	}
	var p sent
	json.Unmarshal(evs[0].Payload, &p)
	if evs[0].Source != "codex" || p.SessionID != "019f-fixture" || p.Model != "gpt-6-luna" || p.Cwd != "/home/me/src/pgmig" {
		t.Fatalf("event: %+v payload %+v", evs[0], p)
	}
	if _, err := Run(context.Background(), g.cfg, Codex(home), []byte(`{"type":"agent-turn-complete","thread-id":"nope"}`)); err == nil {
		t.Fatal("an unknown thread should fail")
	}
}
