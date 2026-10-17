package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brojonat/lifestream/internal/store"
)

type env struct {
	t     *testing.T
	st    *store.Store
	h     http.Handler
	owner string
}

func setup(t *testing.T) env {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	u, _, err := st.AddUser(context.Background(), "me", "+1")
	if err != nil {
		t.Fatal(err)
	}
	return env{t: t, st: st, h: NewHandler(st, slog.New(slog.NewTextHandler(io.Discard, nil))), owner: u.ID}
}

func (e env) do(method, path, body string, header ...string) (int, map[string]any) {
	e.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func (e env) raw(payload string) store.Event {
	e.t.Helper()
	ev, _, err := e.st.Append(context.Background(), store.Event{Owner: e.owner, Source: "twilio.sms", Type: "message.received", Payload: json.RawMessage(payload)})
	if err != nil {
		e.t.Fatal(err)
	}
	return ev
}

func body(v map[string]any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestAgentFlow(t *testing.T) {
	e := setup(t)
	msg := e.raw(`{"Body":"weight 175"}`)

	code, started := e.do("POST", "/api/events", body(map[string]any{
		"owner": e.owner, "type": "collection.started", "parent_id": msg.ID,
		"payload": map[string]any{"name": "weight", "kind": "metric", "unit": "lb", "include": "body weight readings"},
	}), "Idempotency-Key", "toolu_01")
	if code != 201 || started["source"] != "agent" {
		t.Fatalf("start: %d %v", code, started)
	}
	cid := started["id"].(string)

	// Pi Durable retries the tool call: same key, same event, 200.
	code, again := e.do("POST", "/api/events", body(map[string]any{
		"owner": e.owner, "type": "collection.started",
		"payload": map[string]any{"name": "weight", "kind": "metric", "include": "body weight readings"},
	}), "Idempotency-Key", "toolu_01")
	if code != 200 || again["id"] != cid {
		t.Fatalf("retry: %d %v", code, again)
	}

	for _, ev := range []map[string]any{
		{"type": "label.applied", "payload": map[string]any{"event_id": msg.ID, "collection_id": cid, "definition_version": 1, "reason": "weight"}},
		{"type": "metric.observed", "payload": map[string]any{"collection_id": cid, "value": 175}},
	} {
		ev["owner"], ev["parent_id"] = e.owner, msg.ID
		if code, out := e.do("POST", "/api/events", body(ev)); code != 201 {
			t.Fatalf("%s: %d %v", ev["type"], code, out)
		}
	}

	code, out := e.do("GET", "/api/events/"+msg.ID+"/children", "")
	if kids := out["events"].([]any); code != 200 || len(kids) != 3 || kids[1].(map[string]any)["type"] != "label.applied" {
		t.Fatalf("children: %d %v", code, out)
	}
	code, out = e.do("GET", "/api/collections?owner="+e.owner+"&status=open", "")
	cs := out["collections"].([]any)
	if code != 200 || len(cs) != 1 || cs[0].(map[string]any)["name"] != "weight" {
		t.Fatalf("collections: %d %v", code, out)
	}
	code, out = e.do("GET", "/api/collections/"+cid+"/members", "")
	if code != 200 || len(out["members"].([]any)) != 1 {
		t.Fatalf("members: %d %v", code, out)
	}
	if v, _ := e.st.MetricValues(context.Background(), cid, msg.OccurredAt.AddDate(0, 0, -1)); len(v) != 1 || v[0].Value != 175 {
		t.Fatalf("metric value not projected: %v", v)
	}
}

func TestAppendRejects(t *testing.T) {
	e := setup(t)
	cases := map[string]struct {
		body string
		code int
	}{
		"not json":       {`{`, 400},
		"unknown field":  {body(map[string]any{"owner": e.owner, "type": "review.flagged", "source": "cli"}), 400},
		"no owner":       {body(map[string]any{"type": "review.flagged"}), 400},
		"raw type":       {body(map[string]any{"owner": e.owner, "type": "message.received"}), 400},
		"owner corrects": {body(map[string]any{"owner": e.owner, "type": "label.corrected"}), 400},
		"unknown coll":   {body(map[string]any{"owner": e.owner, "type": "collection.stopped", "payload": map[string]any{"collection_id": "x"}}), 422},
		"unknown owner":  {body(map[string]any{"owner": "nobody", "type": "review.flagged"}), 422},
		"string payload": {`{"owner":"` + e.owner + `","type":"review.flagged","payload":"x"}`, 201},
		"missing parent": {body(map[string]any{"owner": e.owner, "type": "review.flagged", "parent_id": "nope"}), 422},
	}
	for name, c := range cases {
		if code, out := e.do("POST", "/api/events", c.body); code != c.code {
			t.Errorf("%s: got %d %v, want %d", name, code, out, c.code)
		}
	}
}

func TestReadLargeEvent(t *testing.T) {
	e := setup(t)
	big := `{"text":"` + strings.Repeat("x", 10000) + `"}`
	ev := e.raw(big)
	small := e.raw(`{"Body":"hi"}`)

	code, out := e.do("GET", "/api/events?owner="+e.owner, "")
	list := out["events"].([]any)
	if code != 200 || len(list) != 2 {
		t.Fatalf("list: %d %v", code, out)
	}
	first, second := list[0].(map[string]any), list[1].(map[string]any)
	if first["payload"] != nil || first["payload_size"].(float64) != float64(len(big)) {
		t.Fatalf("big payload should be elided in lists: %v", first["payload_size"])
	}
	if second["id"] != small.ID || second["payload"].(map[string]any)["Body"] != "hi" {
		t.Fatalf("small payload should be inline: %v", second)
	}

	code, out = e.do("GET", "/api/events/"+ev.ID+"?offset=9000&limit=5000", "")
	if code != 200 || out["offset"].(float64) != 9000 || len(out["data"].(string)) != len(big)-9000 {
		t.Fatalf("slice: %d offset=%v len=%d", code, out["offset"], len(out["data"].(string)))
	}
	code, out = e.do("GET", "/api/events/"+ev.ID, "")
	if code != 200 || out["hash"] != ev.Hash {
		t.Fatalf("full read: %d", code)
	}
	if code, _ := e.do("GET", "/api/events/nope", ""); code != 404 {
		t.Fatalf("unknown id: %d", code)
	}
	if code, out := e.do("GET", "/api/events?owner="+e.owner+"&after="+itoa(small.Seq), ""); code != 200 || len(out["events"].([]any)) != 0 {
		t.Fatalf("after cursor: %d %v", code, out)
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

func TestRules(t *testing.T) {
	e := setup(t)
	if code, out := e.do("POST", "/api/events", body(map[string]any{"owner": e.owner, "type": "rule.added", "payload": map[string]any{"text": "w means weight"}})); code != 201 {
		t.Fatalf("add rule: %d %v", code, out)
	}
	code, out := e.do("GET", "/api/rules?owner="+e.owner, "")
	rules := out["rules"].([]any)
	if code != 200 || len(rules) != 1 || rules[0].(map[string]any)["text"] != "w means weight" {
		t.Fatalf("rules: %d %v", code, out)
	}
}
