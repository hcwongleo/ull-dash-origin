package server

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const timeURL = "https://time.example/?iso"

func withUTC(t *testing.T, url string) {
	t.Helper()
	prev := utcTiming()
	SetUTCTiming(url)
	t.Cleanup(func() { SetUTCTiming(prev) })
}

func TestInjectUTCTimingBeforeFirstPeriod(t *testing.T) {
	withUTC(t, timeURL)

	in := []byte(`<?xml version="1.0"?><MPD type="dynamic"><Period id="0"><AdaptationSet/></Period></MPD>`)
	out := injectUTCTiming(in)

	if !bytes.Contains(out, []byte(`value="`+timeURL+`"`)) {
		t.Fatalf("UTCTiming not injected:\n%s", out)
	}
	if !bytes.Contains(out, []byte(defaultUTCTimingScheme)) {
		t.Errorf("scheme missing:\n%s", out)
	}
	// Schema order: UTCTiming is an MPD child and must precede Period.
	if bytes.Index(out, []byte("UTCTiming")) > bytes.Index(out, []byte("<Period")) {
		t.Errorf("UTCTiming placed after Period:\n%s", out)
	}
	// Nothing from the original may be lost.
	if !bytes.Contains(out, []byte(`<AdaptationSet/>`)) || !bytes.HasSuffix(out, []byte("</MPD>")) {
		t.Errorf("original content damaged:\n%s", out)
	}
}

// Re-injecting on every manifest update would accumulate elements without bound;
// the encoder PUTs a new manifest roughly every second.
func TestInjectUTCTimingIsIdempotent(t *testing.T) {
	withUTC(t, timeURL)

	in := []byte(`<MPD type="dynamic"><Period/></MPD>`)
	once := injectUTCTiming(in)
	twice := injectUTCTiming(once)

	if !bytes.Equal(once, twice) {
		t.Errorf("second pass changed the manifest:\n%s", twice)
	}
	if n := bytes.Count(twice, []byte("UTCTiming")); n != 1 {
		t.Errorf("UTCTiming appears %d times, want 1", n)
	}
}

// An encoder or edge function that already supplies timing wins. Two timing
// sources is worse than one, and the operator chose the other.
func TestExistingUTCTimingIsLeftAlone(t *testing.T) {
	withUTC(t, timeURL)

	in := []byte(`<MPD><UTCTiming schemeIdUri="x" value="https://other/"/><Period/></MPD>`)
	if out := injectUTCTiming(in); !bytes.Equal(in, out) {
		t.Errorf("overwrote an existing UTCTiming:\n%s", out)
	}
}

func TestInjectUTCTimingFailsOpen(t *testing.T) {
	withUTC(t, timeURL)

	for name, in := range map[string]string{
		"not xml":        "this is not a manifest at all",
		"no MPD element": `<?xml version="1.0"?><Other><Period/></Other>`,
		"empty":          "",
		"truncated MPD":  `<MPD type="dynamic"><Peri`,
	} {
		if out := injectUTCTiming([]byte(in)); string(out) != in {
			t.Errorf("%s: input was modified\n  in:  %q\n  out: %q", name, in, out)
		}
	}
}

func TestUTCTimingDisabledByEmptyURL(t *testing.T) {
	withUTC(t, "")

	in := []byte(`<MPD><Period/></MPD>`)
	if out := injectUTCTiming(in); !bytes.Equal(in, out) {
		t.Errorf("injected while disabled:\n%s", out)
	}
}

// The load-bearing guarantee: a media segment must never be buffered, because
// buffering it would defeat read-while-write. Only *.mpd may be rewritten.
func TestOnlyManifestsAreRewritten(t *testing.T) {
	cases := map[string]bool{
		"/live.mpd":             true,
		"/a/b/c.MPD":            true,
		"/live-v-fhd_001.mp4":   false,
		"/live-v-fhd_001.m4s":   false,
		"/liveinit.mp4":         false,
		"/notamanifest.mpd.m4s": false,
		"/live.mpd.tmp":         false,
	}
	for name, want := range cases {
		if got := isManifest(name); got != want {
			t.Errorf("isManifest(%q) = %v, want %v", name, got, want)
		}
	}
}

// End to end through the handler: a PUT manifest comes back out with timing, and
// a PUT segment comes back byte-identical.
func TestPutManifestGainsTimingAndSegmentIsUntouched(t *testing.T) {
	resetFiles(t)
	withUTC(t, timeURL)

	cors := NewCors()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			GetHandler(nil, cors, "", w, r)
		default:
			PostHandler(nil, true, cors, "", w, r)
		}
	}))
	defer srv.Close()

	manifest := `<?xml version="1.0"?><MPD type="dynamic"><Period id="0"/></MPD>`
	segment := "\x00\x00\x00\x18ftypmp42binary\xff\xfe payload"

	for path, payload := range map[string]string{
		"/live.mpd":     manifest,
		"/live_001.m4s": segment,
	} {
		req, _ := http.NewRequest(http.MethodPut, srv.URL+path, strings.NewReader(payload))
		req.Header.Set("Content-Type", "application/octet-stream")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("PUT %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("PUT %s: status %d", path, resp.StatusCode)
		}
	}

	get := func(path string) string {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)

		return string(b)
	}

	gotManifest := get("/live.mpd")
	if !strings.Contains(gotManifest, timeURL) {
		t.Errorf("stored manifest has no UTCTiming:\n%s", gotManifest)
	}
	if !strings.Contains(gotManifest, `<Period id="0"/>`) {
		t.Errorf("stored manifest lost content:\n%s", gotManifest)
	}

	if got := get("/live_001.m4s"); got != segment {
		t.Errorf("segment was altered\n  want %q\n  got  %q", segment, got)
	}
}

// A body larger than the cap must be stored whole and unmodified, not truncated
// at the cap and not held in memory beyond it.
func TestOversizedManifestIsStoredUntruncated(t *testing.T) {
	resetFiles(t)
	withUTC(t, timeURL)

	big := "<MPD>" + strings.Repeat("x", manifestSizeCap+4096) + "</MPD>"

	f := NewFile("/big.mpd", http.Header{}, -1)
	n, err := copyManifest(f, strings.NewReader(big))
	if err != nil {
		t.Fatalf("copyManifest: %v", err)
	}
	if int(n) != len(big) {
		t.Errorf("wrote %d bytes, want %d: body was truncated", n, len(big))
	}

	f.lock.RLock()
	stored := string(f.buffer)
	f.lock.RUnlock()
	if stored != big {
		t.Errorf("stored %d bytes, want %d, and content must be unmodified", len(stored), len(big))
	}
	if strings.Contains(stored, "UTCTiming") {
		t.Error("an oversized body should be passed through, not rewritten")
	}
}

func TestManifestReadErrorIsReported(t *testing.T) {
	resetFiles(t)
	withUTC(t, timeURL)

	f := NewFile("/broken.mpd", http.Header{}, -1)
	want := fmt.Errorf("connection reset")
	r := io.MultiReader(strings.NewReader("<MPD>partial"), errReader{want})

	if _, err := copyManifest(f, r); err == nil {
		t.Fatal("a read failure must be reported so the ingest is discarded")
	}
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }
