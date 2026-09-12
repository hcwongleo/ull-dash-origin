package server

import (
	"bytes"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
)

// Persisting initialisation segments across a restart.
//
// WHY
//
// Everything this origin holds is in RAM, and Elemental Live sends initialisation
// segments ONLY when an output group starts. So any restart - a crash, a deploy, an
// AWS host event, an automatic recovery - loses them, and no encoder resends them
// unprompted. Viewers already watching keep playing because they fetched init long
// ago; only NEW viewers fail, which is why it looks healthy for hours. This caused
// two outages during development.
//
// It also undoes the value of every automatic recovery in the system: systemd
// restarts a crash in 2s, the watchdog catches a hang in a minute, AWS recovers a
// failed host by itself - and all three then hand back a stream that will not accept
// new viewers until somebody restarts the encoder.
//
// Initialisation segments are a few hundred bytes and arrive once per encoder run,
// so writing them to disk costs nothing on the hot path. Media segments stay in RAM
// untouched: writing those would add a disk write per segment for no benefit.
//
// SECURITY
//
// The filename comes from an UNAUTHENTICATED request URL, and this is the only code
// in the server that turns a request into a filesystem path. filepath.Join cleans a
// path but will happily resolve outside its base:
//
//	filepath.Join("/opt/gochunked/content", "/../../etc/cron.d/x") -> "/etc/cron.d/x"
//
// systemd's ProtectSystem=strict would block that write today, but that protection
// lives in the unit file, not the program - run the binary by hand and it is gone.
// So the name is percent-encoded to a single flat filename with no separators, and
// the resolved path is checked to be inside the directory regardless.

const (
	// An initialisation segment is a moov box - measured at ~900 bytes on this
	// stream. 16 MB is far above anything real, and deliberately so: this cap exists
	// only to refuse something that is obviously not an initialisation segment, not
	// to police size. A large ladder, a codec with a fat decoder config, or an
	// encoder that pads the box should never be rejected by a number I guessed.
	maxPersistBytes = 16 << 20

	// A ceiling on files, so an unauthenticated writer cannot fill the disk one
	// init-shaped name at a time. When full the OLDEST is evicted, never the new
	// write refused: with several channels on one origin each run leaves ~3 files, so
	// refusing the newest would deny protection to the channel currently on air while
	// keeping stale files from runs that ended weeks ago.
	//
	// 4096 is generous on purpose. A live channel needs 3 files; everything else on
	// disk is from finished runs and is worthless. Since eviction is oldest-first the
	// number only has to comfortably exceed 3 x channels, and being stingy would risk
	// evicting something still in use for no gain - the realistic footprint is a few
	// megabytes on a 30 GB volume.
	maxPersistFiles = 4096

	// The cap that actually protects the disk. The per-file and per-count limits are
	// deliberately generous so they never reject or evict something real, but
	// generosity multiplies: 4096 files at 16 MB each would be 68 GB on a 30 GB
	// volume. Bounding the AGGREGATE is what makes the other two safe to be loose.
	//
	// 512 MB is ~1.7% of the volume and ~138x the realistic footprint of 3.7 MB.
	maxPersistTotalBytes = 512 << 20

	persistSubdir = "init"
)

var persistDir atomic.Value // string; empty disables

// SetPersistDir enables persistence of initialisation segments under dir. An empty
// dir disables it.
func SetPersistDir(dir string) {
	if dir == "" {
		persistDir.Store("")

		return
	}
	persistDir.Store(filepath.Join(dir, persistSubdir))
}

func persistPath() string {
	s, _ := persistDir.Load().(string)

	return s
}

// encodeName turns a request URL into one flat, filesystem-safe filename.
//
// Percent-encoding rather than replacing separators: it is reversible, so the
// original URL can be recovered exactly on reload, and it cannot produce a path
// separator, a "..", or a leading dot.
func encodeName(name string) string {
	var b strings.Builder
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.':
			// A leading dot would make a hidden file, and ".." must never appear.
			if c == '.' && b.Len() == 0 {
				b.WriteString("%2E")

				continue
			}
			b.WriteByte(c)
		default:
			b.WriteString("%")
			const hex = "0123456789ABCDEF"
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0x0f])
		}
	}

	return b.String()
}

func decodeName(enc string) (string, bool) {
	var b strings.Builder
	for i := 0; i < len(enc); i++ {
		if enc[i] != '%' {
			b.WriteByte(enc[i])

			continue
		}
		if i+2 >= len(enc) {
			return "", false
		}
		var v byte
		for _, c := range []byte{enc[i+1], enc[i+2]} {
			v <<= 4
			switch {
			case c >= '0' && c <= '9':
				v |= c - '0'
			case c >= 'A' && c <= 'F':
				v |= c - 'A' + 10
			default:
				return "", false
			}
		}
		b.WriteByte(v)
		i += 2
	}

	return b.String(), true
}

