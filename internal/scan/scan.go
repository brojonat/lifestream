// Package scan is the deterministic secret scanner, the Go twin of
// capture/strands' scan.py: the same patterns, the same [REDACTED:<kind>]
// masks, pinned by the same corpus (testdata/secrets.json). Capture runs it
// before anything leaves the machine. Patterns favor catching too much
// over too little.
package scan

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
)

// Finding counts the matches of one kind.
type Finding struct {
	Kind  string `json:"kind"`
	Count int    `json:"count"`
}

type pattern struct {
	kind  string
	re    *regexp.Regexp
	group int // the capture group holding the secret; 0 masks the whole match
	keep  int // a capture group before the secret to keep (RE2 has no lookbehind)
}

// Order matters: multi-line and prefixed tokens first, so later generic
// rules see their masks. Keep in step with scan.py.
var patterns = []pattern{
	{kind: "private_key", re: regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`)},
	{kind: "aws_access_key", re: regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{kind: "github_token", re: regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{36,}|github_pat_[A-Za-z0-9_]{60,})`)},
	{kind: "anthropic_key", re: regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{20,}`)},
	{kind: "openai_key", re: regexp.MustCompile(`\bsk-(?:proj-|svcacct-|admin-)?[A-Za-z0-9_-]{20,}`)},
	{kind: "stripe_key", re: regexp.MustCompile(`\b(?:sk|rk|pk)_(?:live|test)_[A-Za-z0-9]{16,}`)},
	{kind: "slack_token", re: regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`)},
	{kind: "google_api_key", re: regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}`)},
	{kind: "twilio_sid", re: regexp.MustCompile(`\b(?:AC|SK)[0-9a-f]{32}\b`)},
	{kind: "jwt", re: regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{10,}`)},
	{kind: "bearer", re: regexp.MustCompile(`\b[Bb]earer\s+([A-Za-z0-9\-._~+/]{20,}=*)`), group: 1},
	{kind: "connection_password", re: regexp.MustCompile(`\b[a-z][a-z0-9+.-]*://[^\s:/@]+:([^\s@/]+)@`), group: 1},
	{kind: "assigned_secret", re: regexp.MustCompile(`(?i)\b(?:[A-Za-z0-9]+[_-])?(?:api[_-]?key|secret|token|passw(?:or)?d|pwd)["']?\s*[:=]\s*["']?([^\s"',;]{8,})`), group: 1},
	{kind: "email", re: regexp.MustCompile(`\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b`)},
	// (?<![\w+])\+... in the TypeScript: keep the character before the +.
	{kind: "phone", re: regexp.MustCompile(`(^|[^\w+])(\+[1-9]\d{7,14})\b`), group: 2, keep: 1},
}

func mask(kind string) string { return "[REDACTED:" + kind + "]" }

// Text redacts one string.
func Text(s string) (string, []Finding) {
	counts := map[string]int{}
	out := s
	for _, p := range patterns {
		out = p.re.ReplaceAllStringFunc(out, func(match string) string {
			if strings.Contains(match, "[REDACTED:") {
				return match // never match across an earlier mask (a token inside a URL, say)
			}
			if p.group == 0 {
				counts[p.kind]++
				return mask(p.kind)
			}
			sub := p.re.FindStringSubmatch(match)
			secret := sub[p.group]
			if secret == "" {
				return match
			}
			counts[p.kind]++
			if p.keep > 0 {
				return sub[p.keep] + mask(p.kind) + match[len(sub[p.keep])+len(secret):]
			}
			return strings.Replace(match, secret, mask(p.kind), 1)
		})
	}
	return out, findings(counts)
}

// JSON redacts every string value in a JSON document; keys, key order,
// numbers and structure are kept. The output is compact.
func JSON(doc []byte) ([]byte, []Finding, error) {
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.UseNumber()
	counts := map[string]int{}
	var out bytes.Buffer
	// One frame per open container: whether it's an object, whether the next
	// token is a key, and whether it needs a comma before the next item.
	type frame struct{ object, key, comma bool }
	var stack []frame
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("scanning JSON: %w", err)
		}
		isKey := false
		if n := len(stack); n > 0 {
			top := &stack[n-1]
			closing := tok == json.Delim('}') || tok == json.Delim(']')
			if !closing {
				if top.object && !top.key {
					out.WriteByte(':')
				} else if top.comma {
					out.WriteByte(',')
				}
				isKey = top.object && top.key
				if top.object {
					top.key = !top.key
				}
				top.comma = true
			}
		}
		switch v := tok.(type) {
		case json.Delim:
			out.WriteRune(rune(v))
			switch v {
			case '{':
				stack = append(stack, frame{object: true, key: true})
			case '[':
				stack = append(stack, frame{})
			default:
				stack = stack[:len(stack)-1]
			}
		case string:
			if !isKey {
				var fs []Finding
				v, fs = Text(v)
				for _, f := range fs {
					counts[f.Kind] += f.Count
				}
			}
			b, _ := json.Marshal(v)
			out.Write(b)
		case json.Number:
			out.WriteString(v.String())
		case bool:
			fmt.Fprint(&out, v)
		case nil:
			out.WriteString("null")
		}
	}
	if len(stack) != 0 {
		return nil, nil, errors.New("scanning JSON: unexpected end of input")
	}
	return out.Bytes(), findings(counts), nil
}

func findings(counts map[string]int) []Finding {
	out := make([]Finding, 0, len(counts))
	for k, n := range counts {
		out = append(out, Finding{k, n})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Kind < out[j].Kind })
	return out
}
