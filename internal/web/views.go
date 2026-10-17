package web

import (
	"bytes"
	"net/http"
	"strings"
	"time"

	"github.com/brojonat/lifestream/internal/state"
	"github.com/starfederation/datastar-go/datastar"
)

// ---- read side --------------------------------------------------------

type itemsData struct {
	pageData
	Items []state.Item
}

func (s *Server) itemsView(r *http.Request) (any, error) {
	ctx := r.Context()
	only, err := s.Store.StarredOnly(ctx)
	if err != nil {
		return nil, err
	}
	items, err := s.Store.Items(ctx)
	if err != nil {
		return nil, err
	}
	if only {
		kept := items[:0]
		for _, it := range items {
			if it.Starred {
				kept = append(kept, it)
			}
		}
		items = kept
	}
	return itemsData{pageData: s.base("Items", "/items/stream", only), Items: items}, nil
}

type itemData struct {
	pageData
	Item state.Item
}

func (s *Server) itemView(r *http.Request) (any, error) {
	ctx := r.Context()
	id := r.PathValue("id")
	it, ok, err := s.Store.Item(ctx, id)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, notFound("item " + id)
	}
	only, err := s.Store.StarredOnly(ctx)
	if err != nil {
		return nil, err
	}
	return itemData{pageData: s.base(it.Title, "/i/"+id+"/stream", only), Item: it}, nil
}

type searchData struct {
	Query string
	Items []state.Item
}

// search is a short-lived read: filter items for the typed query and patch
// the results slot. An empty query clears it.
func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	var signals struct {
		Q string `json:"q"`
	}
	if err := datastar.ReadSignals(r, &signals); err != nil {
		http.Error(w, "bad signals", http.StatusBadRequest)
		return
	}
	data := searchData{Query: strings.TrimSpace(signals.Q)}
	if data.Query != "" {
		start := time.Now()
		items, err := s.Store.Items(r.Context())
		if err != nil {
			s.fail(w, r, err)
			return
		}
		q := strings.ToLower(data.Query)
		for _, it := range items {
			if strings.Contains(strings.ToLower(it.Title), q) || strings.Contains(strings.ToLower(it.Note), q) {
				data.Items = append(data.Items, it)
			}
		}
		if s.Collector != nil {
			s.Collector.Search(time.Since(start))
		}
	}
	var buf bytes.Buffer
	if err := s.results.ExecuteTemplate(&buf, "results", data); err != nil {
		s.fail(w, r, err)
		return
	}
	sse := datastar.NewSSE(w, r)
	sse.PatchElements(buf.String())
}

// ---- write side: mutate, answer 204, let the stream repaint --------------

func (s *Server) addItem(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	title := strings.TrimSpace(r.FormValue("title"))
	if title == "" {
		http.Error(w, "title required", http.StatusBadRequest)
		return
	}
	if _, err := s.Store.Put(r.Context(), state.Item{Title: title}); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) toggleStar(w http.ResponseWriter, r *http.Request) {
	s.mutate(w, r, func(it *state.Item) { it.Starred = !it.Starred })
}

func (s *Server) saveNote(w http.ResponseWriter, r *http.Request) {
	var signals struct {
		Note string `json:"note"`
	}
	if err := datastar.ReadSignals(r, &signals); err != nil {
		http.Error(w, "bad signals", http.StatusBadRequest)
		return
	}
	s.mutate(w, r, func(it *state.Item) { it.Note = signals.Note })
}

func (s *Server) deleteItem(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.Delete(r.Context(), r.PathValue("id")); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// mutate is the shared write path: load, apply, save.
func (s *Server) mutate(w http.ResponseWriter, r *http.Request, apply func(*state.Item)) {
	ctx := r.Context()
	it, ok, err := s.Store.Item(ctx, r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !ok {
		http.Error(w, "item not found", http.StatusNotFound)
		return
	}
	apply(&it)
	if _, err := s.Store.Put(ctx, it); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) toggleStarredOnly(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	on, err := s.Store.StarredOnly(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.Store.SetStarredOnly(ctx, !on); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
