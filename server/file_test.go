package server

import (
	"io"
	"net/http"
	"testing"
	"time"
)

// A reader that has drained the buffer of a file still being written must block
// until more bytes arrive, not return (0, nil). Returning (0, nil) makes
// io.Copy retry immediately, which spins a full CPU core per reader.
func TestReadBlocksUntilWrite(t *testing.T) {
	f := NewFile("/seg.m4s", http.Header{}, 10)
	rc := f.NewReadCloser("", nil)

	type result struct {
		n   int
		err error
	}
	done := make(chan result, 1)
	go func() {
		buf := make([]byte, 32)
		n, err := rc.Read(buf)
		done <- result{n, err}
	}()

	select {
	case r := <-done:
		t.Fatalf("Read returned (%d, %v) on an empty in-progress file; it must block", r.n, r.err)
	case <-time.After(100 * time.Millisecond):
	}

	f.Write([]byte("abc"))

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("Read after Write returned error %v", r.err)
		}
		if r.n != 3 {
			t.Fatalf("Read after Write returned %d bytes, want 3", r.n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Read did not wake after Write")
	}
}

// Close must release a blocked reader with io.EOF.
func TestReadWakesOnCloseWithEOF(t *testing.T) {
	f := NewFile("/seg.m4s", http.Header{}, 10)
	rc := f.NewReadCloser("", nil)

	done := make(chan error, 1)
	go func() {
		buf := make([]byte, 32)
		_, err := rc.Read(buf)
		done <- err
	}()

	time.Sleep(50 * time.Millisecond)
	f.Close()

	select {
	case err := <-done:
		if err != io.EOF {
			t.Fatalf("Read after Close returned %v, want io.EOF", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Read did not wake after Close")
	}
}

// Draining a file that is written slowly must not produce any (0, nil) returns.
// Each one is an immediate io.Copy retry, i.e. a busy loop.
func TestReadNeverReturnsZeroNil(t *testing.T) {
	f := NewFile("/seg.m4s", http.Header{}, 10)
	rc := f.NewReadCloser("", nil)

	go func() {
		for i := 0; i < 5; i++ {
			time.Sleep(20 * time.Millisecond)
			f.Write([]byte("0123456789"))
		}
		f.Close()
	}()

	buf := make([]byte, 4)
	total, zeroNil := 0, 0
	for {
		n, err := rc.Read(buf)
		if n == 0 && err == nil {
			zeroNil++
			if zeroNil > 1000 {
				t.Fatal("Read is busy-looping: over 1000 (0, nil) returns")
			}
		}
		total += n
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("unexpected error %v", err)
		}
	}
	if zeroNil != 0 {
		t.Errorf("Read returned (0, nil) %d times; want 0", zeroNil)
	}
	if total != 50 {
		t.Errorf("read %d bytes, want 50", total)
	}
}
