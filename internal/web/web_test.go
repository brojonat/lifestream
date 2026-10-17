package web

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brojonat/lifestream/internal/bus"
	"github.com/brojonat/lifestream/internal/metrics"
	"github.com/brojonat/lifestream/internal/store"
)

func newServer(t *testing.T) (http.Handler, *store.Store, *metrics.Sampler) {
	t.Helper()
	ctx := context.Background()
	b, err := bus.Start(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
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
	h := NewHandler(Deps{App: "test", Events: st, Log: log,
		Collector: c, MetricsKV: mkv, MetricsStream: mstream, JS: b.JS(), DataDir: t.TempDir(), Interval: time.Second})
	return h, st, metrics.NewSampler(c, b.JS(), mstream, mkv, log)
}

func do(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Datastar-Request", "true")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// world appends events as the agent and a phone would.
type world struct {
	t     *testing.T
	st    *store.Store
	owner string
}

func (w world) add(source, typ, parent string, at time.Time, payload any) store.Event {
	w.t.Helper()
	p, _ := json.Marshal(payload)
	e, _, err := w.st.Append(context.Background(), store.Event{Owner: w.owner, Source: source, Type: typ, ParentID: parent, OccurredAt: at, Payload: p})
	if err != nil {
		w.t.Fatal(err)
	}
	return e
}

// reading is one texted weight, labeled and extracted.
func (w world) reading(collection string, v float64, at time.Time) store.Event {
	msg := w.add("twilio.sms", "message.received", "", at, map[string]string{"Body": "weight " + num(v)})
	w.add(store.SourceAgent, store.TypeLabelApplied, msg.ID, at, store.LabelApplied{EventID: msg.ID, CollectionID: collection, DefinitionVersion: 1, Reason: "weight reading"})
	w.add(store.SourceAgent, store.TypeMetricObserved, msg.ID, at, store.MetricObserved{CollectionID: collection, Value: &v})
	return msg
}

func TestNeedsAUser(t *testing.T) {
	h, st, _ := newServer(t)
	if rec := do(t, h, "GET", "/", ""); rec.Code != 503 || !strings.Contains(rec.Body.String(), "users add") {
		t.Fatalf("no users: %d %q", rec.Code, rec.Body.String())
	}
	st.AddUser(context.Background(), "a", "+1")
	st.AddUser(context.Background(), "b", "+2")
	if rec := do(t, h, "GET", "/inbox", ""); rec.Code != 503 {
		t.Fatalf("two users without login: %d", rec.Code)
	}
}

func TestMetricsPage(t *testing.T) {
	h, st, _ := newServer(t)
	u, _, _ := st.AddUser(context.Background(), "me", "+1")
	w := world{t, st, u.ID}
	if body := do(t, h, "GET", "/", "").Body.String(); !strings.Contains(body, "No metrics yet") {
		t.Fatalf("empty page:\n%s", body)
	}

	now := time.Now()
	weight := w.add(store.SourceAgent, store.TypeCollectionStarted, "", now.AddDate(0, 0, -20),
		store.CollectionStarted{Name: "weight", Kind: store.KindMetric, Unit: "lb", Include: "weight readings"})
	w.reading(weight.ID, 180, now.AddDate(0, 0, -16)) // older than two weeks
	w.reading(weight.ID, 178, now.AddDate(0, 0, -8))  // last week: 178
	w.reading(weight.ID, 176, now.AddDate(0, 0, -3))  // this week: 175
	w.reading(weight.ID, 174, now.AddDate(0, 0, -1))

	body := do(t, h, "GET", "/", "").Body.String()
	for _, want := range []string{
		`<main id="metrics">`,
		`<chart-timeseries id="chart-` + weight.ID + `"`,
		`unit="lb"`,
		`<div class="value">174<span class="unit">lb</span>`,  // latest
		`<div class="value">175<span class="unit">lb</span>`,  // 7-day average
		`<div class="value">▼ -3<span class="unit">lb</span>`, // vs 178
		`average="[[`,
		`data-init="@get('/stream', {openWhenHidden: true})"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(body, `points="[[`) == false {
		t.Errorf("points attribute should carry JSON (escaped by html/template)")
	}

	// The stream repaints when a new reading lands.
	got := patches(t, h, "/stream", 2, func() { w.reading(weight.ID, 172, now) })
	if !strings.Contains(got[0], "174") || !strings.Contains(got[1], "172") {
		t.Errorf("stream did not repaint with the new reading:\n%s", got[1])
	}
}

func TestInboxPage(t *testing.T) {
	ctx := context.Background()
	h, st, _ := newServer(t)
	u, _, _ := st.AddUser(ctx, "me", "+1")
	w := world{t, st, u.ID}
	now := time.Now()
	weight := w.add(store.SourceAgent, store.TypeCollectionStarted, "", now, store.CollectionStarted{Name: "weight", Kind: store.KindMetric, Unit: "lb", Include: "weight"})

	filed := w.reading(weight.ID, 175, now)
	unclear := w.add("twilio.sms", "message.received", "", now, map[string]string{"Body": "w 17"})
	w.add(store.SourceAgent, "review.flagged", unclear.ID, now, map[string]string{"question": "Is 17 a weight?"})
	ignored := w.add("cli", "note", "", now, map[string]string{"text": "<b>hello</b>"})
	waiting := w.add("cli", "note", "", now, map[string]string{"text": "just sent"})
	for _, e := range []store.Event{filed, unclear, ignored} {
		st.MarkDelivered(ctx, e.ID)
	}
	_ = waiting

	body := do(t, h, "GET", "/inbox", "").Body.String()
	for _, want := range []string{
		`<tr class="filed">`, `<span class="chip" title="weight reading">weight</span>`, `<span class="chip">175 lb</span>`,
		`<tr class="flagged">`, "⚠ Is 17 a weight?",
		`<tr class="ignored">`, "&lt;b&gt;hello&lt;/b&gt;",
		`<tr class="waiting">`, "waiting for agent",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	// Agent events are not inbox rows.
	if strings.Count(body, "<tr class=") != 4 {
		t.Errorf("want 4 rows, got %d", strings.Count(body, "<tr class="))
	}
}

func TestSeriesAverage(t *testing.T) {
	day := time.Date(2026, 10, 1, 7, 0, 0, 0, time.UTC)
	var vals []store.MetricValue
	for i, v := range []float64{10, 20, 30, 40, 50, 60, 70, 80, 90} {
		vals = append(vals, store.MetricValue{Value: v, ObservedAt: day.AddDate(0, 0, i)})
	}
	points, avg := series(vals)
	if len(points) != 9 || points[0][1] != 10 {
		t.Fatalf("points: %v", points)
	}
	// Day 6 averages days 0-6; day 7 drops day 0 (exactly 7 days back).
	if avg[0][1] != 10 || avg[6][1] != 40 || avg[7][1] != 50 || avg[8][1] != 60 {
		t.Fatalf("trailing averages: %v", avg)
	}
	if m := mean(vals, day.AddDate(0, 0, 7), day.AddDate(0, 0, 9)); m == nil || *m != 85 {
		t.Fatalf("mean of the last two: %v", m)
	}
	if m := mean(vals, day.AddDate(0, 0, 20), day.AddDate(0, 0, 27)); m != nil {
		t.Fatalf("mean of nothing should be nil, got %v", *m)
	}
}

func TestAdminDashboard(t *testing.T) {
	h, st, sampler := newServer(t)
	st.AddUser(context.Background(), "me", "+1")
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

// patches opens a stream, collects n patch events, calling between() after
// each one but the last, then cancels the request so the handler returns.
func patches(t *testing.T, h http.Handler, path string, n int, between func()) []string {
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
	var out []string
	var b strings.Builder
	sc := bufio.NewScanner(pr)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		b.WriteString(line + "\n")
		if line == "" && strings.Contains(b.String(), "data: elements") {
			out = append(out, b.String())
			b.Reset()
			if len(out) == n {
				break
			}
			between()
		}
	}
	cancel()
	io.Copy(io.Discard, pr)
	if len(out) < n {
		t.Fatalf("got %d of %d patches from %s", len(out), n, path)
	}
	return out
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

func TestCollectionsAndCorrections(t *testing.T) {
	ctx := context.Background()
	h, st, _ := newServer(t)
	u, _, _ := st.AddUser(ctx, "me", "+1")
	w := world{t, st, u.ID}
	now := time.Now()
	weight := w.add(store.SourceAgent, store.TypeCollectionStarted, "", now, store.CollectionStarted{Name: "weight", Kind: store.KindMetric, Unit: "lb", Include: "weight readings"})
	w.add(store.SourceAgent, store.TypeCollectionAmended, "", now, store.CollectionAmended{CollectionID: weight.ID, Exclude: "the dog's weight"})
	reading := w.reading(weight.ID, 175, now)
	note := w.add("cli", "note", "", now, map[string]string{"text": "weighed in at the gym"})

	body := do(t, h, "GET", "/collections", "").Body.String()
	for _, want := range []string{`<a href="/c/` + weight.ID + `">weight</a>`, "● open", "v2", "− the dog&#39;s weight"} {
		if !strings.Contains(body, want) {
			t.Errorf("collections page missing %q", want)
		}
	}
	body = do(t, h, "GET", "/c/"+weight.ID, "").Body.String()
	for _, want := range []string{"started: include weight readings", "exclude the dog&#39;s weight", "weight 175", `value="` + reading.ID + `"`} {
		if !strings.Contains(body, want) {
			t.Errorf("collection page missing %q", want)
		}
	}
	if rec := do(t, h, "GET", "/c/nope", ""); rec.Code != 404 {
		t.Errorf("unknown collection: %d", rec.Code)
	}

	// The owner removes the reading and adds the note; both are events the
	// agent will receive.
	if rec := do(t, h, "POST", "/c/"+weight.ID+"/remove", "event_id="+reading.ID); rec.Code != 204 {
		t.Fatalf("remove: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, h, "POST", "/inbox/add", "event_id="+note.ID+"&collection_id="+weight.ID); rec.Code != 204 {
		t.Fatalf("add: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, h, "POST", "/inbox/add", "event_id=nope&collection_id="+weight.ID); rec.Code != 422 {
		t.Fatalf("add unknown event: %d", rec.Code)
	}
	ms, _ := st.Members(ctx, weight.ID)
	if len(ms) != 1 || ms[0].EventID != note.ID {
		t.Fatalf("members after corrections: %+v", ms)
	}
	pending, _ := st.Undelivered(ctx, 100)
	corrections := 0
	for _, e := range pending {
		if e.Type == store.TypeLabelCorrected && e.Source == SourceDashboard {
			corrections++
		}
	}
	if corrections != 2 {
		t.Fatalf("want 2 label.corrected events queued for the agent, got %d", corrections)
	}
	if body := do(t, h, "GET", "/inbox", "").Body.String(); !strings.Contains(body, `<option value="`+weight.ID+`">weight</option>`) {
		t.Errorf("inbox should offer open collections")
	}
}

func TestRulesPage(t *testing.T) {
	ctx := context.Background()
	h, st, _ := newServer(t)
	u, _, _ := st.AddUser(ctx, "me", "+1")
	w := world{t, st, u.ID}
	w.add(store.SourceAgent, store.TypeRuleAdded, "", time.Now(), store.RuleAdded{Text: "w means weight"})

	if rec := do(t, h, "POST", "/rules", "text=never+sell+%3Cclient%3E+work"); rec.Code != 204 {
		t.Fatalf("add: %d %s", rec.Code, rec.Body.String())
	}
	req := httptest.NewRequest("POST", "/rules", strings.NewReader(`{"text":"sleep is in hours"}`))
	req.Header.Set("Content-Type", "application/json")
	if rec := httptest.NewRecorder(); func() bool { h.ServeHTTP(rec, req); return rec.Code != 204 }() {
		t.Fatalf("add as JSON payload failed")
	}
	if rec := do(t, h, "POST", "/rules", "text=+"); rec.Code != 400 {
		t.Fatalf("empty rule: %d", rec.Code)
	}
	body := do(t, h, "GET", "/rules", "").Body.String()
	for _, want := range []string{"w means weight", "learned", "never sell &lt;client&gt; work", "added"} {
		if !strings.Contains(body, want) {
			t.Errorf("rules page missing %q", want)
		}
	}
	rules, _ := st.Rules(ctx, u.ID)
	if rec := do(t, h, "POST", "/rules/"+rules[0].ID+"/remove", ""); rec.Code != 204 {
		t.Fatalf("remove: %d", rec.Code)
	}
	if rec := do(t, h, "POST", "/rules/nope/remove", ""); rec.Code != 422 {
		t.Fatalf("remove unknown: %d", rec.Code)
	}
	if rules, _ := st.Rules(ctx, u.ID); len(rules) != 2 || rules[0].Source != SourceDashboard {
		t.Fatalf("rules after remove: %+v", rules)
	}
}

func TestRuleLogUndo(t *testing.T) {
	ctx := context.Background()
	h, st, _ := newServer(t)
	u, _, _ := st.AddUser(ctx, "me", "+1")
	w := world{t, st, u.ID}
	now := time.Now()
	mig := w.add(store.SourceAgent, store.TypeCollectionStarted, "", now, store.CollectionStarted{Name: "pg migration", Include: "postgres migration work"})
	inc := w.add(store.SourceAgent, store.TypeCollectionAmended, "", now, store.CollectionAmended{CollectionID: mig.ID, Include: "pgbouncer stuff"})
	stop := w.add(store.SourceAgent, store.TypeCollectionStopped, "", now, store.CollectionRef{CollectionID: mig.ID})

	body := do(t, h, "GET", "/log", "").Body.String()
	for _, want := range []string{"pg migration</a>: stopped", "+ include pgbouncer stuff", "undo (resume it)", "undo (remove rule)"} {
		if !strings.Contains(body, want) {
			t.Errorf("log missing %q", want)
		}
	}
	if strings.Contains(body, "undo (stop it)") {
		t.Errorf("a stopped collection's start has nothing to undo")
	}

	// A misread "stop capturing" is one click to fix.
	if rec := do(t, h, "POST", "/log/"+stop.ID+"/undo", ""); rec.Code != 204 {
		t.Fatalf("undo stop: %d %s", rec.Code, rec.Body.String())
	}
	if c, _ := st.Collection(ctx, mig.ID); c.Status != "open" {
		t.Fatalf("undo should resume: %s", c.Status)
	}
	if rec := do(t, h, "POST", "/log/"+stop.ID+"/undo", ""); rec.Code != 409 {
		t.Fatalf("second undo of the same stop: %d", rec.Code)
	}
	if body := do(t, h, "GET", "/log", "").Body.String(); strings.Count(body, "undo (stop it)") != 1 {
		t.Errorf("only the newest status change offers undo; got %d", strings.Count(body, "undo (stop it)"))
	}
	// Undo an include, then undo the removal.
	if rec := do(t, h, "POST", "/log/"+inc.ID+"/undo", ""); rec.Code != 204 {
		t.Fatalf("undo include: %d %s", rec.Code, rec.Body.String())
	}
	log, _ := st.SteeringLog(ctx, u.ID, 1)
	if rec := do(t, h, "POST", "/log/"+log[0].ID+"/undo", ""); rec.Code != 204 {
		t.Fatalf("undo removal: %d %s", rec.Code, rec.Body.String())
	}
	c, _ := st.Collection(ctx, mig.ID)
	if len(c.Rules) != 2 || c.Rules[1].Text != "pgbouncer stuff" {
		t.Fatalf("rule should be back: %+v", c.Rules)
	}
	if rec := do(t, h, "POST", "/log/nope/undo", ""); rec.Code != 404 {
		t.Fatalf("unknown: %d", rec.Code)
	}
	// Undos are the owner's events: they go to the agent.
	pending, _ := st.Undelivered(ctx, 100)
	if n := len(pending); n != 3 {
		t.Fatalf("want 3 dashboard events queued for the agent, got %d", n)
	}
}

func TestReviewQueue(t *testing.T) {
	ctx := context.Background()
	h, st, _ := newServer(t)
	u, _, _ := st.AddUser(ctx, "me", "+1")
	w := world{t, st, u.ID}
	msg := w.add("twilio.sms", "message.received", "", time.Now(), map[string]string{"Body": "w 17"})
	flag := w.add(store.SourceAgent, "review.flagged", msg.ID, time.Now(), map[string]string{"question": "Is 17 a weight?"})

	body := do(t, h, "GET", "/inbox", "").Body.String()
	for _, want := range []string{"Needs your answer", "Is 17 a weight?", "about: “w 17”", "/review/" + flag.ID + "/answer"} {
		if !strings.Contains(body, want) {
			t.Errorf("inbox missing %q", want)
		}
	}
	if rec := do(t, h, "POST", "/review/"+flag.ID+"/answer", "answer=+"); rec.Code != 400 {
		t.Fatalf("empty answer: %d", rec.Code)
	}
	// The page posts {payload: {answer}} as JSON.
	req := httptest.NewRequest("POST", "/review/"+flag.ID+"/answer", strings.NewReader(`{"answer":"no, I meant 177"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 204 {
		t.Fatalf("answer: %d %s", rec.Code, rec.Body.String())
	}
	if body := do(t, h, "GET", "/inbox", "").Body.String(); strings.Contains(body, "Needs your answer") {
		t.Errorf("answered flag should leave the queue")
	}
	pending, _ := st.Undelivered(ctx, 100)
	var answer store.Event
	for _, e := range pending {
		if e.Type == "review.answered" {
			answer = e
		}
	}
	var p map[string]string
	json.Unmarshal(answer.Payload, &p)
	if answer.ParentID != flag.ID || p["question"] != "Is 17 a weight?" || p["answer"] != "no, I meant 177" || p["event_id"] != msg.ID {
		t.Fatalf("answer event: %+v %v", answer, p)
	}
}

func TestArtifactsPage(t *testing.T) {
	ctx := context.Background()
	h, st, _ := newServer(t)
	u, _, _ := st.AddUser(ctx, "me", "+1")

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("html", "postgres-upgrades.html")
	fw.Write([]byte(`<!doctype html><title>x</title><script>alert("owned")</script>`))
	mw.Close()
	req := httptest.NewRequest("POST", "/artifacts", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 204 {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body.String())
	}

	link := func(payload string) int {
		req := httptest.NewRequest("POST", "/artifacts", strings.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := link(`{"title":"Vacuum, explained","url":"https://claude.ai/public/artifacts/0b7e"}`); code != 204 {
		t.Fatalf("link: %d", code)
	}
	if code := link(`{"title":"x","url":"https://example.com/a"}`); code != 422 {
		t.Fatalf("a non-claude.ai link: %d", code)
	}

	page := do(t, h, "GET", "/artifacts", "").Body.String()
	for _, want := range []string{"postgres-upgrades", "HTML 0.1 KB", "Vacuum, explained", `href="https://claude.ai/public/artifacts/0b7e"`} {
		if !strings.Contains(page, want) {
			t.Errorf("artifacts page missing %q", want)
		}
	}
	if strings.Contains(page, "owned") {
		t.Fatal("uploaded HTML must never be rendered on the dashboard")
	}
	list, _ := st.Artifacts(ctx, u.ID, 10)
	if len(list) != 2 || list[1].Source != SourceDashboard {
		t.Fatalf("artifacts: %+v", list)
	}
}

func TestTopicsPage(t *testing.T) {
	ctx := context.Background()
	h, st, _ := newServer(t)
	u, _, _ := st.AddUser(ctx, "me", "+1")
	post := func(path, payload string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", path, strings.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if rec := post("/topics", `{"name":"my <b>new</b> stock","kind":"monthly","price":"5"}`); rec.Code != 204 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	if rec := post("/topics", `{"name":"x","kind":"monthly","price":"five"}`); rec.Code != 400 {
		t.Fatalf("bad price: %d", rec.Code)
	}
	ts, _ := st.Topics(ctx, u.ID)
	if len(ts) != 1 || ts[0].Price.AmountCents != 500 {
		t.Fatalf("topics: %+v", ts)
	}
	id := ts[0].ID
	w := world{t, st, u.ID}
	w.add("stripe", store.TypeTopicSubscribed, "", time.Now(), store.TopicSubscribed{TopicID: id, Subscriber: "ann@example.com"})

	if rec := post("/topics/"+id+"/history", `{"from":"2026-10-01"}`); rec.Code != 204 {
		t.Fatalf("history: %d %s", rec.Code, rec.Body.String())
	}
	if tp, _ := st.Topic(ctx, id); tp.HistoryStart == nil || tp.HistoryStart.Format("2006-01-02") != "2026-10-01" {
		t.Fatalf("history start: %+v", tp)
	}
	if rec := post("/topics/"+id+"/history", `{"from":""}`); rec.Code != 204 {
		t.Fatalf("clear history: %d", rec.Code)
	}

	if rec := post("/topics/"+id+"/send", `{"text":"buy","confirm":"0"}`); rec.Code != 409 {
		t.Fatalf("a stale count must be refused: %d", rec.Code)
	}
	if rec := post("/topics/"+id+"/send", `{"text":"{\"ticker\":\"LIFE\"}","confirm":"1"}`); rec.Code != 204 {
		t.Fatalf("send: %d %s", rec.Code, rec.Body.String())
	}
	inbox, _ := st.SubscriberInbox(ctx, "ann@example.com", 10)
	if len(inbox) != 1 || string(inbox[0].Payload) != `{"ticker":"LIFE"}` {
		t.Fatalf("inbox: %+v", inbox)
	}

	page := do(t, h, "GET", "/topics", "").Body.String()
	for _, want := range []string{"my &lt;b&gt;new&lt;/b&gt; stock", "$5.00", "a month", "<strong>1</strong> subscriber", "Send to 1"} {
		if !strings.Contains(page, want) {
			t.Errorf("topics page missing %q", want)
		}
	}
	if strings.Contains(page, "<b>new</b>") {
		t.Fatal("topic name rendered as HTML")
	}
}

func TestTopicNameIsEscapedInsideTheConfirm(t *testing.T) {
	ctx := context.Background()
	h, st, _ := newServer(t)
	u, _, _ := st.AddUser(ctx, "me", "+1")
	w := world{t, st, u.ID}
	w.add(SourceDashboard, store.TypeTopicCreated, "", time.Now(), store.TopicCreated{Name: `x'); alert('owned`, Price: store.Price{Kind: store.PriceOneTime}})
	page := do(t, h, "GET", "/topics", "").Body.String()
	i := strings.Index(page, "confirm(")
	if i < 0 {
		t.Fatal("no confirm")
	}
	attr := page[i : i+120]
	if strings.Contains(attr, `x'); alert('owned`) || strings.Contains(attr, "x&#39;); alert(") {
		t.Fatalf("topic name breaks out of the JS string: %s", attr)
	}
}

func TestMoveChapters(t *testing.T) {
	ctx := context.Background()
	h, st, _ := newServer(t)
	u, _, _ := st.AddUser(ctx, "me", "+1")
	w := world{t, st, u.ID}
	book := w.add(store.SourceAgent, store.TypeCollectionStarted, "", time.Now(), store.CollectionStarted{Name: "book", Include: "artifacts"})
	var ids []string
	for _, title := range []string{"intro", "vacuum"} {
		a := w.add(SourceDashboard, store.TypeArtifactSaved, "", time.Now(), store.ArtifactSaved{Title: title, HTML: "<p>x"})
		w.add(store.SourceAgent, store.TypeLabelApplied, a.ID, time.Now(), store.LabelApplied{EventID: a.ID, CollectionID: book.ID, DefinitionVersion: 1, Reason: "book"})
		ids = append(ids, a.ID)
	}
	move := func(id, dir string) int {
		req := httptest.NewRequest("POST", "/c/"+book.ID+"/move", strings.NewReader(`{"event_id":"`+id+`","dir":"`+dir+`"}`))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	page := do(t, h, "GET", "/c/"+book.ID, "").Body.String()
	if strings.Index(page, "vacuum") > strings.Index(page, "intro") || !strings.Contains(page, "chapter order") {
		t.Fatal("unordered members should list newest first, with the chapter-order note")
	}
	if code := move(ids[0], "up"); code != 204 {
		t.Fatalf("move intro up: %d", code)
	}
	if page = do(t, h, "GET", "/c/"+book.ID, "").Body.String(); strings.Index(page, "intro") > strings.Index(page, "vacuum") {
		t.Fatal("intro should come first now")
	}
	if code := move(ids[0], "up"); code != 422 {
		t.Fatalf("moving the first chapter up: %d", code)
	}
}
