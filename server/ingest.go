package server

import (
	"io"
	"net/http"
	"sync/atomic"
	"time"
)

// Ingest bodies get an idle deadline that is extended on every successful read.
//
// A blanket http.Server ReadTimeout is the wrong tool: it bounds the whole
// request, so it would abort a legitimate long chunked PUT. What needs bounding
// is silence, not duration.
//
// Without this, a half-open ingest connection — a dead encoder host, a NAT idle
// timeout, a severed network path with no FIN — leaves the PUT goroutine blocked
// in io.Copy on a socket that will never deliver another byte or close. Retention
// reclaims the file, but the goroutine, the socket and its fd are held forever.
// Measured on the deployed origin: 25 such connections cost 25 goroutines and
// 25 fds, still present 95 s later with the files already reclaimed. There is no
// upper bound, so over months of continuous operation this ends in fd exhaustion.
var ingestIdleNanos atomic.Int64

// SetIngestIdleTimeout configures how long an ingest body may be silent before it
// is aborted. Zero disables the deadline.
func SetIngestIdleTimeout(d time.Duration) {
	ingestIdleNanos.Store(int64(d))
}

func ingestIdleTimeout() time.Duration {
	return time.Duration(ingestIdleNanos.Load())
}

// idleTimeoutBody resets the connection's read deadline before each read, so the
// deadline measures the gap between bytes rather than the life of the request.
type idleTimeoutBody struct {
	body io.ReadCloser
	rc   *http.ResponseController
	idle time.Duration
}

func (b *idleTimeoutBody) Read(p []byte) (int, error) {
	// An error here means the connection does not support deadlines; read anyway
	// rather than failing the ingest.
	_ = b.rc.SetReadDeadline(time.Now().Add(b.idle))

	return b.body.Read(p)
}

func (b *idleTimeoutBody) Close() error {
	return b.body.Close()
}

// withIdleDeadline wraps an ingest body when a timeout is configured.
func withIdleDeadline(w http.ResponseWriter, r *http.Request) io.ReadCloser {
	idle := ingestIdleTimeout()
	if idle <= 0 {
		return r.Body
	}

	return &idleTimeoutBody{
		body: r.Body,
		rc:   http.NewResponseController(w),
		idle: idle,
	}
}
