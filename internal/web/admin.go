package web

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/brojonat/lifestream/internal/bus"
	"github.com/brojonat/lifestream/internal/metrics"
)

// tiles are the sampled metrics shown as stat tiles, in display order.
var tiles = []metrics.Metric{
	{Name: "cpu_pct", Label: "CPU", Unit: "%"},
	{Name: "heap_mb", Label: "Heap", Unit: " MB"},
	{Name: "sys_mb", Label: "Memory from OS", Unit: " MB"},
	{Name: "goroutines", Label: "Goroutines"},
	{Name: "streams", Label: "Open streams"},
	{Name: "req_per_min", Label: "Requests", Unit: "/min"},
	{Name: "search_ms", Label: "Search latency", Unit: " ms"},
	{Name: "errors", Label: "5xx responses"},
}

const historyPoints = 60

type tile struct {
	metrics.Metric
	Value         float64
	ValueText     string
	Threshold     float64
	ThresholdText string
	Over          bool
	Spark         template.HTML
}

type meter struct {
	Label string
	Text  string
	Pct   int
	Level string
}

type adminData struct {
	pageData
	Sample   *metrics.Sample
	Tiles    []tile
	Meters   []meter
	Limits   metrics.Limits
	Uptime   string
	Interval string
	DataDir  string
}

func (s *Server) adminView(r *http.Request) (any, error) {
	ctx := r.Context()
	data := adminData{
		pageData: s.base("Diagnostics", "/admin/stream", false),
		Limits:   metrics.ReadLimits(ctx, s.JS, s.DataDir, bus.MaxMemory, bus.MaxStore),
		Uptime:   s.Collector.Uptime().Round(time.Second).String(),
		Interval: s.Interval.String(),
		DataDir:  s.DataDir,
	}
	latest, ok, err := metrics.Latest(ctx, s.MetricsKV)
	if err != nil {
		return nil, err
	}
	if !ok {
		return data, nil
	}
	data.Sample = &latest
	hist, err := metrics.History(ctx, s.MetricsStream, historyPoints)
	if err != nil {
		return nil, err
	}
	th, err := metrics.Thresholds(ctx, s.MetricsKV)
	if err != nil {
		return nil, err
	}
	for _, m := range tiles {
		v := latest.Value(m.Name)
		t := tile{Metric: m, Value: v, ValueText: compact(v), Threshold: th[m.Name]}
		if t.Threshold > 0 {
			t.ThresholdText = compact(t.Threshold)
			t.Over = v > t.Threshold
		}
		series := make([]float64, 0, len(hist))
		for _, h := range hist {
			series = append(series, h.Value(m.Name))
		}
		t.Spark = sparkline(series, m.Unit)
		data.Tiles = append(data.Tiles, t)
	}
	l := data.Limits
	data.Meters = []meter{
		gauge("Open files", float64(latest.OpenFiles), float64(l.OpenFilesMax), "%d of %d"),
		gauge("JetStream store", l.StoreUsedMB, l.StoreLimitMB, "%.1f of %.0f MB"),
		gauge("Data dir disk", l.DiskTotalMB-l.DiskFreeMB, l.DiskTotalMB, "%.0f of %.0f MB"),
	}
	return data, nil
}

func gauge(label string, used, limit float64, format string) meter {
	m := meter{Label: label, Level: "ok"}
	if limit <= 0 {
		m.Text = compact(used) + ", no limit"
		return m
	}
	m.Pct = int(100 * used / limit)
	if m.Pct > 100 {
		m.Pct = 100
	}
	if strings.Contains(format, "%d") {
		m.Text = fmt.Sprintf(format, int(used), int(limit))
	} else {
		m.Text = fmt.Sprintf(format, used, limit)
	}
	switch {
	case m.Pct >= 90:
		m.Level = "critical"
	case m.Pct >= 75:
		m.Level = "warning"
	}
	return m
}

// compact formats a value the way a stat tile wants: 3 significant-ish digits.
func compact(v float64) string {
	switch {
	case v >= 1e6:
		return strconv.FormatFloat(v/1e6, 'f', 1, 64) + "M"
	case v >= 1e4:
		return strconv.FormatFloat(v/1e3, 'f', 1, 64) + "K"
	case v >= 100 || v == float64(int64(v)):
		return strconv.FormatFloat(v, 'f', 0, 64)
	case v >= 1:
		return strconv.FormatFloat(v, 'f', 1, 64)
	default:
		return strconv.FormatFloat(v, 'g', 2, 64)
	}
}

// sparkline renders a single-series trend: a 2px line in the muted ink, the
// current point in the accent, a hairline baseline, and per-point hit targets
// with native tooltips. It is inline SVG so it fat-morphs like everything else.
func sparkline(series []float64, unit string) template.HTML {
	const w, h, pad = 160.0, 40.0, 4.0
	n := len(series)
	if n < 2 {
		return template.HTML(`<svg class="spark" viewBox="0 0 160 40" width="160" height="40" role="img" aria-label="not enough samples yet"></svg>`)
	}
	lo, hi := series[0], series[0]
	for _, v := range series {
		lo, hi = min(lo, v), max(hi, v)
	}
	if hi == lo {
		hi = lo + 1
	}
	x := func(i int) float64 { return pad + (w-2*pad)*float64(i)/float64(n-1) }
	y := func(v float64) float64 { return h - pad - (h-2*pad)*(v-lo)/(hi-lo) }
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="spark" viewBox="0 0 %.0f %.0f" width="%.0f" height="%.0f" role="img" aria-label="last %d samples, low %s high %s">`, w, h, w, h, n, compact(lo), compact(hi))
	fmt.Fprintf(&b, `<line class="base" x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f"/>`, pad, h-pad, w-pad, h-pad)
	b.WriteString(`<polyline class="line" points="`)
	for i, v := range series {
		fmt.Fprintf(&b, "%.1f,%.1f ", x(i), y(v))
	}
	b.WriteString(`"/>`)
	step := (w - 2*pad) / float64(n-1)
	for i, v := range series {
		fmt.Fprintf(&b, `<rect class="hit" x="%.1f" y="0" width="%.1f" height="%.0f"><title>%s%s</title></rect>`, x(i)-step/2, step, h, compact(v), strings.TrimSpace(unit))
	}
	fmt.Fprintf(&b, `<circle class="now" cx="%.1f" cy="%.1f" r="4"/>`, x(n-1), y(series[n-1]))
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

// setThreshold is the write side: store the ceiling and answer 204. The
// dashboard stream repaints with the new status.
func (s *Server) setThreshold(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	known := false
	for _, m := range tiles {
		if m.Name == name {
			known = true
		}
	}
	if !known {
		http.Error(w, "unknown metric", http.StatusNotFound)
		return
	}
	// The limit box is submitted as a form field, not a signal: one input per
	// tile, nothing shared across the page.
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	v, _ := strconv.ParseFloat(strings.TrimSpace(r.FormValue("value")), 64)
	if err := metrics.SetThreshold(r.Context(), s.MetricsKV, name, v); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// metricsWatch wakes the dashboard on new samples and threshold changes.
func (s *Server) metricsWatch(ctx context.Context) (watcher, error) {
	return metrics.Watch(ctx, s.MetricsKV)
}
