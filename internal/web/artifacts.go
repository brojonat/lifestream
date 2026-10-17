package web

import (
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/brojonat/lifestream/internal/store"
)

// MaxArtifactHTML caps one uploaded artifact.
const MaxArtifactHTML = 16 << 20

type artifactsData struct {
	pageData
	Artifacts []store.Artifact
}

func (s *Server) artifactsView(r *http.Request) (any, error) {
	ctx := r.Context()
	u, err := s.owner(ctx)
	if err != nil {
		return nil, err
	}
	list, err := s.Events.Artifacts(ctx, u.ID, 100)
	if err != nil {
		return nil, err
	}
	return artifactsData{pageData: s.base("Artifacts", "/artifacts/stream", "artifacts"), Artifacts: list}, nil
}

// saveArtifact handles POST /artifacts: a multipart upload (title, html
// file) or a claude.ai link (title, url) as a form or JSON payload. The
// HTML is stored, never rendered here: the dashboard's origin must not run
// it (the sandboxed viewer is TODO A3).
func (s *Server) saveArtifact(w http.ResponseWriter, r *http.Request) {
	var a store.ArtifactSaved
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		r.Body = http.MaxBytesReader(w, r.Body, MaxArtifactHTML+1<<20)
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			http.Error(w, "upload too large or malformed", http.StatusRequestEntityTooLarge)
			return
		}
		a.Title, a.URL = strings.TrimSpace(r.FormValue("title")), strings.TrimSpace(r.FormValue("url"))
		if f, hdr, err := r.FormFile("html"); err == nil {
			defer f.Close()
			b, err := io.ReadAll(io.LimitReader(f, MaxArtifactHTML+1))
			if err != nil || len(b) > MaxArtifactHTML {
				http.Error(w, "artifact HTML is over 16 MB", http.StatusRequestEntityTooLarge)
				return
			}
			a.HTML = string(b)
			if a.Title == "" {
				a.Title = strings.TrimSuffix(hdr.Filename, filepath.Ext(hdr.Filename))
			}
		} else if !errors.Is(err, http.ErrMissingFile) {
			http.Error(w, "reading upload: "+err.Error(), http.StatusBadRequest)
			return
		}
	} else {
		f, err := fields(w, r, "title", "url")
		if err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		a.Title, a.URL = f["title"], f["url"]
	}
	s.appendOwned(w, r, store.TypeArtifactSaved, a)
}
