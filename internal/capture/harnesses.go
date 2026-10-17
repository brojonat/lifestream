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
	Hook: jsonHook,
	Parser: stateless(func(line []byte) (Line, bool) {
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
	}),
}

// Pi tails pi's session file. pi has no command hooks; its extension
// (capture/pi) runs `lifestream capture pi` after every turn with the same
// {session_id, transcript_path, cwd} a Claude Code hook gets. Messages are
// the "message" entries (user, assistant, toolResult, ...); the header,
// model and thinking-level changes and compactions are bookkeeping.
var Pi = Harness{
	Name: "pi",
	Hook: jsonHook,
	Parser: stateless(func(line []byte) (Line, bool) {
		var l struct {
			Type      string    `json:"type"`
			Timestamp time.Time `json:"timestamp"`
			Message   struct {
				Model string `json:"model"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &l) != nil || l.Type != "message" {
			return Line{}, false
		}
		return Line{Model: l.Message.Model, OccurredAt: l.Timestamp}, true
	}),
}

func stateless(f func([]byte) (Line, bool)) func() func([]byte) (Line, bool) {
	return func() func([]byte) (Line, bool) { return f }
}

// jsonHook reads {session_id, transcript_path, cwd}.
func jsonHook(stdin []byte) (Hook, error) {
	var in struct {
		SessionID      string `json:"session_id"`
		TranscriptPath string `json:"transcript_path"`
		Cwd            string `json:"cwd"`
	}
	if err := json.Unmarshal(stdin, &in); err != nil {
		return Hook{}, err
	}
	return Hook{SessionID: in.SessionID, TranscriptPath: in.TranscriptPath, Cwd: in.Cwd}, nil
}

// Harnesses are the transcript tailers `lifestream capture` knows.
// codexHome is $CODEX_HOME (default ~/.codex).
func Harnesses(codexHome string) map[string]Harness {
	codex := Codex(codexHome)
	return map[string]Harness{ClaudeCode.Name: ClaudeCode, Pi.Name: Pi, codex.Name: codex}
}
