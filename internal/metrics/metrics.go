// Package metrics observes the app itself. A Collector counts what handlers
// do; Sample snapshots process resources; a Sampler records samples into a
// JetStream stream and the latest into a KV bucket so the dashboard can watch
// it; thresholds set by the admin live in the same bucket.
package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// Collector holds counters handlers bump. All methods are goroutine-safe.
type Collector struct {
	requests    atomic.Int64
	errors      atomic.Int64
	openStreams atomic.Int64
	searchNanos atomic.Int64
	searchCount atomic.Int64
	started     time.Time
}

func NewCollector() *Collector { return &Collector{started: time.Now()} }

func (c *Collector) Request(status int) {
	c.requests.Add(1)
	if status >= 500 {
		c.errors.Add(1)
	}
}
func (c *Collector) StreamOpened()            { c.openStreams.Add(1) }
func (c *Collector) StreamClosed()            { c.openStreams.Add(-1) }
func (c *Collector) Search(d time.Duration)   { c.searchNanos.Add(int64(d)); c.searchCount.Add(1) }
func (c *Collector) OpenStreams() int64       { return c.openStreams.Load() }
func (c *Collector) Requests() (int64, int64) { return c.requests.Load(), c.errors.Load() }
func (c *Collector) Uptime() time.Duration    { return time.Since(c.started) }
func (c *Collector) searches() (time.Duration, int64) {
	return time.Duration(c.searchNanos.Load()), c.searchCount.Load()
}

// Sample is one observation of the process. Field names are the metric names
// thresholds are keyed by.
type Sample struct {
	Time        time.Time `json:"time"`
	CPUPct      float64   `json:"cpu_pct"`
	HeapMB      float64   `json:"heap_mb"`
	SysMB       float64   `json:"sys_mb"`
	Goroutines  int       `json:"goroutines"`
	OpenStreams int64     `json:"streams"`
	Requests    int64     `json:"requests"`
	Errors      int64     `json:"errors"`
	ReqPerMin   float64   `json:"req_per_min"`
	SearchMs    float64   `json:"search_ms"`
	OpenFiles   int       `json:"open_files"`
	GCs         uint32    `json:"gcs"`
}

// Metric describes a sampled value for display and thresholds.
type Metric struct {
	Name  string // json key in Sample
	Label string
	Unit  string
	// Limit, when non-zero, turns the tile into a meter against this ceiling.
	Limit float64
}

// Value pulls the named metric out of a sample.
func (s Sample) Value(name string) float64 {
	switch name {
	case "cpu_pct":
		return s.CPUPct
	case "heap_mb":
		return s.HeapMB
	case "sys_mb":
		return s.SysMB
	case "goroutines":
		return float64(s.Goroutines)
	case "streams":
		return float64(s.OpenStreams)
	case "requests":
		return float64(s.Requests)
	case "errors":
		return float64(s.Errors)
	case "req_per_min":
		return s.ReqPerMin
	case "search_ms":
		return s.SearchMs
	case "open_files":
		return float64(s.OpenFiles)
	case "gcs":
		return float64(s.GCs)
	}
	return 0
}

// Sampler takes periodic samples and records them.
type Sampler struct {
	c       *Collector
	stream  jetstream.Stream
	js      jetstream.JetStream
	kv      jetstream.KeyValue
	subject string
	log     *slog.Logger

	lastCPU  time.Duration
	lastWall time.Time
	lastReq  int64
}

const (
	StreamName = "METRICS"
	Subject    = "app.metrics.sample"
	latestKey  = "latest"
	thresholdP = "threshold."
)

// NewSampler wires the sampler to its stream and bucket.
func NewSampler(c *Collector, js jetstream.JetStream, stream jetstream.Stream, kv jetstream.KeyValue, log *slog.Logger) *Sampler {
	return &Sampler{c: c, js: js, stream: stream, kv: kv, subject: Subject, log: log, lastWall: time.Now(), lastCPU: cpuTime()}
}

