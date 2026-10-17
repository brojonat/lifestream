package web

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/brojonat/lifestream/internal/store"
)

type topicsData struct {
	pageData
	Topics []store.Topic
}

func (s *Server) topicsView(r *http.Request) (any, error) {
	ctx := r.Context()
	u, err := s.owner(ctx)
	if err != nil {
		return nil, err
	}
	ts, err := s.Events.Topics(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	return topicsData{pageData: s.base("Topics", "/topics/stream", "topics"), Topics: ts}, nil
}

// createTopic handles POST /topics: name, kind (one_time|monthly), price
// in dollars.
func (s *Server) createTopic(w http.ResponseWriter, r *http.Request) {
	f, err := fields(w, r, "name", "kind", "price")
	if err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	cents, err := dollars(f["price"])
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.appendOwned(w, r, store.TypeTopicCreated, store.TopicCreated{Name: f["name"], Price: store.Price{Kind: f["kind"], AmountCents: cents}})
}

// setHistory handles POST /topics/{id}/history: a date (YYYY-MM-DD, UTC)
// new subscribers can read back to, or empty for "from when they
// subscribe".
func (s *Server) setHistory(w http.ResponseWriter, r *http.Request) {
	from, err := field(w, r, "from")
	if err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	a := store.TopicAmended{TopicID: r.PathValue("id")}
	if from == "" {
		a.ClearHistoryStart = true
	} else {
		t, err := time.Parse("2006-01-02", from)
		if err != nil {
			http.Error(w, "from must be a date, YYYY-MM-DD", http.StatusBadRequest)
			return
		}
		a.HistoryStart = &t
	}
	s.appendOwned(w, r, store.TypeTopicAmended, a)
}

// sendNotification handles POST /topics/{id}/send: text and the
// subscriber count the creator confirmed. A count that no longer matches
// is refused, so nobody pings a different audience than they agreed to.
// Text that is a JSON object or array is sent as JSON, anything else as a
// string.
func (s *Server) sendNotification(w http.ResponseWriter, r *http.Request) {
	f, err := fields(w, r, "text", "confirm")
	if err != nil || f["text"] == "" {
		http.Error(w, "text required", http.StatusBadRequest)
		return
	}
	t, err := s.Events.Topic(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if n, err := strconv.Atoi(f["confirm"]); err != nil || n != t.Subscribers {
		http.Error(w, fmt.Sprintf("the topic has %d subscribers now; review and send again", t.Subscribers), http.StatusConflict)
		return
	}
	payload, _ := json.Marshal(f["text"])
	if text := f["text"]; (strings.HasPrefix(text, "{") || strings.HasPrefix(text, "[")) && json.Valid([]byte(text)) {
		payload = []byte(text)
	}
	s.appendOwned(w, r, store.TypeNotificationSent, store.NotificationSent{TopicID: t.ID, Payload: payload})
}

// dollars reads "5", "5.00" or "" (free) as cents.
func dollars(s string) (int64, error) {
	if s = strings.TrimPrefix(strings.TrimSpace(s), "$"); s == "" {
		return 0, nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v < 0 || math.IsInf(v, 0) || math.IsNaN(v) {
		return 0, fmt.Errorf("price must be a dollar amount, got %q", s)
	}
	return int64(math.Round(v * 100)), nil
}
