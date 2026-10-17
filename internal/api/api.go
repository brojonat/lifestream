// Package api is the agent's side of the contract: reads of collections
// and events, and appends of derived events. It is served on its own
// listener that only the pod can reach; nothing here checks a token.
package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/brojonat/lifestream/internal/store"
)

// DerivedTypes are the event types the agent may append.
var DerivedTypes = map[string]bool{
	store.TypeCollectionStarted: true,
	store.TypeCollectionAmended: true,
	store.TypeCollectionStopped: true,
	store.TypeCollectionResumed: true,
	store.TypeLabelApplied:      true,
	store.TypeMetricObserved:    true,
	store.TypeRuleAdded:         true, // removing one is the owner's call, on the dashboard
	"review.flagged":            true,
	"redaction.created":         true,
}

// DefaultMaxPayload is how large a payload can be before list responses
// replace it with its size; the agent reads big ones with GET
// /api/events/{id}?offset=&limit=.
const DefaultMaxPayload = 4096

// NewHandler wires the routes.
func NewHandler(st *store.Store, log *slog.Logger) http.Handler {
	h := &handler{st: st, log: log}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /api/collections", h.collections)
	mux.HandleFunc("GET /api/collections/{id}/members", h.members)
	mux.HandleFunc("GET /api/rules", h.rules)
	mux.HandleFunc("GET /api/events", h.listEvents)
	mux.HandleFunc("GET /api/events/{id}", h.event)
	mux.HandleFunc("POST /api/events", h.appendEvent)
	return mux
}

type handler struct {
	st  *store.Store
	log *slog.Logger
}

// GET /api/collections?owner=&status=open
func (h *handler) collections(w http.ResponseWriter, r *http.Request) {
	owner := r.URL.Query().Get("owner")
	if owner == "" {
		writeError(w, http.StatusBadRequest, "owner is required")
		return
	}
	cs, err := h.st.Collections(r.Context(), owner, r.URL.Query().Get("status"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if cs == nil {
		cs = []store.Collection{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"collections": cs})
}

// GET /api/rules?owner=
func (h *handler) rules(w http.ResponseWriter, r *http.Request) {
	owner := r.URL.Query().Get("owner")
	if owner == "" {
		writeError(w, http.StatusBadRequest, "owner is required")
		return
	}
	rules, err := h.st.Rules(r.Context(), owner)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rules": rules})
}

// GET /api/collections/{id}/members
func (h *handler) members(w http.ResponseWriter, r *http.Request) {
	ms, err := h.st.Members(r.Context(), r.PathValue("id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if ms == nil {
		ms = []store.Member{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"members": ms})
}

// Summary is an event in a list: the envelope, the payload's size, and the
// payload itself only when it's small.
type Summary struct {
	store.Event
	PayloadSize int `json:"payload_size"`
}

func summarize(e store.Event, maxPayload int) Summary {
	s := Summary{Event: e, PayloadSize: len(e.Payload)}
	if len(e.Payload) > maxPayload {
		s.Payload = json.RawMessage("null")
	}
	return s
}

// GET /api/events?owner=&since=RFC3339&after=SEQ&limit=N&max_payload=BYTES
func (h *handler) listEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	owner := q.Get("owner")
	if owner == "" {
		writeError(w, http.StatusBadRequest, "owner is required")
		return
	}
	var since time.Time
	if v := q.Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "since must be RFC 3339")
			return
		}
		since = t
	}
	after, err1 := intParam(q.Get("after"), 0)
	limit, err2 := intParam(q.Get("limit"), 100)
	maxPayload, err3 := intParam(q.Get("max_payload"), DefaultMaxPayload)
	if err := errors.Join(err1, err2, err3); err != nil || limit < 1 || limit > 1000 {
		writeError(w, http.StatusBadRequest, "after, limit (1-1000) and max_payload must be integers")
		return
	}
	evs, err := h.st.EventsSince(r.Context(), owner, since, int64(after), limit)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := make([]Summary, 0, len(evs))
	for _, e := range evs {
		out = append(out, summarize(e, maxPayload))
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": out})
}

// Slice is a byte range of a large payload. Data is the raw text of that
// range, which on its own is usually not valid JSON.
type Slice struct {
	store.Event
	PayloadSize int    `json:"payload_size"`
	Offset      int    `json:"offset"`
	Data        string `json:"data"`
}

// GET /api/events/{id}, or with ?offset=&limit= a byte range of the payload.
func (h *handler) event(w http.ResponseWriter, r *http.Request) {
	e, err := h.st.Event(r.Context(), r.PathValue("id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	q := r.URL.Query()
	if !q.Has("offset") && !q.Has("limit") {
		writeJSON(w, http.StatusOK, e)
		return
	}
	offset, err1 := intParam(q.Get("offset"), 0)
	limit, err2 := intParam(q.Get("limit"), 16<<10)
	if err := errors.Join(err1, err2); err != nil || offset < 0 || limit < 1 {
		writeError(w, http.StatusBadRequest, "offset and limit must be non-negative integers")
		return
	}
	size := len(e.Payload)
	start, end := min(offset, size), min(offset+limit, size)
	data := string(e.Payload[start:end])
	e.Payload = json.RawMessage("null")
	writeJSON(w, http.StatusOK, Slice{Event: e, PayloadSize: size, Offset: start, Data: data})
}

// AppendRequest is a derived event from the agent. The Idempotency-Key
// header (the Pi Durable tool-call id) makes retries safe.
type AppendRequest struct {
	Owner      string          `json:"owner"`
	Type       string          `json:"type"`
	ParentID   string          `json:"parent_id,omitempty"`
	OccurredAt time.Time       `json:"occurred_at,omitzero"`
	Payload    json.RawMessage `json:"payload"`
}

// POST /api/events. 201 with the event when appended, 200 with the stored
// one when the key was already used, 422 when it contradicts the
// projections.
func (h *handler) appendEvent(w http.ResponseWriter, r *http.Request) {
	var req AppendRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "decoding body: "+err.Error())
		return
	}
	if req.Owner == "" {
		writeError(w, http.StatusBadRequest, "owner is required")
		return
	}
	if !DerivedTypes[req.Type] {
		writeError(w, http.StatusBadRequest, "type "+strconv.Quote(req.Type)+" can't be appended by the agent")
		return
	}
	e, created, err := h.st.Append(r.Context(), store.Event{
		Owner:          req.Owner,
		Source:         store.SourceAgent,
		Type:           req.Type,
		ParentID:       req.ParentID,
		OccurredAt:     req.OccurredAt,
		IdempotencyKey: r.Header.Get("Idempotency-Key"),
		Payload:        req.Payload,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	status := http.StatusCreated
	if !created {
		status = http.StatusOK
	}
	writeJSON(w, status, e)
}

func (h *handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	var inv *store.InvalidEventError
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case errors.As(err, &inv):
		writeError(w, http.StatusUnprocessableEntity, inv.Msg)
	default:
		h.log.Error("api request failed", "method", r.Method, "path", r.URL.Path, "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func intParam(s string, def int) (int, error) {
	if s == "" {
		return def, nil
	}
	return strconv.Atoi(s)
}
