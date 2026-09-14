package server

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func withPersist(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	prev, _ := persistDir.Load().(string)
	SetPersistDir(dir)
	if err := SetInitPattern(DefaultInitPattern); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { persistDir.Store(prev) })

	return filepath.Join(dir, persistSubdir)
}

// The whole point: a restart must not lose the segments only NEW viewers need.
func TestInitSegmentSurvivesARestart(t *testing.T) {
	resetFiles(t)
	dir := withPersist(t)

	const name = "/ch18live-v-fhd_20260913T101010init.mp4"
	body := []byte("\x00\x00\x00\x18ftypiso6moov-ish-bytes")
	h := http.Header{}
	h.Set("Content-Type", "video/mp4")

	PersistInit(name, h, body)

	// Simulate the restart: memory is empty, disk is not.
	FilesLock.Lock()
	Files = map[string]*File{}
	FilesLock.Unlock()

	if n := LoadPersisted(); n != 1 {
		t.Fatalf("LoadPersisted() = %d, want 1 — a restart would still lose it", n)
	}

	FilesLock.RLock()
	f, ok := Files[name]
	FilesLock.RUnlock()
	if !ok {
		t.Fatal("segment not restored under its original URL")
	}

	f.lock.RLock()
	got, eof := string(f.buffer), f.eof
	f.lock.RUnlock()

	if got != string(body) {
		t.Errorf("body changed across the round trip:\n  want %q\n  got  %q", body, got)
	}
	if !eof {
		t.Error("restored segment is not marked complete, so readers would block waiting for a writer that will never come")
	}
	if ct := f.headers.Get("Content-Type"); ct != "video/mp4" {
		t.Errorf("Content-Type = %q, want video/mp4", ct)
	}
	_ = dir
}

