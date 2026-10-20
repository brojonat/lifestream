// Package web is the owner's dashboard. It follows the Tao of Datastar: the
// server owns all state, each page holds one long-lived SSE read that
// re-renders its live region whenever something changes, and writes are
// short requests that append an event and answer 204. The same template
// renders the first paint and every patch.
package web

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/brojonat/lifestream/internal/metrics"
	"github.com/brojonat/lifestream/internal/store"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/starfederation/datastar-go/datastar"
)

//go:embed templates/*.html
var files embed.FS

//go:embed assets/*
var assets embed.FS

// Deps is everything the handlers need. Nothing is global.
type Deps struct {
	App    string // shown in the header and title
	Events *store.Store
	Log    *slog.Logger

	// Diagnostics; leave Collector nil to disable /admin and /metrics.
	Collector     *metrics.Collector
	MetricsKV     jetstream.KeyValue
	MetricsStream jetstream.Stream
	JS            jetstream.JetStream
	DataDir       string
	Interval      time.Duration

	// The live agent panel: where the agent's /live feed is, and the client
	// that reads it (no timeout; the feed is a stream). Leave AgentURL empty
	// to show the panel as unconfigured.
	AgentURL    string
	AgentClient *http.Client
}

// changes wakes a stream. Next must be called before rendering, so a change
// that lands mid-render still wakes the following wait.
type changes interface {
	Next() <-chan struct{}
	Stop()
}

// storeChanges wakes on every append to the event store.
type storeChanges struct{ st *store.Store }

func (c storeChanges) Next() <-chan struct{} { return c.st.Changed() }
func (c storeChanges) Stop()                 {}

// kvChanges turns a KV watcher into a wake-up channel that coalesces
// bursts into one pending wake.
func kvChanges(w jetstream.KeyWatcher) changes {
	c := &kvWake{w: w, ch: make(chan struct{}, 1)}
	go func() {
		for range w.Updates() {
			select {
			case c.ch <- struct{}{}:
			default:
			}
		}
	}()
	return c
}

type kvWake struct {
	w  jetstream.KeyWatcher
	ch chan struct{}
}

func (c *kvWake) Next() <-chan struct{} { return c.ch }
func (c *kvWake) Stop()                 { c.w.Stop() }

// Server holds the dependencies and parsed templates.
type Server struct {
	Deps
	pages map[string]*template.Template
}

