// Package web is the browser app. It follows the Tao of Datastar: the server
// owns all state, each page holds one long-lived SSE read that re-renders its
// live region whenever the KV bucket changes, and writes are short requests
// that mutate and answer 204. The same template renders the first paint and
// every patch.
package web

import (
	"bytes"
	"context"
	"embed"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/brojonat/lifestream/internal/metrics"
	"github.com/brojonat/lifestream/internal/state"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/starfederation/datastar-go/datastar"
)

//go:embed templates/*.html
var files embed.FS

//go:embed assets/*
var assets embed.FS

// Deps is everything the handlers need. Nothing is global.
type Deps struct {
	App   string // shown in the header and title
	Store *state.Store
	Log   *slog.Logger

	// Diagnostics; leave Collector nil to disable /admin and /metrics.
	Collector     *metrics.Collector
	MetricsKV     jetstream.KeyValue
	MetricsStream jetstream.Stream
	JS            jetstream.JetStream
	DataDir       string
	Interval      time.Duration
}

// watcher is the part of a KeyWatcher the stream loop uses.
type watcher interface {
	Updates() <-chan jetstream.KeyValueEntry
	Stop() error
}

// Server holds the dependencies and parsed templates.
type Server struct {
	Deps
	pages   map[string]*template.Template
	results *template.Template
}

// NewHandler wires the routes.
func NewHandler(d Deps) http.Handler {
	s := &Server{Deps: d, pages: map[string]*template.Template{}}
	for _, name := range []string{"items", "item", "admin"} {
		s.pages[name] = template.Must(template.New("").Funcs(funcs).ParseFS(files,
			"templates/layout.html", "templates/results.html", "templates/filter.html", "templates/"+name+".html"))
	}
	s.results = template.Must(template.New("").Funcs(funcs).ParseFS(files, "templates/results.html"))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.Handle("GET /favicon.png", s.asset("favicon.png"))
	mux.Handle("GET /apple-touch-icon.png", s.asset("apple-touch-icon.png"))
	mux.HandleFunc("GET /{$}", s.page("items", s.itemsView))
	mux.HandleFunc("GET /items/stream", s.stream("items", s.itemsView, s.stateWatch))
	mux.HandleFunc("GET /i/{id}", s.page("item", s.itemView))
	mux.HandleFunc("GET /i/{id}/stream", s.stream("item", s.itemView, s.stateWatch))
	mux.HandleFunc("GET /search", s.search)
	mux.HandleFunc("POST /items", s.addItem)
	mux.HandleFunc("POST /i/{id}/star", s.toggleStar)
	mux.HandleFunc("PUT /i/{id}/note", s.saveNote)
	mux.HandleFunc("DELETE /i/{id}", s.deleteItem)
	mux.HandleFunc("POST /prefs/starred", s.toggleStarredOnly)
	if s.Collector != nil {
		mux.HandleFunc("GET /admin", s.page("admin", s.adminView))
		mux.HandleFunc("GET /admin/stream", s.stream("admin", s.adminView, s.metricsWatch))
		mux.HandleFunc("PUT /admin/threshold/{name}", s.setThreshold)
		mux.Handle("GET /metrics", metrics.Handler(s.Collector))
	}
	return s.logRequests(mux)
}

var funcs = template.FuncMap{
	"pathEscape": url.PathEscape,
	"firstLine": func(s string) string {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			return s[:i]
		}
		return s
	},
	"trunc": func(n int, s string) string {
		if len(s) > n {
			return s[:n] + "…"
		}
		return s
	},
	"ago": func(t time.Time) string {
		if t.IsZero() {
			return "never"
		}
		d := time.Since(t)
		switch {
		case d < time.Minute:
			return "just now"
		case d < time.Hour:
			return fmt.Sprintf("%dm ago", int(d.Minutes()))
		case d < 48*time.Hour:
			return fmt.Sprintf("%dh ago", int(d.Hours()))
		default:
			return fmt.Sprintf("%dd ago", int(d.Hours()/24))
		}
	},
}

// view builds the data for one page.
type view func(r *http.Request) (any, error)

// pageData is what the layout needs on top of the region's own data.
type pageData struct {
	App         string
	Title       string
	StreamURL   string
	Query       string
	StarredOnly bool
}

func (s *Server) base(title, streamURL string, starredOnly bool) pageData {
	return pageData{App: s.App, Title: title, StreamURL: streamURL, StarredOnly: starredOnly}
}

// page renders the full document: layout plus the live region's first paint.
func (s *Server) page(name string, v view) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data, err := v(r)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		var buf bytes.Buffer
		if err := s.pages[name].ExecuteTemplate(&buf, "layout", data); err != nil {
			s.fail(w, r, fmt.Errorf("rendering %s: %w", name, err))
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(buf.Bytes())
	}
}

func (s *Server) stateWatch(ctx context.Context) (watcher, error) { return s.Store.Watch(ctx) }

// stream is the read path. It renders the region now, then again every time
// the watched bucket changes, until the client goes away.
func (s *Server) stream(name string, v view, watch func(context.Context) (watcher, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, err := v(r); err != nil {
			s.fail(w, r, err)
			return
		}
		ctx := r.Context()
		watcher, err := watch(ctx)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		defer watcher.Stop()
		if s.Collector != nil {
			s.Collector.StreamOpened()
			defer s.Collector.StreamClosed()
		}

		sse := datastar.NewSSE(w, r, datastar.WithCompression())
		for {
			data, err := v(r)
			if err != nil {
				s.Log.Warn("stream render", "page", name, "error", err)
				return
			}
			// The header filter lives outside the region, so patch both.
			for _, tmpl := range []string{"region", "filter"} {
				var buf bytes.Buffer
				if err := s.pages[name].ExecuteTemplate(&buf, tmpl, data); err != nil {
					s.Log.Warn("stream template", "page", name, "template", tmpl, "error", err)
					return
				}
				if err := sse.PatchElements(buf.String()); err != nil {
					return // client disconnected
				}
			}
			select {
			case <-ctx.Done():
				return
			case _, ok := <-watcher.Updates():
				if !ok {
					return
				}
				drain(watcher)
			}
		}
	}
}

// drain coalesces a burst of updates into one re-render.
func drain(w watcher) {
	for {
		select {
		case <-w.Updates():
		default:
			return
		}
	}
}

// asset serves one embedded file with a long cache life; the binary is the
// version, so a new build is a new URL only if you rename the file.
func (s *Server) asset(name string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := assets.ReadFile("assets/" + name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=86400")
		http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(b))
	})
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte("ok\n"))
}

// notFound is returned by views for unknown resources.
type notFound string

func (e notFound) Error() string { return string(e) + " not found" }

func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	if nf, ok := err.(notFound); ok {
		http.Error(w, nf.Error(), http.StatusNotFound)
		return
	}
	s.Log.Error("request failed", "path", r.URL.Path, "error", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Flush keeps SSE streaming through the wrapper.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		if s.Collector != nil {
			s.Collector.Request(sw.status)
		}
		level := slog.LevelInfo
		if strings.HasSuffix(r.URL.Path, "/stream") {
			level = slog.LevelDebug
		}
		s.Log.Log(r.Context(), level, "request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", sw.status,
			"duration", time.Since(start).Round(time.Microsecond).String(),
		)
	})
}
