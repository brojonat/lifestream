package transcript

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

func lines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// summary is role followed by each part as kind[:name|text], compact enough
// to compare whole sessions in one line each.
func summary(m Message) string {
	out := []string{m.Role}
	for _, p := range m.Parts {
		s := p.Kind
		switch {
		case p.Name != "":
			s += ":" + p.Name
		case p.Text != "":
			s += ":" + p.Text
		}
		if p.Error {
			s += "!"
		}
		out = append(out, s)
	}
	return strings.Join(out, " | ")
}

func TestHarnessFixtures(t *testing.T) {
	for _, c := range []struct {
		harness, file string
		lines         []int
		want          []string
	}{
		{"claude-code", "../../testdata/claude-code/transcript.jsonl", []int{1, 2, 3, 6, 7}, []string{
			"user | text:upgrade postgres to 17",
			"assistant | text:Checking the cluster first. | tool_call:Bash",
			"tool | tool_result:Clusters are compatible",
			"assistant | text:Compatible. Running the upgrade.",
			"system | text:12s",
		}},
		{"pi", "../../testdata/pi/session.jsonl", []int{2, 3, 4, 6}, []string{
			"user | text:upgrade postgres to 17",
			"assistant | tool_call:bash",
			"tool | tool_result:bash",
			"assistant | text:Compatible. Running the upgrade.",
		}},
		{"codex", "../../testdata/codex/rollout.jsonl", []int{2, 4, 5, 6}, []string{
			"user | text:upgrade postgres to 17",
			"assistant | tool_call:shell",
			"tool | tool_result:Clusters are compatible",
			"assistant | text:Compatible. Running the upgrade.",
		}},
	} {
		all := lines(t, c.file)
		for i, n := range c.lines {
			if got := summary(Normalize(c.harness, json.RawMessage(all[n]))); got != c.want[i] {
				t.Errorf("%s line %d:\n got %s\nwant %s", c.harness, n, got, c.want[i])
			}
		}
	}
}

func TestToolCallsKeepTheirInputAndPairing(t *testing.T) {
	cc := lines(t, "../../testdata/claude-code/transcript.jsonl")
	call := Normalize("claude-code", json.RawMessage(cc[2])).Parts[1]
	result := Normalize("claude-code", json.RawMessage(cc[3])).Parts[0]
	if call.CallID == "" || call.CallID != result.CallID || string(call.Input) != `{"command":"pg_upgrade --check"}` {
		t.Fatalf("claude-code call %+v result %+v", call, result)
	}
	cx := lines(t, "../../testdata/codex/rollout.jsonl")
	ccall := Normalize("codex", json.RawMessage(cx[4])).Parts[0]
	if ccall.CallID != "call_1" || string(ccall.Input) != `{"command":["pg_upgrade","--check"]}` {
		t.Fatalf("codex call %+v (arguments are a JSON string; they should come out as JSON)", ccall)
	}
}

func TestStrands(t *testing.T) {
	for msg, want := range map[string]string{
		`{"role":"user","content":[{"text":"upgrade postgres"}]}`: "user | text:upgrade postgres",
		`{"role":"assistant","content":[{"reasoningContent":{"reasoningText":{"text":"check first"}}},{"toolUse":{"toolUseId":"t1","name":"shell","input":{"cmd":"ls"}}}]}`: "assistant | reasoning:check first | tool_call:shell",
		`{"role":"user","content":[{"toolResult":{"toolUseId":"t1","status":"error","content":[{"text":"nope"},{"json":{"a":1}}]}}]}`:                                       "tool | tool_result:nope\n{\"a\":1}!",
		`{"role":"assistant","content":[{"image":{"format":"png","source":{"bytes":"iVBORw=="}}},{"cachePoint":{}}]}`:                                                       "assistant | image | other:cachePoint",
	} {
		if got := summary(Normalize("strands", json.RawMessage(msg))); got != want {
			t.Errorf("%s:\n got %q\nwant %q", msg, got, want)
		}
	}
}

func TestUnknownHarnessAndJunk(t *testing.T) {
	if got := Normalize("emacs", json.RawMessage(`{}`)); !reflect.DeepEqual(got, Message{Role: RoleUnknown, Parts: []Part{}}) {
		t.Fatalf("unknown harness: %+v", got)
	}
	if got := Normalize("pi", json.RawMessage(`[1,2]`)); got.Role != RoleUnknown {
		t.Fatalf("junk: %+v", got)
	}
}
