package scan

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

type corpus struct {
	Positives []struct{ Kind, Secret, Text string }
	Negatives []struct{ Name, Text string }
}

func load(t *testing.T) corpus {
	t.Helper()
	b, err := os.ReadFile("../../testdata/secrets.json")
	if err != nil {
		t.Fatal(err)
	}
	var c corpus
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

func has(fs []Finding, kind string) bool {
	for _, f := range fs {
		if f.Kind == kind {
			return true
		}
	}
	return false
}

func TestCorpusPositives(t *testing.T) {
	c := load(t)
	if len(c.Positives) == 0 {
		t.Fatal("empty corpus")
	}
	for _, p := range c.Positives {
		got, fs := Text(p.Text)
		if strings.Contains(got, p.Secret) {
			t.Errorf("%s: secret survived in %q", p.Kind, got)
		}
		if !has(fs, p.Kind) {
			t.Errorf("%s: reported %v", p.Kind, fs)
		}
	}
}

func TestCorpusNegatives(t *testing.T) {
	for _, n := range load(t).Negatives {
		if got, fs := Text(n.Text); got != n.Text || len(fs) != 0 {
			t.Errorf("%s changed: %q %v", n.Name, got, fs)
		}
	}
}

func TestContextIsKept(t *testing.T) {
	for in, want := range map[string]string{
		"DATABASE_URL=postgres://app:hunter2hunter2@db:5432/x":                     "DATABASE_URL=postgres://app:[REDACTED:connection_password]@db:5432/x",
		"Authorization: Bearer abcdefghijklmnopqrstuvwxyz012345":                   "Authorization: Bearer [REDACTED:bearer]",
		"git clone https://ghp_" + strings.Repeat("a", 36) + "@github.com/me/repo": "git clone https://[REDACTED:github_token]@github.com/me/repo",
		"?key=AIzaSy" + strings.Repeat("b", 33) + "&q=1":                           "?key=[REDACTED:google_api_key]&q=1",
		"call +15555550100 or (+15555550101)":                                      "call [REDACTED:phone] or ([REDACTED:phone])",
		"build 1+15555550100":                                                      "build 1+15555550100",
	} {
		if got, _ := Text(in); got != want {
			t.Errorf("%q:\n got %q\nwant %q", in, got, want)
		}
	}
}

func TestJSONKeepsStructureKeysAndOrder(t *testing.T) {
	in := `{"Body": "key AKIAIOSFODNN7EXAMPLE", "From": "+15555550100", "n": 3.10, "ok": true, "z": null,
		"tags": ["alice@example.com", {"token": "x"}], "AKIAIOSFODNN7EXAMPLE": [], "e": {}}`
	out, fs, err := JSON([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"Body":"key [REDACTED:aws_access_key]","From":"[REDACTED:phone]","n":3.10,"ok":true,"z":null,` +
		`"tags":["[REDACTED:email]",{"token":"x"}],"AKIAIOSFODNN7EXAMPLE":[],"e":{}}`
	if string(out) != want {
		t.Fatalf("got  %s\nwant %s", out, want)
	}
	if len(fs) != 3 || fs[0].Kind != "aws_access_key" || fs[1].Kind != "email" || fs[2].Kind != "phone" {
		t.Fatalf("findings %v", fs)
	}
	if _, _, err := JSON([]byte(`{"a":`)); err == nil {
		t.Fatal("truncated JSON should fail")
	}
}

func TestJSONPrivateKeyAcrossEscapedNewlines(t *testing.T) {
	key := "-----BEGIN RSA PRIVATE KEY-----\nMIIEow\n-----END RSA PRIVATE KEY-----"
	doc, _ := json.Marshal(map[string]string{"text": "here: " + key})
	out, _, err := JSON(doc)
	if err != nil || string(out) != `{"text":"here: [REDACTED:private_key]"}` {
		t.Fatalf("%s %v", out, err)
	}
}
