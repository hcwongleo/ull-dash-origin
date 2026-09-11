package server

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"
)

// A half-open ingest — headers and a partial body sent, then silence with the
// socket still open — must be aborted rather than pinning the handler goroutine
// and its socket forever. Measured on the deployed origin before this existed:
// 25 such connections cost 25 goroutines and 25 fds, permanently.
func TestHalfOpenIngestIsAborted(t *testing.T) {
	resetFiles(t)
	SetIngestIdleTimeout(300 * time.Millisecond)
	defer SetIngestIdleTimeout(0)

	cors := NewCors()
	retention := Retention{AgeDefaultS: 120, AgeFloorS: 120, AgeCapS: 300}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		PostHandler(nil, true, cors, "", retention, w, r)
	}))
	defer srv.Close()

	addr := srv.Listener.Addr().String()
	before := runtime.NumGoroutine()

	conns := make([]net.Conn, 0, 8)
	for i := 0; i < 8; i++ {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		// Headers plus one chunk, then deliberate silence. The socket stays open.
		fmt.Fprintf(c, "PUT /half_%d.m4s HTTP/1.1\r\nHost: x\r\n"+
			"Content-Type: video/mp4\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nAAAAA\r\n", i)
		conns = append(conns, c)
	}
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()

	time.Sleep(150 * time.Millisecond)
	if got := runtime.NumGoroutine(); got <= before {
		t.Skip("handlers did not start; environment too slow to observe")
	}

	// Well past the idle timeout, every handler must have returned.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= before+2 {
			return // aborted and reaped
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("goroutines still elevated (%d vs %d before): half-open ingests were not aborted",
		runtime.NumGoroutine(), before)
}

// The deadline must bound silence, not total duration, or a legitimate long
// chunked PUT would be killed mid-segment.
func TestSlowButSteadyIngestIsNotAborted(t *testing.T) {
	resetFiles(t)
	SetIngestIdleTimeout(300 * time.Millisecond)
	defer SetIngestIdleTimeout(0)

	cors := NewCors()
	retention := Retention{AgeDefaultS: 120, AgeFloorS: 120, AgeCapS: 300}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		PostHandler(nil, true, cors, "", retention, w, r)
	}))
	defer srv.Close()

	c, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	fmt.Fprintf(c, "PUT /slow.m4s HTTP/1.1\r\nHost: x\r\n"+
		"Content-Type: video/mp4\r\nTransfer-Encoding: chunked\r\n\r\n")
	// 10 chunks at 100ms: 1s total, well over the 300ms idle timeout, but never
	// silent for that long.
	for i := 0; i < 10; i++ {
		if _, err := fmt.Fprintf(c, "5\r\nAAAAA\r\n"); err != nil {
			t.Fatalf("write chunk %d: %v", i, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	fmt.Fprintf(c, "0\r\n\r\n")

	time.Sleep(300 * time.Millisecond)
	FilesLock.RLock()
	f, ok := Files["/slow.m4s"]
	FilesLock.RUnlock()
	if !ok {
		t.Fatal("a steadily-fed slow ingest was dropped; the deadline is bounding duration, not silence")
	}
	if got := f.Bytes(); got != 50 {
		t.Errorf("stored %d bytes, want 50 — body was truncated", got)
	}
}
