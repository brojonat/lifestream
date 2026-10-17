// Package capture tails a harness's transcript and sends each new line to
// the service as an llm.message event. A hook runs it at the end of every
// turn; it reads from the offset saved for the session, sends the complete
// lines after it, and saves the new offset only once the service has them.
// The transcript is the offline buffer: if a send fails, the next turn
// sends everything since the last success, and the service drops repeats
// because each event is keyed by (session_id, line number).
package capture

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Harness knows one harness's hook input and transcript lines.
type Harness struct {
	Name string
	// Hook reads the hook's stdin.
	Hook func(stdin []byte) (Hook, error)
	// Line inspects one transcript line. ok == false skips it (bookkeeping
	// the harness writes for itself); its line number is still used up.
	Line func(line []byte) (l Line, ok bool)
}

// Hook is what every harness's hook input boils down to.
type Hook struct {
	SessionID      string
	TranscriptPath string
	Cwd            string
}

// Line is what a parser can tell about one message.
type Line struct {
	Model      string
	OccurredAt time.Time
}

// Config is the environment: LIFESTREAM_URL, LIFESTREAM_TOKEN,
// LIFESTREAM_CAPTURE, LIFESTREAM_DENY, plus where offsets live.
type Config struct {
	URL      string
	Token    string
	On       bool     // LIFESTREAM_CAPTURE=on
	Deny     []string // path globs; a match on cwd or any parent skips capture
	StateDir string   // one offset file per session under here
	Client   *http.Client
	// MaxBatch caps one POST, in bytes (default 4 MB). A line bigger than
	// MaxLine (default 16 MB) is skipped: it would never fit.
	MaxBatch, MaxLine int
}

// Result says what one run did. Skipped is why nothing was sent, if so.
type Result struct {
	Skipped string `json:"skipped,omitempty"`
	Sent    int    `json:"sent"`
	Offset  int64  `json:"offset"`
}

// TransportError means the service couldn't be reached or failed (5xx).
// The offset didn't move, so the next run retries; hooks treat it as
// expected (an offline laptop), not as a failure.
type TransportError struct{ Err error }

func (e *TransportError) Error() string { return "sending to lifestream: " + e.Err.Error() }
func (e *TransportError) Unwrap() error { return e.Err }

// state is the saved position in one session's transcript.
type state struct {
	Offset int64 `json:"offset"` // byte offset of the next unread line
	Line   int64 `json:"line"`   // its line number, which is its sequence
}

// Run captures whatever the transcript gained since the last run.
func Run(ctx context.Context, cfg Config, h Harness, stdin []byte) (Result, error) {
	if !cfg.On {
		return Result{Skipped: "LIFESTREAM_CAPTURE is not on"}, nil
	}
	hook, err := h.Hook(stdin)
	if err != nil {
		return Result{}, fmt.Errorf("reading %s hook input: %w", h.Name, err)
	}
	if hook.SessionID == "" || hook.TranscriptPath == "" {
		return Result{}, fmt.Errorf("%s hook input has no session id or transcript path", h.Name)
	}
	if pattern := Denied(cfg.Deny, hook.Cwd); pattern != "" {
		return Result{Skipped: "cwd matches LIFESTREAM_DENY " + pattern}, nil
	}
	if cfg.URL == "" || cfg.Token == "" {
		return Result{}, errors.New("LIFESTREAM_URL and LIFESTREAM_TOKEN must be set when LIFESTREAM_CAPTURE=on")
	}
	if cfg.MaxBatch == 0 {
		cfg.MaxBatch = 4 << 20
	}
	if cfg.MaxLine == 0 {
		cfg.MaxLine = 16 << 20
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: 30 * time.Second}
	}

	statePath := filepath.Join(cfg.StateDir, h.Name, safeName(hook.SessionID)+".json")
	st, err := loadState(statePath)
	if err != nil {
		return Result{}, err
	}
	f, err := os.Open(hook.TranscriptPath)
	if err != nil {
		return Result{}, fmt.Errorf("opening transcript: %w", err)
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil {
		return Result{}, fmt.Errorf("reading transcript: %w", err)
	} else if info.Size() < st.Offset {
		st = state{} // rewritten, not appended: start over; the service drops repeats
	}
	if _, err := f.Seek(st.Offset, io.SeekStart); err != nil {
		return Result{}, fmt.Errorf("seeking transcript: %w", err)
	}

	src := Source{Harness: h.Name, Cwd: hook.Cwd, Git: gitInfo(ctx, hook.Cwd)}
	res := Result{Offset: st.Offset}
	var batch bytes.Buffer
	pending := 0
	// flush sends the queued lines, then saves pos (where reading stopped)
	// as the offset. Skipped lines move it too, so it's saved even when
	// nothing was queued.
	flush := func(pos state) error {
		if pending > 0 {
			if err := post(ctx, cfg, batch.Bytes()); err != nil {
				return err
			}
		}
		if pos != st {
			if err := saveState(statePath, pos); err != nil {
				return err
			}
		}
		res.Sent += pending
		res.Offset, st, pending = pos.Offset, pos, 0
		batch.Reset()
		return nil
	}
	r := bufio.NewReaderSize(f, 64<<10)
	cur := st // position after every line examined so far
	for {
		raw, err := r.ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			break // a partial last line waits for the next run
		}
		if err != nil {
			return res, fmt.Errorf("reading transcript: %w", err)
		}
		seq, after := cur.Line, state{Offset: cur.Offset + int64(len(raw)), Line: cur.Line + 1}
		line := bytes.TrimRight(raw, "\r\n")
		info, ok := h.Line(line)
		if !ok || len(line) > cfg.MaxLine || !json.Valid(line) {
			cur = after
			continue
		}
		ev, err := event(hook.SessionID, seq, src, info, line)
		if err != nil {
			return res, err
		}
		if pending > 0 && batch.Len()+len(ev) > cfg.MaxBatch {
			if err := flush(cur); err != nil {
				return res, err
			}
		}
		batch.Write(ev)
		pending++
		cur = after
	}
	return res, flush(cur)
}

