package ingest

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brojonat/lifestream/internal/store"
)

// Vectors from twilio-python's tests/unit/test_request_validator.py.
func TestTwilioSignatureVectors(t *testing.T) {
	const token = "12345"
	uri := "https://mycompany.com/myapp.php?foo=1&bar=2"
	params := url.Values{
		"CallSid": {"CA1234567890ABCDE"},
		"Digits":  {"1234"},
		"From":    {"+14158675309"},
		"To":      {"+18005551212"},
		"Caller":  {"+14158675309"},
	}
	if got := TwilioSignature(token, uri, params); got != "RSOYDt4T1cUTdK1PDd93/VVr8B8=" {
		t.Fatalf("signature = %s", got)
	}
	dup := url.Values{"Sid": {"CA123"}, "SidAccount": {"AC123"}, "Digits": {"5678", "1234", "1234"}}
	if got := TwilioSignature(token, uri, dup); got != "IK+Dwps556ElfBT0I3Rgjkr1wJU=" {
		t.Fatalf("duplicate-param signature = %s", got)
	}
	withPort := strings.Replace(uri, ".com", ".com:1234", 1)
	if !ValidTwilioSignature(token, withPort, params, "RSOYDt4T1cUTdK1PDd93/VVr8B8=") {
		t.Fatal("https URL with a port should validate against the portless signature")
	}
	httpPort := strings.Replace(withPort, "https", "http", 1)
	if !ValidTwilioSignature(token, httpPort, params, "Zmvh+3yNM1Phv2jhDCwEM3q5ebU=") {
		t.Fatal("http URL with a port should validate")
	}
	if ValidTwilioSignature(token, uri, params, "RSOYDt4T1cUTdK1PDd93/VVr8B9=") {
		t.Fatal("wrong signature validated")
	}
	if ValidTwilioSignature("", uri, params, "RSOYDt4T1cUTdK1PDd93/VVr8B8=") {
		t.Fatal("empty auth token must never validate")
	}
}

type fixture struct {
	AuthToken string `json:"auth_token"`
	URL       string `json:"url"`
	Signature string `json:"signature"`
	Body      string `json:"body"`
}

func loadFixture(t *testing.T) fixture {
	t.Helper()
	b, err := os.ReadFile("testdata/twilio_sms.json")
	if err != nil {
		t.Fatal(err)
	}
	var f fixture
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

func newStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func postSMS(h http.Handler, body, sig string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/twilio/sms", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if sig != "" {
		req.Header.Set("X-Twilio-Signature", sig)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestTwilioWebhook(t *testing.T) {
	ctx := context.Background()
	f := loadFixture(t)
	st := newStore(t)
	owner, _, err := st.AddUser(ctx, "me", "+15555550100")
	if err != nil {
		t.Fatal(err)
	}
	h := Twilio(st, f.AuthToken, "https://lifestream.example.com/", quiet)

	rec := postSMS(h, f.Body, f.Signature)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "<Response></Response>") {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
	evs, err := st.Events(ctx, owner.ID, 0, 10)
	if err != nil || len(evs) != 1 {
		t.Fatalf("want 1 event, got %d (%v)", len(evs), err)
	}
	e := evs[0]
	var p map[string]string
	json.Unmarshal(e.Payload, &p)
	if e.Type != "message.received" || e.Source != "twilio.sms" || p["Body"] != "weight 175" || p["From"] != "+15555550100" {
		t.Fatalf("unexpected event %+v", e)
	}

	// Twilio retries: same MessageSid, no second event.
	postSMS(h, f.Body, f.Signature)
	if evs, _ := st.Events(ctx, owner.ID, 0, 10); len(evs) != 1 {
		t.Fatalf("retry appended a duplicate: %d events", len(evs))
	}
}

func TestTwilioRejectsBadSignature(t *testing.T) {
	ctx := context.Background()
	f := loadFixture(t)
	st := newStore(t)
	owner, _, _ := st.AddUser(ctx, "me", "+15555550100")
	h := Twilio(st, f.AuthToken, "https://lifestream.example.com", quiet)

	tampered := strings.Replace(f.Body, "weight+175", "weight+999", 1)
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"no signature":   postSMS(h, f.Body, ""),
		"tampered body":  postSMS(h, tampered, f.Signature),
		"wrong base URL": postSMS(Twilio(st, f.AuthToken, "https://evil.example.com", quiet), f.Body, f.Signature),
	} {
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: got %d, want 403", name, rec.Code)
		}
	}
	if evs, _ := st.Events(ctx, owner.ID, 0, 10); len(evs) != 0 {
		t.Fatalf("rejected requests appended %d events", len(evs))
	}
}

