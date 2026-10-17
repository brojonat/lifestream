package metrics

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/brojonat/lifestream/internal/bus"
)

func TestSamplerRecordsHistoryAndLatest(t *testing.T) {
	b, err := bus.Start(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream, err := b.Stream(ctx, StreamName, []string{Subject}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	kv, err := b.KV(ctx, "metrics")
	if err != nil {
		t.Fatal(err)
	}
	c := NewCollector()
	c.Request(200)
	c.Request(503)
	c.StreamOpened()
	c.Search(20 * time.Millisecond)
	s := NewSampler(c, b.JS(), stream, kv, slog.New(slog.NewTextHandler(io.Discard, nil)))

	for i := 0; i < 3; i++ {
		if err := s.Once(ctx); err != nil {
			t.Fatal(err)
		}
	}
	hist, err := History(ctx, stream, 10)
	if err != nil || len(hist) != 3 {
		t.Fatalf("history: %d %v", len(hist), err)
	}
	if !hist[0].Time.Before(hist[2].Time) {
		t.Error("history should be oldest first")
	}
	latest, ok, err := Latest(ctx, kv)
	if err != nil || !ok {
		t.Fatalf("latest: %v %v", ok, err)
	}
	if latest.Requests != 2 || latest.Errors != 1 || latest.OpenStreams != 1 || latest.SearchMs != 20 || latest.Goroutines == 0 || latest.HeapMB == 0 {
		t.Errorf("sample: %+v", latest)
	}
	if latest.Value("errors") != 1 || latest.Value("nope") != 0 {
		t.Errorf("Value lookup")
	}

	if err := SetThreshold(ctx, kv, "heap_mb", 512); err != nil {
		t.Fatal(err)
	}
	th, _ := Thresholds(ctx, kv)
	if th["heap_mb"] != 512 || len(th) != 1 {
		t.Errorf("thresholds: %v", th)
	}
	SetThreshold(ctx, kv, "heap_mb", 0)
	if th, _ = Thresholds(ctx, kv); len(th) != 0 {
		t.Errorf("clear: %v", th)
	}

	l := ReadLimits(ctx, b.JS(), t.TempDir(), bus.MaxMemory, bus.MaxStore)
	// The KV bucket is a stream too, so two streams.
	if l.NumCPU == 0 || l.OpenFilesMax == 0 || l.DiskTotalMB == 0 || l.Streams != 2 || l.StoreLimitMB == 0 {
		t.Errorf("limits: %+v", l)
	}
	if latest.OpenFiles < 3 {
		t.Errorf("open files should count at least stdio: %d", latest.OpenFiles)
	}
}

func TestPrometheusExposition(t *testing.T) {
	c := NewCollector()
	c.Request(500)
	rec := httptest.NewRecorder()
	Handler(c).ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()
	for _, want := range []string{"# TYPE app_requests_total counter", "app_requests_total 1", "app_request_errors_total 1", "go_goroutines ", "app_open_files "} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}
	if !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain; version=0.0.4") {
		t.Errorf("content type %q", rec.Header().Get("Content-Type"))
	}
}
