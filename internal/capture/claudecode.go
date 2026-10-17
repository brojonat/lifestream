package capture

import (
	"encoding/json"
	"time"
)

// ClaudeCode tails Claude Code's session transcript. Every hook gets
// session_id, transcript_path and cwd on stdin; the plugin runs capture on
// Stop (each turn) and SessionEnd. Messages are the user, assistant and
// system lines; the rest (titles, modes, file-history backups) is the
// harness's own bookkeeping and stays on the machine.
var ClaudeCode = Harness{
	Name: "claude-code",
	Hook: func(stdin []byte) (Hook, error) {
		var in struct {
			SessionID      string `json:"session_id"`
			TranscriptPath string `json:"transcript_path"`
			Cwd            string `json:"cwd"`
		}
		if err := json.Unmarshal(stdin, &in); err != nil {
			return Hook{}, err
		}
		return Hook{SessionID: in.SessionID, TranscriptPath: in.TranscriptPath, Cwd: in.Cwd}, nil
	},
	Line: func(line []byte) (Line, bool) {
		var l struct {
			Type      string    `json:"type"`
			Timestamp time.Time `json:"timestamp"`
			Message   struct {
				Model string `json:"model"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &l) != nil {
			return Line{}, false
		}
		switch l.Type {
		case "user", "assistant", "system":
			return Line{Model: l.Message.Model, OccurredAt: l.Timestamp}, true
		}
		return Line{}, false
	},
}

// Harnesses are the transcript tailers `lifestream capture` knows.
func Harnesses() map[string]Harness {
	return map[string]Harness{ClaudeCode.Name: ClaudeCode}
}
