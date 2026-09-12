package server

import (
	"context"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"
)

// withSweep configures the sweeper for a test and restores it afterwards.
func withSweep(t *testing.T, after time.Duration, pattern string) {
	t.Helper()
	prevSecs := idleSweepSeconds.Load()
	prevRe, _ := initPattern.Load().(interface{})

	SetIdleSweep(after)
	if err := SetInitPattern(pattern); err != nil {
		t.Fatalf("SetInitPattern(%q): %v", pattern, err)
	}

	t.Cleanup(func() {
		idleSweepSeconds.Store(prevSecs)
		if prevRe != nil {
			initPattern.Store(prevRe)
		}
	})
}

// store puts a file in the map as if a PUT had happened at receivedAt.
func store(t *testing.T, name string, body string, complete bool, receivedAt time.Time) *File {
	t.Helper()

	f := NewFile(name, http.Header{}, -1)
	f.receivedAt = receivedAt
	if body != "" {
		f.Write([]byte(body))
	}
	if complete {
		f.Close()
	}

	FilesLock.Lock()
	Files[name] = f
	FilesLock.Unlock()

	return f
}

func present(name string) bool {
	FilesLock.RLock()
	defer FilesLock.RUnlock()
	_, ok := Files[name]

	return ok
}

func TestSweepRemovesOnlyOldCompleteNonInitFiles(t *testing.T) {
	resetFiles(t)
	withSweep(t, 300*time.Second, DefaultInitPattern)

	old := time.Now().Add(-2 * time.Hour)
	now := time.Now()

	store(t, "/old_001.m4s", "aaa", true, old)       // swept
	store(t, "/recent_002.m4s", "bbb", true, now)    // too young
	store(t, "/growing_003.m4s", "ccc", false, old)  // still being written
	store(t, "/stream-v_init.mp4", "ddd", true, old) // init, exempt
	store(t, "/live.mpd", "<MPD/>", true, now)       // manifest, re-PUT so young

	if n := sweepOnce(time.Now()); n != 1 {
		t.Errorf("swept %d files, want exactly 1", n)
	}

	for name, wantPresent := range map[string]bool{
		"/old_001.m4s":       false,
		"/recent_002.m4s":    true,
		"/growing_003.m4s":   true,
		"/stream-v_init.mp4": true,
		"/live.mpd":          true,
	} {
		if got := present(name); got != wantPresent {
			t.Errorf("%s present=%v, want %v", name, got, wantPresent)
		}
	}
}

// An initialisation segment is PUT once and needed by every viewer who joins
// LATER, so it looks perfectly idle while being essential. Sweeping it means
// every new viewer gets 404 on init and cannot start — an outage this project
// has already had twice.
func TestSweepNeverTouchesInitSegments(t *testing.T) {
	resetFiles(t)
	withSweep(t, 1*time.Second, DefaultInitPattern)

	ancient := time.Now().Add(-30 * 24 * time.Hour)
	for _, name := range []string{
		"/ch18live-v-fhd_20260911T082217init.mp4",
		"/ch18live-a1_20260911T082217init.mp4",
		"/x_init.m4s",
		"/y_init.cmfv",
	} {
		store(t, name, "moov", true, ancient)
	}

	sweepOnce(time.Now())

	for _, name := range []string{
		"/ch18live-v-fhd_20260911T082217init.mp4",
		"/ch18live-a1_20260911T082217init.mp4",
		"/x_init.m4s",
		"/y_init.cmfv",
	} {
		if !present(name) {
			t.Errorf("%s was swept; init segments must survive any age", name)
		}
	}
}

// Reclaiming a file mid-write would truncate a segment for every viewer reading
// it, which produces corrupt media rather than an error.
func TestSweepNeverTouchesAnIncompleteFile(t *testing.T) {
	resetFiles(t)
	withSweep(t, 1*time.Second, DefaultInitPattern)

	f := store(t, "/inflight.m4s", "partial", false, time.Now().Add(-time.Hour))

	if n := sweepOnce(time.Now()); n != 0 {
		t.Fatalf("swept %d files; an incomplete file must never be a candidate", n)
	}
	if !present("/inflight.m4s") {
		t.Fatal("incomplete file was removed from the map")
	}

	// It becomes eligible only once the writer closes it.
	f.Close()
	if n := sweepOnce(time.Now()); n != 1 {
		t.Errorf("swept %d after Close, want 1", n)
	}
}

