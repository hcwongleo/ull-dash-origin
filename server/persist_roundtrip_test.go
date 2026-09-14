package server

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// A minimal but structurally real initialisation segment: 4-byte box size then the
// ftyp box type, which is what every ISO-BMFF file starts with.
var initSegmentBytes = []byte("\x00\x00\x00\x1cftypiso6\x00\x00\x02\x00moov-ish-bytes")

// The encoder's PUT does not always carry Content-Type, and when it does not, the
// persisted file used to be written with a lone "\n" separator instead of a blank
// line. LoadPersisted looks for "\n\n" to find where the body starts, so it found
// nothing and took the whole file as the body - one byte too long.
//
// The result was an init segment beginning "\n" then ftyp, which no player can
// parse: the decoder never initialises and the viewer sees a black screen while the
// manifest, every media segment and every HTTP status stay perfectly healthy.
// Measured on the deployed origin 2026-09-14: 705 bytes served where the encoder
// had sent 704, ftyp at offset 5, and 22 hours of dark channel.
func TestInitSurvivesARestartWhenThePutHadNoContentType(t *testing.T) {
	resetFiles(t)
	withPersist(t)

	const name = "/ch18live-v-fhd_20260914T101010init.mp4"

	// http.Header{} deliberately: no Content-Type, exactly as the encoder sent it.
	PersistInit(name, http.Header{}, initSegmentBytes)

	FilesLock.Lock()
	Files = map[string]*File{}
	FilesLock.Unlock()

	if n := LoadPersisted(); n != 1 {
		t.Fatalf("LoadPersisted() = %d, want 1", n)
	}

	FilesLock.RLock()
	f, ok := Files[name]
	FilesLock.RUnlock()
	if !ok {
		t.Fatal("segment not restored under its original URL")
	}

	f.lock.RLock()
	got := string(f.buffer)
	f.lock.RUnlock()

	if got != string(initSegmentBytes) {
		t.Errorf("body changed across the round trip, so players get unplayable media:\n"+
			"  want %d bytes starting %q\n  got  %d bytes starting %q",
			len(initSegmentBytes), initSegmentBytes[:8], len(got), got[:min(8, len(got))])
	}
}

// Belt and braces for the files already on disk in the field, written by the old
// code. Their body cannot be recovered, so the only safe thing is to refuse them:
// a 404 is recoverable and trips the existing runbook, whereas a shifted init
// segment is served with status 200 and looks healthy from every angle.
func TestCorruptPersistedInitIsRefusedRatherThanServed(t *testing.T) {
	resetFiles(t)
	dir := withPersist(t)

	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	const name = "/ch18live-v-fhd_20260914T101010init.mp4"
	// Exactly what the old writer produced for a PUT with no Content-Type.
	legacy := append([]byte("\n"), initSegmentBytes...)
	if err := os.WriteFile(filepath.Join(dir, encodeName(name)), legacy, 0o644); err != nil {
		t.Fatal(err)
	}

	if n := LoadPersisted(); n != 0 {
		t.Errorf("LoadPersisted() = %d, want 0: a byte-shifted initialisation segment "+
			"plays as a black screen while every status stays 200, so it must be "+
			"refused rather than served", n)
	}

	FilesLock.RLock()
	_, present := Files[name]
	FilesLock.RUnlock()
	if present {
		t.Error("corrupt segment was registered and would be served to viewers")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}

	return b
}
