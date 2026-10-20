package web

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/starfederation/datastar-go/datastar"
)

// The live agent panel. The agent publishes a snapshot of what it's doing
// (agent/src/live.ts) on GET {AgentURL}/live?owner=, one SSE message per
// change; only this server can reach it. /agent/stream relays it: each
// snapshot is rendered through the same template as the first paint and
// patched in, like every other page.

// agentSnapshot mirrors live.ts's Snapshot.
type agentSnapshot struct {
	At        time.Time    `json:"at"`
	Main      agentConv    `json:"main"`
	Lookbacks []agentChild `json:"lookbacks"`
}

type agentConv struct {
	ID       string      `json:"id"`
	Busy     bool        `json:"busy"`
	Handling string      `json:"handling"`
	Thinking string      `json:"thinking"`
	Text     string      `json:"text"`
	Retry    string      `json:"retry"`
	Script   string      `json:"script"`
	Calls    []agentCall `json:"calls"`
	Output   string      `json:"output"`
	Queued   int         `json:"queued"`
	Last     string      `json:"last"`
	CostUSD  float64     `json:"cost_usd"`
}

type agentCall struct {
	Name   string `json:"name"`
	Args   string `json:"args"`
	Status string `json:"status"`
	Error  string `json:"error"`
	Ms     int    `json:"ms"`
}

type agentChild struct {
	TaskID string     `json:"task_id"`
	Kind   string     `json:"kind"`
	Phase  string     `json:"phase"`
	Child  *agentConv `json:"child"`
}

type agentData struct {
	pageData
	Snap    *agentSnapshot
	Problem string // why there's no snapshot
}

// agentRetry is how long the relay waits before reconnecting to the agent.
const agentRetry = 2 * time.Second

func (s *Server) agentView(r *http.Request) (any, error) {
	if _, err := s.owner(r.Context()); err != nil {
		return nil, err
	}
	d := agentData{pageData: s.base("Agent", "/agent/stream", "agent"), Problem: "Connecting to the agent…"}
	if s.AgentURL == "" {
		d.Problem = "No agent is configured (LIFESTREAM_AGENT_URL)."
	}
	return d, nil
}

// agentStream relays the agent's snapshots until the browser leaves,
// reconnecting when the agent goes away (a restart, a deploy).
func (s *Server) agentStream(w http.ResponseWriter, r *http.Request) {
	u, err := s.owner(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	ctx := r.Context()
	if s.Collector != nil {
		s.Collector.StreamOpened()
		defer s.Collector.StreamClosed()
	}
	sse := datastar.NewSSE(w, r, datastar.WithCompression())
	render := func(d agentData) error {
		var buf bytes.Buffer
		if err := s.pages["agent"].ExecuteTemplate(&buf, "region", d); err != nil {
			return fmt.Errorf("rendering agent: %w", err)
		}
		return sse.PatchElements(buf.String())
	}
	base := s.base("Agent", "/agent/stream", "agent")
	if s.AgentURL == "" {
		render(agentData{pageData: base, Problem: "No agent is configured (LIFESTREAM_AGENT_URL)."})
		return
	}
	for {
		err := s.relayAgent(ctx, u.ID, func(snap *agentSnapshot) error { return render(agentData{pageData: base, Snap: snap}) })
		if ctx.Err() != nil {
			return
		}
		s.Log.Debug("agent live feed", "error", err)
		if render(agentData{pageData: base, Problem: "The agent is unreachable; retrying…"}) != nil {
			return // client disconnected
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(agentRetry):
		}
	}
}

// relayAgent reads one connection's snapshots and hands each to emit. It
// returns when the connection ends or emit fails.
func (s *Server) relayAgent(ctx context.Context, owner string, emit func(*agentSnapshot) error) error {
	req, err := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(s.AgentURL, "/")+"/live?owner="+url.QueryEscape(owner), nil)
	if err != nil {
		return err
	}
	res, err := s.AgentClient.Do(req)
	if err != nil {
		return fmt.Errorf("connecting to the agent: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("agent /live: %s", res.Status)
	}
	sc := bufio.NewScanner(res.Body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		var snap agentSnapshot
		if err := json.Unmarshal([]byte(data), &snap); err != nil {
			return fmt.Errorf("agent snapshot: %w", err)
		}
		if err := emit(&snap); err != nil {
			return err
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("reading the agent's feed: %w", err)
	}
	return fmt.Errorf("the agent closed its feed")
}
