package ingest

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/brojonat/lifestream/internal/store"
)

// MaxEventsBody caps one POST /events request. Payloads may be large
// (traces, artifacts), so this is generous.
const MaxEventsBody = 32 << 20

// Input is one event as a client sends it. Owner comes from the token;
// clients can't set ids, hashes or parents (raw events have none).
type Input struct {
	Type           string          `json:"type"`
	Source         string          `json:"source,omitempty"`
	OccurredAt     time.Time       `json:"occurred_at,omitzero"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
	Payload        json.RawMessage `json:"payload,omitempty"`
}

// Result is what POST /events answers for each input, in order.
type Result struct {
	ID      string `json:"id"`
	Seq     int64  `json:"seq"`
	Created bool   `json:"created"`
}

// DefaultSource is used when an input names none.
const DefaultSource = "webhook"

// Events handles POST /events, authenticated with a user's bearer token.
// The body is one JSON object, a JSON array of them, or JSONL. Every input
// is validated before any is appended, so a bad batch appends nothing.
func Events(st *store.Store, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || token == "" {
			http.Error(w, "missing bearer token", http.StatusUnauthorized)
			return
		}
		ctx := r.Context()
		user, err := st.UserByToken(ctx, token)
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "unknown token", http.StatusUnauthorized)
			return
		}
		if err != nil {
			log.Error("looking up token", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		inputs, err := DecodeInputs(http.MaxBytesReader(w, r.Body, MaxEventsBody))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		results := make([]Result, 0, len(inputs))
		for _, in := range inputs {
			if in.Source == "" {
				in.Source = DefaultSource
			}
			e, created, err := st.Append(ctx, store.Event{
				Owner:          user.ID,
				Source:         in.Source,
				Type:           in.Type,
				OccurredAt:     in.OccurredAt,
				IdempotencyKey: in.IdempotencyKey,
				Payload:        in.Payload,
			})
			var inv *store.InvalidEventError
			if errors.As(err, &inv) {
				http.Error(w, err.Error(), http.StatusUnprocessableEntity)
				return
			}
			if err != nil {
				log.Error("appending pushed event", "owner", user.ID, "error", err)
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			results = append(results, Result{ID: e.ID, Seq: e.Seq, Created: created})
		}
		log.Info("events pushed", "owner", user.ID, "count", len(results))
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"events": results})
	}
}

// DecodeInputs reads one object, an array, or JSONL, and validates each
// input. An empty source is left for the caller to fill in.
func DecodeInputs(r io.Reader) ([]Input, error) {
	br := bufio.NewReader(r)
	first, err := firstNonSpace(br)
	if err != nil {
		return nil, fmt.Errorf("empty body")
	}
	var inputs []Input
	dec := json.NewDecoder(br)
	dec.DisallowUnknownFields()
	if first == '[' {
		if err := dec.Decode(&inputs); err != nil {
			return nil, fmt.Errorf("decoding event array: %w", err)
		}
	} else {
		for {
			var in Input
			err := dec.Decode(&in)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, fmt.Errorf("decoding event %d: %w", len(inputs)+1, err)
			}
			inputs = append(inputs, in)
		}
	}
	if len(inputs) == 0 {
		return nil, fmt.Errorf("no events in body")
	}
	for i, in := range inputs {
		if strings.TrimSpace(in.Type) == "" {
			return nil, fmt.Errorf("event %d: type is required", i+1)
		}
		if in.Source == store.SourceAgent {
			return nil, fmt.Errorf("event %d: source %q is reserved", i+1, store.SourceAgent)
		}
		if len(in.Payload) > 0 && !json.Valid(in.Payload) {
			return nil, fmt.Errorf("event %d: payload is not JSON", i+1)
		}
	}
	return inputs, nil
}

// firstNonSpace peeks at the first non-whitespace byte without consuming it.
func firstNonSpace(br *bufio.Reader) (byte, error) {
	for {
		b, err := br.ReadByte()
		if err != nil {
			return 0, err
		}
		if !bytes.ContainsRune([]byte(" \t\r\n"), rune(b)) {
			return b, br.UnreadByte()
		}
	}
}
