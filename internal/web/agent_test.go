package web

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brojonat/lifestream/internal/store"
)

// agentServer is a dashboard whose agent feed is the given handler.
func agentServer(t *testing.T, feed http.HandlerFunc) (http.Handler, string) {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	u, _, _ := st.AddUser(context.Background(), "me", "+1")
	url := ""
	if feed != nil {
		agent := httptest.NewServer(feed)
		t.Cleanup(agent.Close)
		url = agent.URL
	}
	return NewHandler(Deps{App: "test", Events: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), AgentURL: url}), u.ID
}

func TestAgentPanel(t *testing.T) {
	var owner string
	snaps := []string{
		`{"at":"2026-10-20T18:00:00Z","main":{"id":"3","busy":true,"handling":"ran 5k <fast>","script":"await tools.query({sql: \"SELECT 1\"})","calls":[{"name":"query","args":"{\"sql\":\"SELECT 1\"}","status":"running"}],"queued":1,"cost_usd":0.0123},"lookbacks":[{"task_id":"9","kind":"lifestream.lookback","phase":"run","child":{"id":"4","busy":true,"handling":"Look-back job (from the system, not an event).","calls":[],"queued":0,"cost_usd":0.002}}]}`,
		`{"at":"2026-10-20T18:00:05Z","main":{"id":"3","busy":false,"script":"await tools.append({})","calls":[{"name":"append","args":"{}","status":"done","ms":2}],"output":"ok: record.saved","queued":0,"last":"kept a workout record","cost_usd":0.02},"lookbacks":[]}`,
	}
	h, id := agentServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/live" || r.URL.Query().Get("owner") != owner {
			http.Error(w, "unexpected "+r.URL.String(), 400)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, s := range snaps {
			fmt.Fprintf(w, "data: %s\n\n", s)
		}
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	owner = id

	if body := do(t, h, "GET", "/agent", "").Body.String(); !strings.Contains(body, "Connecting to the agent") || !strings.Contains(body, `aria-current="page">agent`) {
		t.Fatalf("first paint:\n%s", body)
	}
	got := patches(t, h, "/agent/stream", 2, func() {})
	for _, want := range []string{
		"● working", "1 queued", "$0.01 spent", "ran 5k &lt;fast&gt;",
		`<pre class="script">await tools.query({sql: &#34;SELECT 1&#34;})</pre>`,
		`<li class="running"><code>query</code>`, "Look-back", "task 9 · run", "Look-back job (from the system",
	} {
		if !strings.Contains(got[0], want) {
			t.Errorf("busy patch missing %q", want)
		}
	}
	if !strings.Contains(got[1], "idle") || !strings.Contains(got[1], "kept a workout record") || strings.Contains(got[1], "Look-back") ||
		!strings.Contains(got[1], "<h3>Last script</h3>") || !strings.Contains(got[1], `<li class="done"><code>append</code>`) || !strings.Contains(got[1], "ok: record.saved") {
		t.Errorf("idle patch:\n%s", got[1])
	}
}

func TestAgentPanelWithoutAnAgent(t *testing.T) {
	down, _ := agentServer(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "starting", 503) })
	if got := patches(t, down, "/agent/stream", 1, func() {}); !strings.Contains(got[0], "The agent is unreachable; retrying") {
		t.Errorf("unreachable:\n%s", got[0])
	}
	none, _ := agentServer(t, nil)
	if got := patches(t, none, "/agent/stream", 1, func() {}); !strings.Contains(got[0], "No agent is configured") {
		t.Errorf("unconfigured:\n%s", got[0])
	}
}
