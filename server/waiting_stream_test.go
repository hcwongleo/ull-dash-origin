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

// A GET that arrives before the segment exists is parked by -w rather than
// refused, because the manifest's availabilityTimeOffset invited it. The whole
// point of parking it is to hand it into read-while-write the moment the encoder
// starts writing - the -wait-timeout-ms flag documents itself as "a ceiling and
// not added latency".
//
// Measured against the deployed origin before this test existed: a GET placed
// ~0.4s early received nothing for 2.3s and then the entire segment at once,
// because the waiters were only released after the PUT had finished. That is one
// whole segment of dead air for any player whose clock runs slightly fast, and
// it defeats read-while-write for exactly the requests the manifest invited.
func TestWaitingGetStreamsFirstChunkBeforeIngestCompletes(t *testing.T) {
	resetFiles(t)

	prev := requestExpirationMs.Load()
	t.Cleanup(func() { requestExpirationMs.Store(prev) })
	SetWaitTimeout(5 * time.Second)

	prevIdle := ingestIdleTimeout()
	t.Cleanup(func() { SetIngestIdleTimeout(prevIdle) })
	SetIngestIdleTimeout(10 * time.Second)

	cors := NewCors()
	waiting := NewWaitingRequests()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			PostHandler(waiting, true, cors, "", w, r)
			return
		}
		GetHandler(waiting, cors, "", w, r)
	}))
	defer srv.Close()

	type result struct {
		first []byte
		err   error
	}
	got := make(chan result, 1)

	// The GET goes first, while the segment does not exist, so it parks.
	go func() {
		resp, err := http.Get(srv.URL + "/seg.m4s")
		if err != nil {
			got <- result{err: err}
			return
		}
		defer resp.Body.Close()
		buf := make([]byte, 5)
		n, err := io.ReadFull(resp.Body, buf)
		got <- result{first: buf[:n], err: err}
	}()

	// Give it time to park before any ingest exists.
	time.Sleep(250 * time.Millisecond)

	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// One chunk, then hold the body open: the segment is now being written but is
	// nowhere near complete, which is precisely the live-edge case.
	if _, err := fmt.Fprint(conn, "PUT /seg.m4s HTTP/1.1\r\nHost: x\r\n"+
		"Content-Type: video/mp4\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nAAAAA\r\n"); err != nil {
		t.Fatalf("writing first chunk: %v", err)
	}

	select {
	case r := <-got:
		if r.err != nil {
			t.Fatalf("parked GET failed: %v", r.err)
		}
		if string(r.first) != "AAAAA" {
			t.Errorf("parked GET read %q, want the first ingested chunk %q", r.first, "AAAAA")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("parked GET received nothing 2s after the first chunk was ingested: " +
			"waiters are being released on PUT completion instead of on first data, " +
			"so an early request gets a full segment of dead air instead of read-while-write")
	}

	// Let the ingest finish so the handler returns cleanly.
	fmt.Fprint(conn, "0\r\n\r\n")
}
