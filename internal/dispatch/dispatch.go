// Package dispatch hands raw events to the agent. It POSTs each
// undelivered event, in log order, to the agent's /submit endpoint and
// marks it delivered on a 2xx. The event id is the agent's requestId, so
// redelivering after a crash is safe; the agent drops what it has seen.
package dispatch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/brojonat/lifestream/internal/store"
)

// Submission is the body of POST /submit.
type Submission struct {
	Owner string      `json:"owner"`
	Event store.Event `json:"event"`
}

// Dispatcher owns the delivery loop. Construct it with New and run it in
// one goroutine; Run returns when ctx is done.
type Dispatcher struct {
	st         *store.Store
	submitURL  string
	client     *http.Client
	log        *slog.Logger
	minBackoff time.Duration
	maxBackoff time.Duration
	poll       time.Duration
}

// Options tunes retry timing; zero values get defaults.
type Options struct {
	MinBackoff time.Duration // first wait after a failure (default 1s)
	MaxBackoff time.Duration // cap on the doubling wait (default 1m)
	Poll       time.Duration // re-check even without a wake-up (default 30s)
}

// New builds a dispatcher that submits to agentURL + "/submit".
func New(st *store.Store, agentURL string, client *http.Client, log *slog.Logger, opt Options) *Dispatcher {
	d := &Dispatcher{
		st: st, submitURL: strings.TrimSuffix(agentURL, "/") + "/submit", client: client, log: log,
		minBackoff: opt.MinBackoff, maxBackoff: opt.MaxBackoff, poll: opt.Poll,
	}
	if d.minBackoff == 0 {
		d.minBackoff = time.Second
	}
	if d.maxBackoff == 0 {
		d.maxBackoff = time.Minute
	}
	if d.poll == 0 {
		d.poll = 30 * time.Second
	}
	return d
}

// Run delivers the backlog, then waits for appends. On a failure it backs
// off and retries the same event, so the agent sees events in log order.
func (d *Dispatcher) Run(ctx context.Context) {
	backoff := time.Duration(0)
	for {
		changed := d.st.Changed() // take it before reading, so no append is missed
		err := d.drain(ctx)
		wait := d.poll
		if err != nil {
			backoff = min(max(backoff*2, d.minBackoff), d.maxBackoff)
			wait = backoff
			d.log.Warn("dispatch failed; retrying", "error", err, "retry_in", wait)
			changed = nil // don't hammer a failing agent on every append
		} else {
			backoff = 0
		}
		select {
		case <-ctx.Done():
			return
		case <-changed:
		case <-time.After(wait):
		}
	}
}

// drain submits every undelivered event, oldest first, stopping at the
// first failure.
func (d *Dispatcher) drain(ctx context.Context) error {
	for {
		batch, err := d.st.Undelivered(ctx, 100)
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			return nil
		}
		for _, e := range batch {
			if err := d.submit(ctx, e); err != nil {
				return fmt.Errorf("submitting %s: %w", e.ID, err)
			}
			if err := d.st.MarkDelivered(ctx, e.ID); err != nil {
				return err
			}
			d.log.Debug("dispatched", "event", e.ID, "type", e.Type)
		}
	}
}

func (d *Dispatcher) submit(ctx context.Context, e store.Event) error {
	body, err := json.Marshal(Submission{Owner: e.Owner, Event: e})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", d.submitURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("agent answered %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	io.Copy(io.Discard, resp.Body)
	return nil
}
