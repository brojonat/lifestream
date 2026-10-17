package metrics

import (
	"fmt"
	"net/http"
	"runtime"
)

// Handler serves the collector's counters and current process stats in the
// Prometheus text exposition format, with no client library.
func Handler(c *Collector) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqs, errs := c.Requests()
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		fmt.Fprintf(w, "# HELP app_requests_total HTTP requests served.\n# TYPE app_requests_total counter\napp_requests_total %d\n", reqs)
		fmt.Fprintf(w, "# HELP app_request_errors_total HTTP responses with status >= 500.\n# TYPE app_request_errors_total counter\napp_request_errors_total %d\n", errs)
		fmt.Fprintf(w, "# HELP app_open_streams Open SSE connections.\n# TYPE app_open_streams gauge\napp_open_streams %d\n", c.OpenStreams())
		fmt.Fprintf(w, "# HELP app_uptime_seconds Seconds since start.\n# TYPE app_uptime_seconds gauge\napp_uptime_seconds %.0f\n", c.Uptime().Seconds())
		fmt.Fprintf(w, "# HELP go_goroutines Number of goroutines.\n# TYPE go_goroutines gauge\ngo_goroutines %d\n", runtime.NumGoroutine())
		fmt.Fprintf(w, "# HELP go_memstats_heap_alloc_bytes Heap bytes allocated and in use.\n# TYPE go_memstats_heap_alloc_bytes gauge\ngo_memstats_heap_alloc_bytes %d\n", ms.HeapAlloc)
		fmt.Fprintf(w, "# HELP go_memstats_sys_bytes Bytes obtained from the OS.\n# TYPE go_memstats_sys_bytes gauge\ngo_memstats_sys_bytes %d\n", ms.Sys)
		fmt.Fprintf(w, "# HELP app_open_files Open file descriptors.\n# TYPE app_open_files gauge\napp_open_files %d\n", openFiles())
	})
}
