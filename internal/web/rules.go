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

// addRule handles POST /rules with field text.
func (s *Server) addRule(w http.ResponseWriter, r *http.Request) {
	text, err := field(w, r, "text")
	if err != nil || text == "" {
		http.Error(w, "text required", http.StatusBadRequest)
		return
	}
	s.appendOwned(w, r, store.TypeRuleAdded, store.RuleAdded{Text: text})
}

// field reads one trimmed string field from a JSON body (Datastar's
// {payload: ...}, used where a form resets right after posting) or from a
// form.
func field(w http.ResponseWriter, r *http.Request, name string) (string, error) {
	f, err := fields(w, r, name)
	return f[name], err
}

// fields is field for several names from one body.
func fields(w http.ResponseWriter, r *http.Request, names ...string) (map[string]string, error) {
	out := map[string]string{}
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		for _, n := range names {
			out[n] = strings.TrimSpace(r.FormValue(n))
		}
		return out, nil
	}
	var body map[string]any
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
		return out, err
	}
	for _, n := range names {
		v, _ := body[n].(string)
		out[n] = strings.TrimSpace(v)
	}
	return out, nil
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
