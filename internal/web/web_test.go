package web

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/brojonat/lifestream/internal/bus"
	"github.com/brojonat/lifestream/internal/metrics"
	"github.com/brojonat/lifestream/internal/state"
)

func newServer(t *testing.T) (http.Handler, *state.Store, *metrics.Sampler) {
	t.Helper()
	b, err := bus.Start(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	ctx := context.Background()
	kv, err := b.KV(ctx, "app")
	if err != nil {
		t.Fatal(err)
	}
	mstream, err := b.Stream(ctx, metrics.StreamName, []string{metrics.Subject}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	mkv, err := b.KV(ctx, "metrics")
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := metrics.NewCollector()
	store := state.New(kv)
	h := NewHandler(Deps{App: "test", Store: store, Log: log,
		Collector: c, MetricsKV: mkv, MetricsStream: mstream, JS: b.JS(), DataDir: t.TempDir(), Interval: time.Second})
	return h, store, metrics.NewSampler(c, b.JS(), mstream, mkv, log)
}

func do(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
		if strings.Contains(body, "=") && !strings.HasPrefix(body, "{") {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		req.Header.Set("Datastar-Request", "true")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestItemsLoop(t *testing.T) {
	h, store, _ := newServer(t)
	body := do(t, h, "GET", "/", "").Body.String()
	for _, want := range []string{`<main id="items">`, `@get('/items/stream', {openWhenHidden: true})`, "Nothing yet", `data-bind:q`, `<div id="results"></div>`, `<div id="filter">`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	// Writes render nothing.
	if rec := do(t, h, "POST", "/items", "title=Buy+milk"); rec.Code != 204 || rec.Body.Len() != 0 {
		t.Fatalf("add: %d %q", rec.Code, rec.Body.String())
	}
	if rec := do(t, h, "POST", "/items", "title="); rec.Code != 400 {
		t.Errorf("empty title: %d", rec.Code)
	}
	items, _ := store.Items(context.Background())
	if len(items) != 1 || items[0].Title != "Buy milk" {
		t.Fatalf("items: %+v", items)
	}
	id := items[0].ID
	if rec := do(t, h, "POST", "/i/"+id+"/star", ""); rec.Code != 204 {
		t.Fatalf("star: %d", rec.Code)
	}
	if rec := do(t, h, "PUT", "/i/"+id+"/note", `{"note":"2%\nnot <b>whole</b>"}`); rec.Code != 204 {
		t.Fatalf("note: %d", rec.Code)
	}
	body = do(t, h, "GET", "/i/"+id, "").Body.String()
	if !strings.Contains(body, "★") || !strings.Contains(body, "<div class=\"note-text\">2%\nnot &lt;b&gt;whole&lt;/b&gt;</div>") {
		t.Errorf("item page:\n%s", body)
	}
	if rec := do(t, h, "GET", "/i/nope", ""); rec.Code != 404 {
		t.Errorf("unknown item: %d", rec.Code)
	}
	// The stream's first patch carries the new state.
	first := firstPatch(t, h, "/items/stream")
	if !strings.Contains(first, "event: datastar-patch-elements") || !strings.Contains(first, "★") || !strings.Contains(first, "Buy milk") {
		t.Errorf("stream patch:\n%s", first)
	}
	// Search patches the results slot.
	rec := do(t, h, "GET", "/search?datastar="+url.QueryEscape(`{"q":"milk"}`), "")
	if !strings.Contains(rec.Body.String(), `href="/i/`+id+`"`) {
		t.Errorf("search:\n%s", rec.Body.String())
	}
	if rec := do(t, h, "GET", "/search?datastar="+url.QueryEscape(`{"q":"zzz"}`), ""); !strings.Contains(rec.Body.String(), "Nothing matches") {
		t.Errorf("no match:\n%s", rec.Body.String())
	}
	// Starred-only filter hides unstarred items.
	store.Put(context.Background(), state.Item{Title: "plain"})
	do(t, h, "POST", "/prefs/starred", "")
	body = do(t, h, "GET", "/", "").Body.String()
	if strings.Contains(body, "plain") || !strings.Contains(body, "Buy milk") || !strings.Contains(body, `aria-pressed="true"`) {
		t.Errorf("filter:\n%s", body)
	}
	// Delete.
	if rec := do(t, h, "DELETE", "/i/"+id, ""); rec.Code != 204 {
		t.Fatalf("delete: %d", rec.Code)
	}
	if _, ok, _ := store.Item(context.Background(), id); ok {
		t.Error("item should be gone")
	}
}

func TestAdminDashboard(t *testing.T) {
	h, _, sampler := newServer(t)
	if rec := do(t, h, "GET", "/admin", ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), "No samples yet") {
		t.Fatalf("empty dashboard: %d", rec.Code)
	}
	for i := 0; i < 3; i++ {
		if err := sampler.Once(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	body := do(t, h, "GET", "/admin", "").Body.String()
	for _, want := range []string{`<main id="admin"`, `<svg class="spark"`, `role="meter"`, "RLIMIT_NOFILE", `href="/metrics"`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	if rec := do(t, h, "PUT", "/admin/threshold/heap_mb", "value=0.001"); rec.Code != 204 {
		t.Fatalf("threshold: %d", rec.Code)
	}
	if body := do(t, h, "GET", "/admin", "").Body.String(); !strings.Contains(body, `class="tile over"`) {
		t.Errorf("breached threshold should flag")
	}
	if rec := do(t, h, "GET", "/metrics", ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), "app_requests_total") {
		t.Errorf("prometheus endpoint: %d", rec.Code)
	}
	if rec := do(t, h, "GET", "/healthz", ""); rec.Body.String() != "ok\n" {
		t.Errorf("healthz: %q", rec.Body.String())
	}
	for _, p := range []string{"/favicon.png", "/apple-touch-icon.png"} {
		if rec := do(t, h, "GET", p, ""); rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "image/png") {
			t.Errorf("%s: %d %s", p, rec.Code, rec.Header().Get("Content-Type"))
		}
	}
	if body := do(t, h, "GET", "/", "").Body.String(); !strings.Contains(body, `<link rel="icon" type="image/png" href="/favicon.png">`) {
		t.Errorf("layout should link the favicon")
	}
}

// firstPatch opens a stream and returns everything received in the first
// event, then cancels the request so the handler returns.
func firstPatch(t *testing.T, h http.Handler, path string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	pr, pw := io.Pipe()
	req := httptest.NewRequest("GET", path, nil).WithContext(ctx)
	rec := &pipeWriter{header: http.Header{}, w: pw}
	go func() {
		h.ServeHTTP(rec, req)
		pw.Close()
	}()
	var b strings.Builder
	sc := bufio.NewScanner(pr)
	for sc.Scan() {
		line := sc.Text()
		b.WriteString(line + "\n")
		if line == "" && strings.Contains(b.String(), "data: elements") {
			break
		}
	}
	cancel()
	io.Copy(io.Discard, pr)
	return b.String()
}

type pipeWriter struct {
	header http.Header
	w      io.Writer
	status int
}

func (p *pipeWriter) Header() http.Header         { return p.header }
func (p *pipeWriter) Write(b []byte) (int, error) { return p.w.Write(b) }
func (p *pipeWriter) WriteHeader(code int)        { p.status = code }
func (p *pipeWriter) Flush()                      {}