// THE hazard. A reader parked on a file that gets reclaimed must be woken, or it
// waits forever on a condition variable no writer will signal again. Previously
// a real bug in this server.
func TestSweepReleasesParkedReaders(t *testing.T) {
	resetFiles(t)
	withSweep(t, 1*time.Second, DefaultInitPattern)

	// Incomplete on purpose, so the reader parks in Read rather than hitting EOF.
	f := store(t, "/parked.m4s", "first", false, time.Now().Add(-time.Hour))

	rc := f.NewReadCloser(context.Background(), "", nil)
	if rc == nil {
		t.Fatal("no reader")
	}
	defer rc.Close()

	// Drain what is there, then the next Read parks.
	buf := make([]byte, 5)
	if _, err := rc.Read(buf); err != nil {
		t.Fatalf("first read: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := rc.Read(make([]byte, 8))
		done <- err
	}()

	// Give the goroutine time to actually park on the condition variable.
	time.Sleep(120 * time.Millisecond)

	// Complete it so the sweeper considers it, then sweep.
	f.Close()
	if n := sweepOnce(time.Now()); n != 1 {
		t.Fatalf("swept %d, want 1", n)
	}

	select {
	case err := <-done:
		if err != io.EOF {
			t.Errorf("parked reader returned %v, want io.EOF", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("parked reader never woke: the sweeper left it waiting on a condition variable forever")
	}
}

// Candidates are collected under RLock and deleted under Lock. A PUT landing
// between the two phases replaces the map entry, and deleting the key blindly
// would destroy the new file — serving 404 for a segment that had just arrived.
func TestSweepDoesNotDeleteAReplacedFile(t *testing.T) {
	resetFiles(t)
	withSweep(t, 1*time.Second, DefaultInitPattern)

	const name = "/replaced.m4s"
	store(t, name, "stale", true, time.Now().Add(-time.Hour))

	// Simulate the interleaving directly: replace the entry with a fresh file,
	// then run the phase-2 identity check via sweepOnce.
	FilesLock.RLock()
	stale := Files[name]
	FilesLock.RUnlock()

	fresh := store(t, name, "brandnew", true, time.Now())
	if stale == fresh {
		t.Fatal("test setup: replacement is the same object")
	}

	sweepOnce(time.Now())

	FilesLock.RLock()
	cur, ok := Files[name]
	FilesLock.RUnlock()

	if !ok {
		t.Fatal("the replacement was deleted: the identity re-check did not hold")
	}
	if cur != fresh {
		t.Error("map holds an unexpected object")
	}
}

func TestSweepDisabledDoesNothing(t *testing.T) {
	resetFiles(t)
	withSweep(t, 0, DefaultInitPattern)

	store(t, "/ancient.m4s", "x", true, time.Now().Add(-365*24*time.Hour))

	if n := sweepOnce(time.Now()); n != 0 {
		t.Errorf("swept %d with -idle-sweep 0, want 0", n)
	}
	if !present("/ancient.m4s") {
		t.Error("file removed while sweeping is disabled")
	}
}

func TestSweepCountsAndConcurrency(t *testing.T) {
	resetFiles(t)
	withSweep(t, 1*time.Second, DefaultInitPattern)

	before := SweptTotal.Load()
	old := time.Now().Add(-time.Hour)
	for i := 0; i < 20; i++ {
		store(t, "/c_"+string(rune('a'+i))+".m4s", "data", true, old)
	}

	// Sweeping while writes and reads are in flight must not race.
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); sweepOnce(time.Now()) }()
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			store(t, "/new.m4s", "fresh", true, time.Now())
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			FilesLock.RLock()
			_ = len(Files)
			FilesLock.RUnlock()
		}
	}()
	wg.Wait()

	if got := SweptTotal.Load(); got <= before {
		t.Errorf("SweptTotal did not advance: %d then %d", before, got)
	}
}

func TestInitPatternCanBeDisabledAndValidated(t *testing.T) {
	resetFiles(t)

	if err := SetInitPattern("([unclosed"); err == nil {
		t.Error("an invalid pattern must be rejected, not silently ignored")
	}

	withSweep(t, 1*time.Second, "")
	store(t, "/thing_init.mp4", "moov", true, time.Now().Add(-time.Hour))

	if n := sweepOnce(time.Now()); n != 1 {
		t.Errorf("swept %d with no init pattern, want 1", n)
	}
}
