package server

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// An ingest aborted by the idle deadline leaves a truncated segment in the map.
// Serving that as if it were complete hands players corrupt media, which is
// worse than a 404: a 404 the encoder or player retries, bad bytes it does not.
func TestAbortedIngestIsDiscardedNotServed(t *testing.T) {
	resetFiles(t)
	SetIngestIdleTimeout(200 * time.Millisecond)
	defer SetIngestIdleTimeout(0)

	before := PutAbortedTotal.Load()

	cors := NewCors()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		PostHandler(nil, true, cors, "", w, r)
	}))
	defer srv.Close()

	c, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	// Headers plus a partial body, then silence. The segment is never terminated.
	fmt.Fprintf(c, "PUT /truncated.m4s HTTP/1.1\r\nHost: x\r\n"+
		"Content-Type: video/mp4\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nAAAAA\r\n")

	// Wait for the ingest to register first, or "absent" would pass trivially
	// before the handler had even parsed the request.
	appeared := time.Now().Add(2 * time.Second)
	for time.Now().Before(appeared) {
		FilesLock.RLock()
		_, present := Files["/truncated.m4s"]
		FilesLock.RUnlock()

		if present {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	FilesLock.RLock()
	_, started := Files["/truncated.m4s"]
	FilesLock.RUnlock()
	if !started {
		t.Skip("ingest never registered; environment too slow to observe")
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		FilesLock.RLock()
		_, present := Files["/truncated.m4s"]
		FilesLock.RUnlock()

		if !present {
			if got := PutAbortedTotal.Load(); got != before+1 {
				t.Errorf("PutAbortedTotal = %d, want %d: the abort was not counted", got, before+1)
			}

			return
		}
		time.Sleep(50 * time.Millisecond)
	}

	t.Fatal("a truncated segment is still being served after its ingest was aborted")
}

// readersActive drives the only gauge that would reveal leaking readers, so a
// reader that finishes must decrement it. An always-zero or ever-climbing gauge
// is worse than none: it is the figure an operator would trust.
func TestReadersActiveReturnsToZero(t *testing.T) {
	resetFiles(t)

	f := NewFile("/complete.m4s", http.Header{}, -1)
	f.Write([]byte("payload"))
	f.Close()

	FilesLock.Lock()
	Files["/complete.m4s"] = f
	FilesLock.Unlock()

	start := readersActive.Load()

	cors := NewCors()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		GetHandler(nil, cors, "", w, r)
	}))
	defer srv.Close()

	for i := 0; i < 5; i++ {
		resp, err := http.Get(srv.URL + "/complete.m4s")
		if err != nil {
			t.Fatalf("get %d: %v", i, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if string(body) != "payload" {
			t.Fatalf("got %q, want %q", body, "payload")
		}
	}

	// The handler decrements on return, which can trail the client by a moment.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if readersActive.Load() == start {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}

	t.Errorf("readersActive = %d after 5 completed GETs, want %d: readers are leaking",
		readersActive.Load(), start)
}
