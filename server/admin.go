package server

import (
	"fmt"
	"net/http"
	_ "net/http/pprof"
	"runtime"
	"sync/atomic"
	"time"
)

// The admin listener is separate from the content port and bound to localhost.
//
// Two reasons it cannot live on the content port: Files is keyed by request URL,
// so a PUT to /-/healthz would shadow the endpoint; and pprof must never be
// reachable from the internet.
//
// Localhost is sufficient because everything that consumes this runs on the
// instance. Nothing external needs it.

var (
	startedAt = time.Now()

	ingestBytesTotal atomic.Int64
	lastIngestUnix   atomic.Int64

	readersActive   atomic.Int64
	waitingRequests atomic.Int64

	// PutAbortedTotal counts ingests that ended before the body was complete.
	PutAbortedTotal atomic.Int64

	buildSHA = "unknown"
)

func noteIngest(n int) {
	ingestBytesTotal.Add(int64(n))
	lastIngestUnix.Store(time.Now().Unix())
}

// StartAdminListener serves health, metrics and pprof on addr. A blank addr
// disables it.
func StartAdminListener(addr string, sha string) {
	if addr == "" {
		logInfof("admin listener disabled")
		return
	}
	if sha != "" {
		buildSHA = sha
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/-/healthz", healthzHandler)
	mux.HandleFunc("/-/metrics", metricsHandler)
	// pprof registers on DefaultServeMux; forward to it rather than re-registering.
	mux.Handle("/debug/pprof/", http.DefaultServeMux)

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		logInfof("admin listener on %s serving /-/healthz /-/metrics /debug/pprof", addr)
		if err := srv.ListenAndServe(); err != nil {
			logErrorf("admin listener stopped: %v", err)
		}
	}()
}

// healthzHandler reports whether this process can serve.
//
// It deliberately does NOT fail on stale ingest. A watchdog restarts the service
// when this endpoint stops responding, which is what catches a hang; if staleness
// returned 503, a legitimately ended event would put the service into a restart
// loop. Ingest staleness is reported as data for monitoring instead.
func healthzHandler(w http.ResponseWriter, r *http.Request) {
	FilesLock.RLock()
	files := len(Files)
	FilesLock.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprintf(w, `{"ok":true,"git_sha":%q,"uptime_s":%d,"files":%d,"heap_bytes":%d,`+
		`"last_ingest_age_s":%d,"readers":%d,"put_aborted":%d,"goroutines":%d}`+"\n",
		buildSHA,
		int64(time.Since(startedAt).Seconds()),
		files,
		heapBytes(),
		lastIngestAgeSeconds(),
		readersActive.Load(),
		PutAbortedTotal.Load(),
		runtime.NumGoroutine(),
	)
}

func lastIngestAgeSeconds() int64 {
	last := lastIngestUnix.Load()
	if last == 0 {
		return -1
	}

	return time.Now().Unix() - last
}

// heapBytes reports live heap from the runtime rather than a hand-maintained
// counter. An earlier build tracked content bytes by incrementing on write and
// decrementing on release; the release path was missed on one route and the
// figure drifted to 60x the true value, which made it useless as an alarm. The
// runtime cannot drift.
func heapBytes() int64 {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	return int64(ms.HeapAlloc)
}

// metricsHandler emits Prometheus text format by hand, to keep the dependency
// tree at two modules for a reproducible offline build.
func metricsHandler(w http.ResponseWriter, r *http.Request) {
	FilesLock.RLock()
	files := len(Files)
	FilesLock.RUnlock()

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

	type m struct {
		name, kind, help string
		value            int64
	}
	for _, x := range []m{
		{"gcss_up", "gauge", "1 while the process is serving", 1},
		{"gcss_uptime_seconds", "gauge", "Seconds since start", int64(time.Since(startedAt).Seconds())},
		{"gcss_files", "gauge", "Files currently held", int64(files)},
		{"gcss_ingest_bytes_total", "counter", "Bytes received from the encoder", ingestBytesTotal.Load()},
		{"gcss_last_ingest_age_seconds", "gauge", "Seconds since the last ingested byte; -1 if none yet", lastIngestAgeSeconds()},
		{"gcss_readers_active", "gauge", "In-flight GETs streaming from memory", readersActive.Load()},
		{"gcss_waiting_requests", "gauge", "GETs held waiting for data to arrive", waitingRequests.Load()},
		{"gcss_swept_total", "counter", "Files reclaimed by the idle sweeper; the encoder should be deleting most segments itself", SweptTotal.Load()},
		{"gcss_put_aborted_total", "counter", "Ingests that ended before the body completed, and were discarded", PutAbortedTotal.Load()},
		// The two figures worth alarming on. Heap should plateau once the encoder's
		// DELETEs balance its PUTs; goroutines should plateau at a low number.
		// Sustained growth in either is the shape every leak in this server took.
		{"go_memstats_heap_alloc_bytes", "gauge", "Live heap; should plateau, not climb", int64(ms.HeapAlloc)},
		{"go_memstats_sys_bytes", "gauge", "Total memory obtained from the OS", int64(ms.Sys)},
		{"go_goroutines", "gauge", "Goroutines; sustained growth means readers or ingests are leaking", int64(runtime.NumGoroutine())},
	} {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n%s %d\n", x.name, x.help, x.name, x.kind, x.name, x.value)
	}

	fmt.Fprintf(w, "# HELP gcss_build_info Build identity\n# TYPE gcss_build_info gauge\ngcss_build_info{git_sha=%q} 1\n", buildSHA)
}