// safePath resolves the on-disk path for a stored name and refuses anything that
// escapes the directory. Belt and braces over encodeName, which cannot produce a
// separator - this is the check that still holds if encodeName is ever changed.
func safePath(dir, encoded string) (string, bool) {
	if encoded == "" || strings.ContainsAny(encoded, `/\`) {
		return "", false
	}
	full := filepath.Join(dir, encoded)
	rel, err := filepath.Rel(dir, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}

	return full, true
}

// PersistInit writes an initialisation segment so it survives a restart.
//
// Stored HTTP-style: header lines, a blank line, then the body. One file rather than
// a sidecar, and self-describing, so a human can see what it is.
func PersistInit(name string, headers http.Header, body []byte) {
	dir := persistPath()
	if dir == "" || !IsInit(name) {
		return
	}
	if len(body) == 0 || len(body) > maxPersistBytes {
		logWarnf("not persisting %s: %d bytes is not an initialisation segment", name, len(body))

		return
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		logErrorf("cannot create %s: %v", dir, err)

		return
	}

	enc := encodeName(name)
	full, ok := safePath(dir, enc)
	if !ok {
		logWarnf("refusing to persist %s: unsafe name", name)

		return
	}

	// Only touch the ceiling when adding something new, so re-PUTs of the same init
	// segment always pass straight through.
	if _, err := os.Stat(full); os.IsNotExist(err) {
		evictOldest(dir, maxPersistFiles-1, maxPersistTotalBytes-int64(len(body)))
	}

	var buf bytes.Buffer
	if ct := headers.Get("Content-Type"); ct != "" {
		buf.WriteString("Content-Type: " + ct + "\n")
	}
	buf.WriteString("\n")
	buf.Write(body)

	// Write then rename, so a restart can never read a half-written file.
	tmp := full + ".tmp"
	if err := ioutil.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		logErrorf("persisting %s: %v", name, err)

		return
	}
	if err := os.Rename(tmp, full); err != nil {
		logErrorf("persisting %s: %v", name, err)
		os.Remove(tmp)

		return
	}

	logInfof("persisted initialisation segment %s (%d bytes)", name, len(body))
}

// evictOldest removes the oldest files until both budgets are satisfied, so a new
// write always has room.
//
// Cost is O(files) per NEW file, because it reads the directory. That is acceptable
// only because new initialisation segments are rare - three per encoder run, not per
// segment - so this runs a handful of times a day, not 1.5 times a second. A re-PUT
// of an existing init segment skips it entirely.
//
// Oldest-first by modification time, because a file from a finished encoder run is
// worthless while the one being written now is exactly what a new viewer needs after
// a restart. Both budgets matter: the file count stops an unbounded number of tiny
// files, and the byte budget stops a small number of large ones.
func evictOldest(dir string, keepFiles int, keepBytes int64) {
	entries, err := ioutil.ReadDir(dir)
	if err != nil {
		return
	}

	files := make([]os.FileInfo, 0, len(entries))
	var total int64
	for _, e := range entries {
		if !e.IsDir() {
			files = append(files, e)
			total += e.Size()
		}
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].ModTime().Before(files[j].ModTime())
	})

	for _, e := range files {
		if len(files) <= keepFiles && total <= keepBytes {
			return
		}
		full, ok := safePath(dir, e.Name())
		if !ok {
			continue
		}
		if err := os.Remove(full); err != nil {
			continue
		}
		total -= e.Size()
		files = files[1:]
		logWarnf("evicted persisted %s (%d bytes) to stay within %d files / %d MB; it is from an older encoder run",
			e.Name(), e.Size(), maxPersistFiles, maxPersistTotalBytes>>20)
	}
}

// LoadPersisted restores initialisation segments into memory at startup and reports
// how many were loaded.
//
// Every failure is skipped rather than fatal: a corrupt or unreadable file must not
// stop the origin from serving. The worst outcome is the state we are trying to fix.
func LoadPersisted() int {
	dir := persistPath()
	if dir == "" {
		return 0
	}

	entries, err := ioutil.ReadDir(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			logErrorf("reading %s: %v", dir, err)
		}

		return 0
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && !strings.HasSuffix(e.Name(), ".tmp") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	loaded := 0
	for _, enc := range names {
		full, ok := safePath(dir, enc)
		if !ok {
			continue
		}
		raw, err := ioutil.ReadFile(full)
		if err != nil {
			logErrorf("reading %s: %v", full, err)

			continue
		}
		name, ok := decodeName(enc)
		if !ok || !IsInit(name) {
			logWarnf("ignoring unrecognised persisted file %s", enc)

			continue
		}

		headers := http.Header{}
		body := raw
		if i := bytes.Index(raw, []byte("\n\n")); i >= 0 {
			for _, line := range strings.Split(string(raw[:i]), "\n") {
				if k, v, found := cut(line, ": "); found {
					headers.Set(k, v)
				}
			}
			body = raw[i+2:]
		}

		f := NewFile(name, headers, -1)
		f.Write(body)
		f.Close()

		FilesLock.Lock()
		Files[name] = f
		FilesLock.Unlock()
		loaded++
	}

	if loaded > 0 {
		log.Printf("restored %d initialisation segment(s) from disk; viewers can start immediately", loaded)
	}

	return loaded
}

// cut is strings.Cut, which needs go1.18; go.mod declares 1.13.
func cut(s, sep string) (before, after string, found bool) {
	if i := strings.Index(s, sep); i >= 0 {
		return s[:i], s[i+len(sep):], true
	}

	return s, "", false
}