// Run samples every interval until ctx ends.
func (s *Sampler) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := s.Once(ctx); err != nil && ctx.Err() == nil {
			s.log.Warn("metrics sample", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Once takes and records one sample.
func (s *Sampler) Once(ctx context.Context) error {
	sample := s.Take()
	b, err := json.Marshal(sample)
	if err != nil {
		return err
	}
	if _, err := s.js.Publish(ctx, s.subject, b); err != nil {
		return fmt.Errorf("publishing sample: %w", err)
	}
	if _, err := s.kv.Put(ctx, latestKey, b); err != nil {
		return fmt.Errorf("storing latest sample: %w", err)
	}
	return nil
}

// Take builds a sample from the process and the collector.
func (s *Sampler) Take() Sample {
	now := time.Now()
	cpu := cpuTime()
	var pct float64
	if wall := now.Sub(s.lastWall); wall > 0 {
		pct = 100 * float64(cpu-s.lastCPU) / float64(wall)
	}
	reqs, errs := s.c.Requests()
	var perMin float64
	if wall := now.Sub(s.lastWall); wall > 0 {
		perMin = float64(reqs-s.lastReq) / wall.Minutes()
	}
	s.lastCPU, s.lastWall, s.lastReq = cpu, now, reqs

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	searchTotal, searchN := s.c.searches()
	var searchMs float64
	if searchN > 0 {
		searchMs = float64(searchTotal.Milliseconds()) / float64(searchN)
	}
	return Sample{
		Time:        now,
		CPUPct:      pct,
		HeapMB:      float64(ms.HeapAlloc) / 1e6,
		SysMB:       float64(ms.Sys) / 1e6,
		Goroutines:  runtime.NumGoroutine(),
		OpenStreams: s.c.OpenStreams(),
		Requests:    reqs,
		Errors:      errs,
		ReqPerMin:   perMin,
		SearchMs:    searchMs,
		OpenFiles:   openFiles(),
		GCs:         ms.NumGC,
	}
}

// History returns up to n most recent samples, oldest first.
func History(ctx context.Context, stream jetstream.Stream, n int) ([]Sample, error) {
	info, err := stream.Info(ctx)
	if err != nil {
		return nil, fmt.Errorf("stream info: %w", err)
	}
	last := info.State.LastSeq
	first := info.State.FirstSeq
	var out []Sample
	for seq := last; seq >= first && seq > 0 && len(out) < n; seq-- {
		msg, err := stream.GetMsg(ctx, seq)
		if err != nil {
			if errors.Is(err, jetstream.ErrMsgNotFound) {
				continue
			}
			return nil, fmt.Errorf("reading sample %d: %w", seq, err)
		}
		var s Sample
		if json.Unmarshal(msg.Data, &s) == nil {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	return out, nil
}

// Latest returns the most recent sample from the bucket, if any.
func Latest(ctx context.Context, kv jetstream.KeyValue) (Sample, bool, error) {
	e, err := kv.Get(ctx, latestKey)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return Sample{}, false, nil
	}
	if err != nil {
		return Sample{}, false, err
	}
	var s Sample
	if err := json.Unmarshal(e.Value(), &s); err != nil {
		return Sample{}, false, err
	}
	return s, true, nil
}

// Thresholds returns the admin-set ceilings keyed by metric name.
func Thresholds(ctx context.Context, kv jetstream.KeyValue) (map[string]float64, error) {
	out := map[string]float64{}
	lister, err := kv.ListKeys(ctx)
	if errors.Is(err, jetstream.ErrNoKeysFound) {
		return out, nil
	}
	if err != nil {
		return nil, fmt.Errorf("listing thresholds: %w", err)
	}
	defer lister.Stop()
	for key := range lister.Keys() {
		if !strings.HasPrefix(key, thresholdP) {
			continue
		}
		e, err := kv.Get(ctx, key)
		if err != nil {
			continue
		}
		if v, err := strconv.ParseFloat(strings.TrimSpace(string(e.Value())), 64); err == nil {
			out[strings.TrimPrefix(key, thresholdP)] = v
		}
	}
	return out, nil
}

// SetThreshold stores a ceiling; a value <= 0 clears it.
func SetThreshold(ctx context.Context, kv jetstream.KeyValue, name string, v float64) error {
	if v <= 0 {
		err := kv.Delete(ctx, thresholdP+name)
		if err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
			return fmt.Errorf("clearing threshold: %w", err)
		}
		return nil
	}
	if _, err := kv.Put(ctx, thresholdP+name, []byte(strconv.FormatFloat(v, 'f', -1, 64))); err != nil {
		return fmt.Errorf("storing threshold: %w", err)
	}
	return nil
}

// Watch wakes on any change to the bucket: a new sample or a threshold.
func Watch(ctx context.Context, kv jetstream.KeyValue) (jetstream.KeyWatcher, error) {
	return kv.WatchAll(ctx, jetstream.UpdatesOnly())
}

// Limits is what the process is allowed, as it sees it.
type Limits struct {
	PID          int
	GoVersion    string
	NumCPU       int
	GOMAXPROCS   int
	OpenFilesMax uint64
	DiskTotalMB  float64
	DiskFreeMB   float64
	StoreUsedMB  float64
	StoreLimitMB float64 // 0 = unlimited
	MemUsedMB    float64
	MemLimitMB   float64
	Streams      int
	Consumers    int
}

// ReadLimits gathers limits from the OS and JetStream. memLimit and
// storeLimit are the server-wide ceilings the bus was started with; the
// account view reports unlimited, so they are supplied by the caller.
func ReadLimits(ctx context.Context, js jetstream.JetStream, dataDir string, memLimit, storeLimit int64) Limits {
	l := Limits{PID: os.Getpid(), GoVersion: runtime.Version(), NumCPU: runtime.NumCPU(), GOMAXPROCS: runtime.GOMAXPROCS(0)}
	var rl syscall.Rlimit
	if syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rl) == nil {
		l.OpenFilesMax = uint64(rl.Cur)
	}
	var st syscall.Statfs_t
	if syscall.Statfs(dataDir, &st) == nil {
		l.DiskTotalMB = float64(st.Blocks) * float64(st.Bsize) / 1e6
		l.DiskFreeMB = float64(st.Bavail) * float64(st.Bsize) / 1e6
	}
	if info, err := js.AccountInfo(ctx); err == nil {
		l.StoreUsedMB = float64(info.Store) / 1e6
		l.MemUsedMB = float64(info.Memory) / 1e6
		l.StoreLimitMB = float64(storeLimit) / 1e6
		l.MemLimitMB = float64(memLimit) / 1e6
		if info.Limits.MaxStore > 0 && float64(info.Limits.MaxStore)/1e6 < l.StoreLimitMB {
			l.StoreLimitMB = float64(info.Limits.MaxStore) / 1e6
		}
		if info.Limits.MaxMemory > 0 && float64(info.Limits.MaxMemory)/1e6 < l.MemLimitMB {
			l.MemLimitMB = float64(info.Limits.MaxMemory) / 1e6
		}
		l.Streams = info.Streams
		l.Consumers = info.Consumers
	}
	return l
}

func cpuTime() time.Duration {
	var ru syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &ru) != nil {
		return 0
	}
	return time.Duration(ru.Utime.Sec)*time.Second + time.Duration(ru.Utime.Usec)*time.Microsecond +
		time.Duration(ru.Stime.Sec)*time.Second + time.Duration(ru.Stime.Usec)*time.Microsecond
}

// openFiles counts this process's descriptors. /dev/fd cannot be listed on
// macOS, so probe each descriptor with fcntl(F_GETFD) up to the soft limit.
func openFiles() int {
	var rl syscall.Rlimit
	limit := uint64(65536)
	if syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rl) == nil && rl.Cur < limit {
		limit = uint64(rl.Cur)
	}
	n := 0
	for fd := uintptr(0); fd < uintptr(limit); fd++ {
		if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, fd, syscall.F_GETFD, 0); errno == 0 {
			n++
		}
	}
	return n
}