// Source is where a message came from, sent beside it in the payload.
type Source struct {
	Harness string `json:"harness"`
	Cwd     string `json:"cwd,omitempty"`
	Git     *Git   `json:"git,omitempty"`
}

// Git describes the repo cwd is in, if any.
type Git struct {
	Remote string `json:"remote,omitempty"`
	Branch string `json:"branch,omitempty"`
	Commit string `json:"commit,omitempty"`
}

// event renders one JSONL line of the POST body.
func event(sessionID string, seq int64, src Source, info Line, line []byte) ([]byte, error) {
	payload := struct {
		SessionID string          `json:"session_id"`
		Sequence  int64           `json:"sequence"`
		Harness   string          `json:"harness"`
		Model     string          `json:"model,omitempty"`
		Cwd       string          `json:"cwd,omitempty"`
		Git       *Git            `json:"git,omitempty"`
		Message   json.RawMessage `json:"message"`
	}{sessionID, seq, src.Harness, info.Model, src.Cwd, src.Git, line}
	in := struct {
		Type       string    `json:"type"`
		Source     string    `json:"source"`
		OccurredAt time.Time `json:"occurred_at,omitzero"`
		Payload    any       `json:"payload"`
	}{"llm.message", src.Harness, info.OccurredAt, payload}
	b, err := json.Marshal(in)
	if err != nil {
		return nil, fmt.Errorf("encoding line %d: %w", seq, err)
	}
	return append(b, '\n'), nil
}

func post(ctx context.Context, cfg Config, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimSuffix(cfg.URL, "/")+"/events", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	req.Header.Set("Content-Type", "application/x-ndjson")
	resp, err := cfg.Client.Do(req)
	if err != nil {
		return &TransportError{err}
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	switch {
	case resp.StatusCode == http.StatusOK:
		return nil
	case resp.StatusCode >= 500:
		return &TransportError{fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(msg)))}
	default:
		return fmt.Errorf("sending to lifestream: %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
}

// Denied returns the first pattern matching cwd or one of its parents, or
// "". Patterns are filepath.Match globs; a leading ~ is the home directory.
func Denied(patterns []string, cwd string) string {
	if cwd == "" {
		return ""
	}
	home, _ := os.UserHomeDir()
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		pat := p
		if home != "" && (pat == "~" || strings.HasPrefix(pat, "~/")) {
			pat = home + pat[1:]
		}
		pat = filepath.Clean(pat)
		for dir := filepath.Clean(cwd); ; dir = filepath.Dir(dir) {
			if ok, _ := filepath.Match(pat, dir); ok {
				return p
			}
			if dir == filepath.Dir(dir) {
				break
			}
		}
	}
	return ""
}

// gitInfo asks git about cwd; any failure leaves the field empty.
func gitInfo(ctx context.Context, cwd string) *Git {
	if cwd == "" {
		return nil
	}
	run := func(args ...string) string {
		ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "git", append([]string{"-C", cwd}, args...)...).Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	g := Git{Commit: run("rev-parse", "HEAD")}
	if g.Commit == "" {
		return nil
	}
	g.Branch = run("rev-parse", "--abbrev-ref", "HEAD")
	g.Remote = stripCredentials(run("remote", "get-url", "origin"))
	return &g
}

// stripCredentials drops user:token@ from an https remote, which some
// people keep in their git config.
func stripCredentials(remote string) string {
	u, err := url.Parse(remote)
	if err != nil || u.User == nil || u.Scheme == "" {
		return remote
	}
	u.User = nil
	return u.String()
}

func safeName(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == 0 || r == ':' {
			return '_'
		}
		return r
	}, s)
}

func loadState(path string) (state, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return state{}, nil
	}
	if err != nil {
		return state{}, fmt.Errorf("reading capture offset: %w", err)
	}
	var st state
	if err := json.Unmarshal(b, &st); err != nil {
		return state{}, nil // a corrupt offset file means start over; repeats are dropped
	}
	return st, nil
}

// saveState writes atomically, so a kill never leaves half a file.
func saveState(path string, st state) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("saving capture offset: %w", err)
	}
	b, _ := json.Marshal(st)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("saving capture offset: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("saving capture offset: %w", err)
	}
	return nil
}