// The filename comes from an UNAUTHENTICATED request URL. This is the only place in
// the server that turns a request into a filesystem path.
func TestPersistRefusesPathTraversal(t *testing.T) {
	resetFiles(t)
	dir := withPersist(t)

	for _, name := range []string{
		"/../../../etc/cron.d/evil_init.mp4",
		"/..%2F..%2Fetc/x_init.mp4",
		"//etc/passwd_init.mp4",
		"/a/b/../../../../tmp/pwned_init.mp4",
	} {
		PersistInit(name, http.Header{}, []byte("payload"))
	}

	// Nothing may exist outside the directory, and nothing inside may contain a
	// separator or a parent reference.
	entries, err := ioutil.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	for _, e := range entries {
		// A literal ".." inside a name is harmless: what makes traversal possible is
		// a path SEPARATOR, and every one is percent-encoded. The dangerous cases are
		// a separator anywhere, or a name that is exactly "." or "..".
		if strings.ContainsAny(e.Name(), `/\`) {
			t.Errorf("wrote a filename containing a path separator: %q", e.Name())
		}
		if e.Name() == "." || e.Name() == ".." {
			t.Errorf("wrote a parent/current directory reference: %q", e.Name())
		}
		full := filepath.Join(dir, e.Name())
		abs, _ := filepath.Abs(full)
		absDir, _ := filepath.Abs(dir)
		if !strings.HasPrefix(abs, absDir+string(filepath.Separator)) {
			t.Errorf("%q resolves outside %q", abs, absDir)
		}
	}

	for _, escaped := range []string{"/etc/cron.d/evil_init.mp4", "/tmp/pwned_init.mp4"} {
		if _, err := os.Stat(escaped); err == nil {
			t.Fatalf("PATH TRAVERSAL: wrote %s outside the content directory", escaped)
		}
	}
}

// Media segments must never touch the disk: that is a write per segment for no
// benefit, and it is the difference between this being cheap and being a cost.
func TestOnlyInitSegmentsArePersisted(t *testing.T) {
	resetFiles(t)
	dir := withPersist(t)

	for _, name := range []string{
		"/ch18live-v_000000123.mp4",
		"/ch18live.mpd",
		"/something.m4s",
	} {
		PersistInit(name, http.Header{}, []byte("payload"))
	}

	entries, err := ioutil.ReadDir(dir)
	if err == nil && len(entries) != 0 {
		var got []string
		for _, e := range entries {
			got = append(got, e.Name())
		}
		t.Errorf("persisted non-init files: %v", got)
	}
}

func TestPersistRefusesOversizedBodies(t *testing.T) {
	resetFiles(t)
	dir := withPersist(t)

	PersistInit("/big_init.mp4", http.Header{}, make([]byte, maxPersistBytes+1))
	PersistInit("/empty_init.mp4", http.Header{}, nil)

	if entries, err := ioutil.ReadDir(dir); err == nil && len(entries) != 0 {
		t.Errorf("persisted %d files, want 0: neither is an initialisation segment", len(entries))
	}
}

// The newest write must always succeed and the oldest must be the one to go: with
// several channels on one origin, the newest is the channel currently on air while
// the oldest is from a run that already ended.
//
// Exercises evictOldest directly with small budgets rather than writing thousands of
// files, because the real ceiling is 4096 and doing that took 40 seconds for no extra
// confidence.
func TestEvictionTakesTheOldestFirst(t *testing.T) {
	dir := t.TempDir()

	for i := 0; i < 6; i++ {
		name := filepath.Join(dir, fmt.Sprintf("f%d", i))
		if err := ioutil.WriteFile(name, []byte("xxxx"), 0o644); err != nil {
			t.Fatal(err)
		}
		// Distinct modification times, oldest first.
		mt := time.Now().Add(time.Duration(i-10) * time.Minute)
		if err := os.Chtimes(name, mt, mt); err != nil {
			t.Fatal(err)
		}
	}

	evictOldest(dir, 3, 1<<30) // keep 3 files, bytes not binding

	entries, err := ioutil.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("left %d files, want 3", len(entries))
	}
	got := map[string]bool{}
	for _, e := range entries {
		got[e.Name()] = true
	}
	for _, kept := range []string{"f3", "f4", "f5"} {
		if !got[kept] {
			t.Errorf("%s was evicted; it is newer than the ones kept", kept)
		}
	}
	for _, gone := range []string{"f0", "f1", "f2"} {
		if got[gone] {
			t.Errorf("%s survived; eviction is not oldest-first", gone)
		}
	}
}

// And the byte budget evicts too, not just the count.
func TestEvictionHonoursTheByteBudget(t *testing.T) {
	dir := t.TempDir()

	for i := 0; i < 5; i++ {
		name := filepath.Join(dir, fmt.Sprintf("b%d", i))
		if err := ioutil.WriteFile(name, make([]byte, 1000), 0o644); err != nil {
			t.Fatal(err)
		}
		mt := time.Now().Add(time.Duration(i-10) * time.Minute)
		if err := os.Chtimes(name, mt, mt); err != nil {
			t.Fatal(err)
		}
	}

	evictOldest(dir, 1000, 2500) // count not binding; 2500 bytes allows 2 files

	entries, _ := ioutil.ReadDir(dir)
	var total int64
	for _, e := range entries {
		total += e.Size()
	}
	if total > 2500 {
		t.Errorf("left %d bytes, budget was 2500", total)
	}
	if len(entries) == 0 {
		t.Error("evicted everything; it should stop as soon as the budget is met")
	}
}

// The per-file and per-count caps are deliberately loose, so the aggregate cap is
// what actually protects the disk. 4096 files at the 16 MB per-file cap would be
// 68 GB on a 30 GB volume.
func TestPersistBoundsTotalBytesNotJustFileCount(t *testing.T) {
	resetFiles(t)
	dir := withPersist(t)

	// Well under the file ceiling, but each file is large.
	big := make([]byte, 8<<20) // 8 MB
	for i := 0; i < 90; i++ {  // 720 MB if nothing were evicted
		PersistInit(fmt.Sprintf("/big%d-v_init.mp4", i), http.Header{}, big)
	}

	entries, err := ioutil.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, e := range entries {
		total += e.Size()
	}
	if total > maxPersistTotalBytes {
		t.Errorf("holding %d MB, aggregate cap is %d MB - the disk is not bounded",
			total>>20, maxPersistTotalBytes>>20)
	}
	if len(entries) > maxPersistFiles {
		t.Errorf("holding %d files, ceiling is %d", len(entries), maxPersistFiles)
	}

	// The newest must still be there: evicting what is currently on air to satisfy a
	// budget would defeat the point.
	last := fmt.Sprintf("/big%d-v_init.mp4", 89)
	if _, err := os.Stat(filepath.Join(dir, encodeName(last))); err != nil {
		t.Errorf("the newest write was evicted or refused: %v", err)
	}
}

// Several channels on one origin must not collide.
func TestMultipleChannelsCoexist(t *testing.T) {
	resetFiles(t)
	withPersist(t)

	names := []string{
		"/ch18live-v-fhd_20260913T101010init.mp4",
		"/ch19live-v-fhd_20260913T101500init.mp4",
		"/ch18_live/nested/ch20live-a1_init.m4s",
	}
	for i, n := range names {
		h := http.Header{}
		h.Set("Content-Type", "video/mp4")
		// Must open with an ftyp box: LoadPersisted refuses anything that is not a
		// real initialisation segment. The tail stays per-channel so this still
		// detects channels sharing a file.
		PersistInit(n, h, []byte(fmt.Sprintf("\x00\x00\x00\x1cftypiso6moov-%d", i)))
	}

	FilesLock.Lock()
	Files = map[string]*File{}
	FilesLock.Unlock()

	if n := LoadPersisted(); n != len(names) {
		t.Fatalf("restored %d of %d channels", n, len(names))
	}
	for i, n := range names {
		FilesLock.RLock()
		f, ok := Files[n]
		FilesLock.RUnlock()
		if !ok {
			t.Errorf("%s not restored", n)

			continue
		}
		f.lock.RLock()
		got := string(f.buffer)
		f.lock.RUnlock()
		if want := fmt.Sprintf("\x00\x00\x00\x1cftypiso6moov-%d", i); got != want {
			t.Errorf("%s has %q, want %q - channels are sharing a file", n, got, want)
		}
	}
}

// A corrupt or foreign file on disk must be skipped, never fatal — the origin has to
// come up regardless.
func TestLoadSkipsUnreadableFilesWithoutFailing(t *testing.T) {
	resetFiles(t)
	dir := withPersist(t)

	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Garbage, a name that decodes to nothing useful, and a partial write.
	for name, content := range map[string]string{
		"not-a-valid-name":                    "junk",
		encodeName("/good_init.mp4"):          "Content-Type: video/mp4\n\n\x00\x00\x00\x1cftypiso6real",
		encodeName("/half_init.mp4") + ".tmp": "half written",
		"%ZZbad_init.mp4":                     "bad encoding",
	} {
		if err := ioutil.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	n := LoadPersisted()
	if n != 1 {
		t.Errorf("loaded %d, want exactly 1 (the valid file); the rest must be skipped, not fatal", n)
	}

	FilesLock.RLock()
	_, ok := Files["/good_init.mp4"]
	FilesLock.RUnlock()
	if !ok {
		t.Error("the one valid file was not loaded")
	}
}

func TestNameEncodingRoundTrips(t *testing.T) {
	// A name that is exactly "." or ".." must never come out of the encoder, whatever
	// goes in.
	for _, in := range []string{".", "..", "/", "/..", "./.."} {
		if enc := encodeName(in); enc == "." || enc == ".." || strings.ContainsAny(enc, `/\`) {
			t.Errorf("encodeName(%q) = %q is a dangerous filename", in, enc)
		}
	}

	for _, name := range []string{
		"/ch18live-v-fhd_20260913T101010init.mp4",
		"/ch18_live/nested/path-v_init.m4s",
		"/../../etc/passwd_init.mp4",
		"/spaces and %25 signs_init.mp4",
	} {
		enc := encodeName(name)
		if strings.ContainsAny(enc, `/\`) {
			t.Errorf("encodeName(%q) = %q contains a path separator", name, enc)
		}
		if strings.HasPrefix(enc, ".") {
			t.Errorf("encodeName(%q) = %q starts with a dot", name, enc)
		}
		got, ok := decodeName(enc)
		if !ok || got != name {
			t.Errorf("round trip failed: %q -> %q -> %q (ok=%v)", name, enc, got, ok)
		}
	}
}

func TestPersistDisabledWritesNothing(t *testing.T) {
	resetFiles(t)
	dir := t.TempDir()
	prev, _ := persistDir.Load().(string)
	SetPersistDir("")
	t.Cleanup(func() { persistDir.Store(prev) })

	PersistInit("/x_init.mp4", http.Header{}, []byte("moov"))

	if entries, err := ioutil.ReadDir(filepath.Join(dir, persistSubdir)); err == nil && len(entries) > 0 {
		t.Error("wrote files while persistence is disabled")
	}
	if n := LoadPersisted(); n != 0 {
		t.Errorf("LoadPersisted() = %d while disabled, want 0", n)
	}
}
