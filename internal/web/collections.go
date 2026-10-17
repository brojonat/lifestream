package web

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/brojonat/lifestream/internal/store"
)

// SourceDashboard marks events the owner made on the dashboard. They are
// dispatched to the agent like any other raw event, so it learns from them.
const SourceDashboard = "dashboard"

// ---- read side --------------------------------------------------------

type collectionSummary struct {
	store.Collection
	Members int
}

type collectionsData struct {
	pageData
	Collections []collectionSummary
}

func (s *Server) collectionsView(r *http.Request) (any, error) {
	ctx := r.Context()
	u, err := s.owner(ctx)
	if err != nil {
		return nil, err
	}
	cs, err := s.Events.Collections(ctx, u.ID, "")
	if err != nil {
		return nil, err
	}
	counts, err := s.Events.MemberCounts(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	data := collectionsData{pageData: s.base("Collections", "/collections/stream", "collections")}
	for i := len(cs) - 1; i >= 0; i-- { // newest first
		data.Collections = append(data.Collections, collectionSummary{cs[i], counts[cs[i].ID]})
	}
	return data, nil
}

// change is one step in a collection's definition history.
type change struct {
	Version int
	At      time.Time
	What    string // "started", "include", "exclude", "stopped", "resumed"
	Text    string
}

type memberRow struct {
	EventID string
	At      time.Time
	Text    string
	Version int
	Reason  string
}

type collectionData struct {
	pageData
	C       store.Collection
	History []change
	Members []memberRow
	Total   int
}

const membersShown = 100

func (s *Server) collectionView(r *http.Request) (any, error) {
	ctx := r.Context()
	u, err := s.owner(ctx)
	if err != nil {
		return nil, err
	}
	id := r.PathValue("id")
	c, err := s.Events.Collection(ctx, id)
	if err != nil {
		return nil, err
	}
	if c.Owner != u.ID {
		return nil, store.ErrNotFound
	}
	hist, err := s.Events.CollectionHistory(ctx, id)
	if err != nil {
		return nil, err
	}
	members, err := s.Events.MemberEvents(ctx, id, membersShown)
	if err != nil {
		return nil, err
	}
	counts, err := s.Events.MemberCounts(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	data := collectionData{pageData: s.base(c.Name, "/c/"+id+"/stream", "collections"), C: c, Total: counts[id]}
	for i, e := range hist {
		ch := change{Version: i + 1, At: e.OccurredAt}
		switch e.Type {
		case store.TypeCollectionStarted:
			var p store.CollectionStarted
			json.Unmarshal(e.Payload, &p)
			ch.What, ch.Text = "started", p.Include
		case store.TypeCollectionAmended:
			var p store.CollectionAmended
			json.Unmarshal(e.Payload, &p)
			ch.What, ch.Text = "include", p.Include
			if p.Exclude != "" {
				ch.What, ch.Text = "exclude", p.Exclude
			}
		case store.TypeCollectionStopped:
			ch.What = "stopped"
		case store.TypeCollectionResumed:
			ch.What = "resumed"
		}
		data.History = append(data.History, ch)
	}
	for _, m := range members {
		data.Members = append(data.Members, memberRow{EventID: m.EventID, At: m.Event.OccurredAt,
			Text: eventText(m.Event), Version: m.DefinitionVersion, Reason: m.Reason})
	}
	return data, nil
}

// ---- write side: append label.corrected, answer 204, let streams repaint

// removeMember handles POST /c/{id}/remove with form field event_id.
func (s *Server) removeMember(w http.ResponseWriter, r *http.Request) {
	s.correct(w, r, r.PathValue("id"), "remove")
}

// addMember handles POST /inbox/add with form fields event_id and
// collection_id.
func (s *Server) addMember(w http.ResponseWriter, r *http.Request) {
	s.correct(w, r, r.FormValue("collection_id"), "add")
}

func (s *Server) correct(w http.ResponseWriter, r *http.Request, collectionID, action string) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	s.appendOwned(w, r, store.TypeLabelCorrected, store.LabelCorrected{
		EventID: r.FormValue("event_id"), CollectionID: collectionID, Action: action,
	})
}