func TestTwilioDropsUnknownSender(t *testing.T) {
	f := loadFixture(t)
	st := newStore(t)
	st.AddUser(context.Background(), "someone else", "+15555550111")
	rec := postSMS(Twilio(st, f.AuthToken, "https://lifestream.example.com", quiet), f.Body, f.Signature)
	if rec.Code != 200 {
		t.Fatalf("unknown sender should get an ordinary 200, got %d", rec.Code)
	}
	if evs, _ := st.AllEvents(context.Background(), 0, 10); len(evs) != 0 {
		t.Fatalf("unknown sender appended %d events", len(evs))
	}
}

func postEvents(h http.Handler, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/events", strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestPushEvents(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	owner, token, _ := st.AddUser(ctx, "me", "+15555550100")
	h := Events(st, quiet)

	bodies := map[string]string{
		"object": `{"type":"note","source":"cli","payload":{"text":"one"}}`,
		"array":  `[{"type":"note","payload":{"text":"two"}},{"type":"note","payload":{"text":"three"}}]`,
		"jsonl":  "{\"type\":\"note\",\"payload\":{\"text\":\"four\"}}\n{\"type\":\"note\",\"occurred_at\":\"2026-10-01T08:00:00Z\"}\n",
	}
	want := map[string]int{"object": 1, "array": 2, "jsonl": 2}
	for name, body := range bodies {
		rec := postEvents(h, token, body)
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
		var resp struct{ Events []Result }
		json.Unmarshal(rec.Body.Bytes(), &resp)
		if len(resp.Events) != want[name] || !resp.Events[0].Created {
			t.Fatalf("%s: got %+v", name, resp)
		}
	}
	evs, _ := st.Events(ctx, owner.ID, 0, 10)
	if len(evs) != 5 {
		t.Fatalf("want 5 events, got %d", len(evs))
	}
	if evs[0].Source != "cli" || evs[1].Source != DefaultSource {
		t.Fatalf("sources: %s, %s", evs[0].Source, evs[1].Source)
	}
	if got := evs[4].OccurredAt.Format("2006-01-02"); got != "2026-10-01" {
		t.Fatalf("occurred_at not kept: %s", got)
	}
}

func TestPushEventsRejects(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	owner, token, _ := st.AddUser(ctx, "me", "+15555550100")
	h := Events(st, quiet)

	if rec := postEvents(h, "", `{"type":"note"}`); rec.Code != 401 {
		t.Errorf("no token: %d", rec.Code)
	}
	if rec := postEvents(h, "nope", `{"type":"note"}`); rec.Code != 401 {
		t.Errorf("bad token: %d", rec.Code)
	}
	for name, body := range map[string]string{
		"empty":          ``,
		"missing type":   `{"payload":{}}`,
		"agent source":   `{"type":"label.applied","source":"agent"}`,
		"parent smuggle": `{"type":"note","parent_id":"x"}`,
		"bad in batch":   `[{"type":"note"},{"payload":{}}]`,
	} {
		if rec := postEvents(h, token, body); rec.Code != 400 {
			t.Errorf("%s: got %d, want 400", name, rec.Code)
		}
	}
	if evs, _ := st.Events(ctx, owner.ID, 0, 10); len(evs) != 0 {
		t.Fatalf("rejected batches appended %d events", len(evs))
	}
}
