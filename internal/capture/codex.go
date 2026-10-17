package capture

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Codex tails Codex's rollout files,
// <home>/sessions/YYYY/MM/DD/rollout-<time>-<thread id>.jsonl (plain
// JSONL, not compressed, as of 0.56). Its end-of-turn notify program gets
// {"type":"agent-turn-complete","thread-id",...} as its last argument, with
// no transcript path, so the hook finds the rollout by thread id and reads
// cwd from the rollout's session_meta when the input has none. Input that
// already names a transcript_path (a hooks.json hook) is used as is.
//
// Messages are the response_item lines: messages, tool calls and their
// output, reasoning. session_meta (which holds the system instructions),
// turn_context and the event_msg UI echoes stay on the machine. The model
// is named once per turn, in turn_context, so the parser carries it.
func Codex(home string) Harness {
	return Harness{
		Name: "codex",
		Hook: func(stdin []byte) (Hook, error) {
			var in struct {
				Type           string `json:"type"`
				ThreadID       string `json:"thread-id"`
				SessionID      string `json:"session_id"`
				TranscriptPath string `json:"transcript_path"`
				Cwd            string `json:"cwd"`
			}
			if err := json.Unmarshal(stdin, &in); err != nil {
				return Hook{}, err
			}
			h := Hook{SessionID: in.SessionID, TranscriptPath: in.TranscriptPath, Cwd: in.Cwd}
			if h.SessionID == "" {
				h.SessionID = in.ThreadID
			}
			if h.TranscriptPath == "" && h.SessionID != "" {
				matches, err := filepath.Glob(filepath.Join(home, "sessions", "*", "*", "*", "rollout-*-"+h.SessionID+".jsonl"))
				if err != nil {
					return Hook{}, err
				}
				if len(matches) == 0 {
					return Hook{}, fmt.Errorf("no rollout file for thread %s under %s", h.SessionID, home)
				}
				h.TranscriptPath = matches[len(matches)-1]
			}
			if h.Cwd == "" && h.TranscriptPath != "" {
				h.Cwd = rolloutCwd(h.TranscriptPath)
			}
			return h, nil
		},
		Parser: func() func([]byte) (Line, bool) {
			model := ""
			return func(line []byte) (Line, bool) {
				var l struct {
					Type      string    `json:"type"`
					Timestamp time.Time `json:"timestamp"`
					Payload   struct {
						Model string `json:"model"`
					} `json:"payload"`
				}
				if json.Unmarshal(line, &l) != nil {
					return Line{}, false
				}
				switch l.Type {
				case "turn_context":
					model = l.Payload.Model
				case "response_item":
					return Line{Model: model, OccurredAt: l.Timestamp}, true
				}
				return Line{}, false
			}
		},
	}
}

// rolloutCwd reads cwd from the rollout's first line (session_meta).
func rolloutCwd(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 64<<10)
	first, _ := r.ReadBytes('\n')
	var meta struct {
		Type    string `json:"type"`
		Payload struct {
			Cwd string `json:"cwd"`
		} `json:"payload"`
	}
	if json.Unmarshal(first, &meta) != nil || meta.Type != "session_meta" {
		return ""
	}
	return meta.Payload.Cwd
}
