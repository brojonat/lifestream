package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/brojonat/lifestream/internal/store"
)

type rulesData struct {
	pageData
	Rules []store.OwnerRule
}

func (s *Server) rulesView(r *http.Request) (any, error) {
	ctx := r.Context()
	u, err := s.owner(ctx)
	if err != nil {
		return nil, err
	}
	rules, err := s.Events.Rules(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	return rulesData{pageData: s.base("Rules", "/rules/stream", "rules"), Rules: rules}, nil
}

// addRule handles POST /rules: {"text": ...} as JSON (the page) or a form
// field (scripts).
func (s *Server) addRule(w http.ResponseWriter, r *http.Request) {
	text := r.FormValue("text")
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var body struct {
			Text string `json:"text"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		text = body.Text
	}
	text = strings.TrimSpace(text)
	if text == "" {
		http.Error(w, "text required", http.StatusBadRequest)
		return
	}
	s.appendOwned(w, r, store.TypeRuleAdded, store.RuleAdded{Text: text})
}

// removeRule handles POST /rules/{id}/remove.
func (s *Server) removeRule(w http.ResponseWriter, r *http.Request) {
	s.appendOwned(w, r, store.TypeRuleRemoved, store.RuleRemoved{RuleID: r.PathValue("id")})
}

// appendOwned appends a dashboard event for the owner and answers 204, or
// 422 when it contradicts current state.
func (s *Server) appendOwned(w http.ResponseWriter, r *http.Request, typ string, payload any) {
	ctx := r.Context()
	u, err := s.owner(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	b, _ := json.Marshal(payload)
	_, _, err = s.Events.Append(ctx, store.Event{Owner: u.ID, Source: SourceDashboard, Type: typ, Payload: b})
	var inv *store.InvalidEventError
	if errors.As(err, &inv) {
		http.Error(w, inv.Msg, http.StatusUnprocessableEntity)
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