// NewHandler wires the routes.
func NewHandler(d Deps) http.Handler {
	s := &Server{Deps: d, pages: map[string]*template.Template{}}
	if s.AgentClient == nil {
		s.AgentClient = &http.Client{}
	}
	for _, name := range []string{"metrics", "inbox", "collections", "collection", "rules", "log", "admin", "agent"} {
		s.pages[name] = template.Must(template.New("").Funcs(funcs).ParseFS(files,
			"templates/layout.html", "templates/"+name+".html"))
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.Handle("GET /favicon.png", s.asset("favicon.png", "public, max-age=86400"))
	mux.Handle("GET /apple-touch-icon.png", s.asset("apple-touch-icon.png", "public, max-age=86400"))
	mux.Handle("GET /static/components.js", s.asset("components.js", "no-cache"))
	mux.HandleFunc("GET /{$}", s.page("metrics", s.metricsView))
	mux.HandleFunc("GET /stream", s.stream("metrics", s.metricsView, s.storeWatch))
	mux.HandleFunc("GET /inbox", s.page("inbox", s.inboxView))
	mux.HandleFunc("GET /inbox/stream", s.stream("inbox", s.inboxView, s.storeWatch))
	mux.HandleFunc("POST /inbox/add", s.addMember)
	mux.HandleFunc("GET /collections", s.page("collections", s.collectionsView))
	mux.HandleFunc("GET /collections/stream", s.stream("collections", s.collectionsView, s.storeWatch))
	mux.HandleFunc("GET /c/{id}", s.page("collection", s.collectionView))
	mux.HandleFunc("GET /c/{id}/stream", s.stream("collection", s.collectionView, s.storeWatch))
	mux.HandleFunc("POST /c/{id}/remove", s.removeMember)
	mux.HandleFunc("GET /log", s.page("log", s.logView))
	mux.HandleFunc("GET /log/stream", s.stream("log", s.logView, s.storeWatch))
	mux.HandleFunc("POST /log/{id}/undo", s.undoSteering)
	mux.HandleFunc("POST /review/{id}/answer", s.answerFlag)
	mux.HandleFunc("GET /rules", s.page("rules", s.rulesView))
	mux.HandleFunc("GET /rules/stream", s.stream("rules", s.rulesView, s.storeWatch))
	mux.HandleFunc("POST /rules", s.addRule)
	mux.HandleFunc("POST /rules/{id}/remove", s.removeRule)
	mux.HandleFunc("GET /agent", s.page("agent", s.agentView))
	mux.HandleFunc("GET /agent/stream", s.agentStream)
	if s.Collector != nil {
		mux.HandleFunc("GET /admin", s.page("admin", s.adminView))
		mux.HandleFunc("GET /admin/stream", s.stream("admin", s.adminView, s.metricsWatch))
		mux.HandleFunc("PUT /admin/threshold/{name}", s.setThreshold)
		mux.Handle("GET /metrics", metrics.Handler(s.Collector))
	}
	return s.logRequests(mux)
}

var funcs = template.FuncMap{
	"num":    num,
	"signed": signed,
	"deref":  func(p *float64) float64 { return *p },
	// utc is a time for <local-time datetime=…>, which the browser shows in
	// the viewer's timezone.
	"utc": func(t time.Time) string { return t.UTC().Format(time.RFC3339) },
	"usd": func(v float64) string {
		if v < 0.01 {
			return fmt.Sprintf("$%.4f", v)
		}
		return fmt.Sprintf("$%.2f", v)
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
	App       string
	Title     string
	StreamURL string
	Nav       string // which nav entry is current
}

func (s *Server) base(title, streamURL, nav string) pageData {
	return pageData{App: s.App, Title: title, StreamURL: streamURL, Nav: nav}
}

// owner picks whose dashboard this is. Until login exists (TODO 24) the
// dashboard serves the only user and refuses when there are several.
func (s *Server) owner(ctx context.Context) (store.User, error) {
	users, err := s.Events.Users(ctx)
	if err != nil {
		return store.User{}, err
	}
	switch len(users) {
	case 0:
		return store.User{}, errNoUsers
	case 1:
		return users[0], nil
	default:
		return store.User{}, errNeedsLogin
	}
}

var (
	errNoUsers    = errors.New("no users yet: run `lifestream users add`")
	errNeedsLogin = errors.New("more than one user and no login yet (TODO 24)")
)

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

func (s *Server) storeWatch(context.Context) (changes, error) { return storeChanges{s.Events}, nil }

// stream is the read path. It renders the region now, then again every time
// something changes, until the client goes away.
func (s *Server) stream(name string, v view, watch func(context.Context) (changes, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, err := v(r); err != nil {
			s.fail(w, r, err)
			return
		}
		ctx := r.Context()
		ch, err := watch(ctx)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		defer ch.Stop()
		if s.Collector != nil {
			s.Collector.StreamOpened()
			defer s.Collector.StreamClosed()
		}

		sse := datastar.NewSSE(w, r, datastar.WithCompression())
		last := ""
		for {
			wake := ch.Next()
			data, err := v(r)
			if err != nil {
				s.Log.Warn("stream render", "page", name, "error", err)
				return
			}
			var buf bytes.Buffer
			if err := s.pages[name].ExecuteTemplate(&buf, "region", data); err != nil {
				s.Log.Warn("stream template", "page", name, "error", err)
				return
			}
			// Appends that don't change this page (most of them) send nothing.
			if html := buf.String(); html != last {
				if err := sse.PatchElements(html); err != nil {
					return // client disconnected
				}
				last = html
			}
			select {
			case <-ctx.Done():
				return
			case <-wake:
			}
		}
	}
}

// asset serves one embedded file.
func (s *Server) asset(name, cacheControl string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := assets.ReadFile("assets/" + name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", cacheControl)
		http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(b))
	})
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte("ok\n"))
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, errNoUsers), errors.Is(err, errNeedsLogin):
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	case errors.Is(err, store.ErrNotFound):
		http.Error(w, "not found", http.StatusNotFound)
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
