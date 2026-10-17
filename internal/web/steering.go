package web

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/brojonat/lifestream/internal/store"
)

// ---- rule log with undo ------------------------------------------------

type logRow struct {
	ID         string
	At         time.Time
	Collection string
	CollID     string
	What       string // started, include, exclude, removed rule, stopped, resumed
	Text       string
	By         string // agent or you
	Undo       string // button label; empty when there's nothing to undo
}

type logData struct {
	pageData
	Rows []logRow
}

const logSize = 100

func (s *Server) logView(r *http.Request) (any, error) {
	ctx := r.Context()
	u, err := s.owner(ctx)
	if err != nil {
		return nil, err
	}
	evs, err := s.Events.SteeringLog(ctx, u.ID, logSize)
	if err != nil {
		return nil, err
	}
	cs, err := s.Events.Collections(ctx, u.ID, "")
	if err != nil {
		return nil, err
	}
	names := map[string]store.Collection{}
	for _, c := range cs {
		names[c.ID] = c
	}
	data := logData{pageData: s.base("Rule log", "/log/stream", "log")}
	statusSeen := map[string]bool{} // only the newest start/stop/resume per collection can be undone
	for _, e := range evs {
		row := logRow{ID: e.ID, At: e.OccurredAt, By: "agent"}
		if e.Source != store.SourceAgent {
			row.By = "you"
		}
		row.What, row.Text, row.CollID = describeSteering(e)
		row.Collection = names[row.CollID].Name
		if e.Type != store.TypeCollectionAmended {
			if statusSeen[row.CollID] {
				data.Rows = append(data.Rows, row)
				continue
			}
			statusSeen[row.CollID] = true
		}
		if inv, err := s.inverse(ctx, e, names); err != nil {
			return nil, err
		} else if inv != nil {
			row.Undo = inv.label
		}
		data.Rows = append(data.Rows, row)
	}
	return data, nil
}

func describeSteering(e store.Event) (what, text, collectionID string) {
	switch e.Type {
	case store.TypeCollectionStarted:
		var p store.CollectionStarted
		json.Unmarshal(e.Payload, &p)
		return "started", p.Include, e.ID
	case store.TypeCollectionAmended:
		var p store.CollectionAmended
		json.Unmarshal(e.Payload, &p)
		switch {
		case p.Include != "":
			return "include", p.Include, p.CollectionID
		case p.Exclude != "":
			return "exclude", p.Exclude, p.CollectionID
		default:
			return "removed rule", "", p.CollectionID
		}
	default:
		var p store.CollectionRef
		json.Unmarshal(e.Payload, &p)
		return strings.TrimPrefix(e.Type, "collection."), "", p.CollectionID
	}
}

// undo is the event that reverses a steering event.
type undo struct {
	label   string
	typ     string
	payload any
}

// inverse returns how to undo e given the current state, or nil when e is
// no longer in effect (already undone, or overtaken).
func (s *Server) inverse(ctx context.Context, e store.Event, colls map[string]store.Collection) (*undo, error) {
	what, _, cid := describeSteering(e)
	c, ok := colls[cid]
	if !ok {
		return nil, nil
	}
	ref := store.CollectionRef{CollectionID: cid}
	switch what {
	case "started", "resumed":
		if c.Status == "open" {
			return &undo{"undo (stop it)", store.TypeCollectionStopped, ref}, nil
		}
	case "stopped":
		if c.Status != "open" {
			return &undo{"undo (resume it)", store.TypeCollectionResumed, ref}, nil
		}
	case "include", "exclude":
		has, err := s.Events.HasRule(ctx, cid, e.ID)
		if err != nil || !has {
			return nil, err
		}
		return &undo{"undo (remove rule)", store.TypeCollectionAmended, store.CollectionAmended{CollectionID: cid, RemoveRule: e.ID}}, nil
	case "removed rule":
		var p store.CollectionAmended
		json.Unmarshal(e.Payload, &p)
		orig, err := s.Events.Event(ctx, p.RemoveRule)
		if err != nil {
			return nil, err
		}
		kind, text, _ := describeSteering(orig)
		if has, err := s.Events.HasRule(ctx, cid, orig.ID); err != nil || has {
			return nil, err
		}
		add := store.CollectionAmended{CollectionID: cid}
		if kind == "exclude" {
			add.Exclude = text
		} else {
			add.Include = text
		}
		return &undo{"undo (put rule back)", store.TypeCollectionAmended, add}, nil
	}
	return nil, nil
}

// undoSteering handles POST /log/{id}/undo.
func (s *Server) undoSteering(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u, err := s.owner(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	e, err := s.Events.Event(ctx, r.PathValue("id"))
	if err != nil || e.Owner != u.ID || !strings.HasPrefix(e.Type, "collection.") {
		http.Error(w, "no such steering event", http.StatusNotFound)
		return
	}
	cs, err := s.Events.Collections(ctx, u.ID, "")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	colls := map[string]store.Collection{}
	for _, c := range cs {
		colls[c.ID] = c
	}
	inv, err := s.inverse(ctx, e, colls)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if inv == nil {
		http.Error(w, "nothing to undo: that change is no longer in effect", http.StatusConflict)
		return
	}
	s.appendOwned(w, r, inv.typ, inv.payload)
}

// ---- review queue --------------------------------------------------------

type flagRow struct {
	ID       string
	At       time.Time
	Question string
	EventID  string
	Original string
}

func (s *Server) flags(ctx context.Context, owner string) ([]flagRow, error) {
	evs, err := s.Events.OpenFlags(ctx, owner)
	if err != nil {
		return nil, err
	}
	var out []flagRow
	for _, f := range evs {
		var p struct {
			Question string `json:"question"`
			Reason   string `json:"reason"`
		}
		json.Unmarshal(f.Payload, &p)
		row := flagRow{ID: f.ID, At: f.OccurredAt, Question: firstNonEmpty(p.Question, p.Reason, "needs review"), EventID: f.ParentID}
		if f.ParentID != "" {
			if orig, err := s.Events.Event(ctx, f.ParentID); err == nil {
				row.Original = eventText(orig)
			}
		}
		out = append(out, row)
	}
	return out, nil
}

// answerFlag handles POST /review/{id}/answer with field answer. The
// answer is an event the agent receives, with the question for context.
func (s *Server) answerFlag(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u, err := s.owner(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	answer, err := field(w, r, "answer")
	if err != nil || answer == "" {
		http.Error(w, "answer required", http.StatusBadRequest)
		return
	}
	flag, err := s.Events.Event(ctx, r.PathValue("id"))
	if err != nil || flag.Owner != u.ID || flag.Type != "review.flagged" {
		http.Error(w, "no such flag", http.StatusNotFound)
		return
	}
	var q struct {
		Question string `json:"question"`
	}
	json.Unmarshal(flag.Payload, &q)
	payload, _ := json.Marshal(map[string]string{"flag_id": flag.ID, "event_id": flag.ParentID, "question": q.Question, "answer": answer})
	if _, _, err := s.Events.Append(ctx, store.Event{
		Owner: u.ID, Source: SourceDashboard, Type: "review.answered", ParentID: flag.ID,
		IdempotencyKey: "answer:" + flag.ID, Payload: payload,
	}); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
