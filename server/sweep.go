package server

import (
	"log"
	"regexp"
	"sync/atomic"
	"time"
)

// Idle sweep: reclaim files nothing can plausibly still want.
//
// WHY THIS EXISTS
//
// Files live in a Go map, and a map entry IS a reference — the garbage collector
// will never free them however old they are. Verified: four test objects were
// still resident after 18.9 hours, including across a forced GC.
//
// Elemental Live deletes its own segments as they fall out of the manifest
// window: 27,507 DELETEs measured in one five-hour session. So the normal path
// needs nothing from us. Two cases are not covered:
//
//   1. An output that stops. The DELETE for segment N is triggered by segment
//      N+6 arriving, so when the encoder stops, the last window's worth never
//      gets deleted — about 6.4 MB, plus that run's initialisation segments,
//      whose names carry a per-run token and so are never revisited.
//
//   2. DELETEs stopping altogether — a reconfigured output, a different encoder,
//      a bug. Growth then runs at the full ingest rate, ~1.8 GB/hour, and the
//      process is killed in about two hours. The kill compounds: RAM is emptied,
//      the initialisation segments go with it, and new viewers get 404 until
//      somebody restarts the encoder. A silent config change becomes an outage.
//
// Case 1 alone would not justify code: any origin restart clears it, and it
// would take roughly 640 encoder restarts to exhaust memory. Case 2 is the one
// worth defending against, and the same sweep covers both — only the threshold
// differs.
//
// WHAT THIS IS NOT
//
// Not a DVR window. An earlier build implemented retention as a window sized
// against the MPD's timeShiftBufferDepth, and it deleted the origin's own
// manifest two seconds after ingest because the encoder correctly sends
// Cache-Control: max-age=2 on it. That coupling is the thing to avoid: the
// default here is 300x a typical 12 s window, so it is a garbage collector with
// an enormous margin rather than a window that has to be tuned against the
// encoder.

const DefaultInitPattern = `init\.(mp4|m4s|cmfv|cmfa|cmft)$`

var (
	// idleSweepSeconds is 0 when sweeping is disabled.
	idleSweepSeconds atomic.Int64
	initPattern      atomic.Value // *regexp.Regexp

	// SweptTotal counts files reclaimed by the sweeper.
	SweptTotal atomic.Int64

	sweepStop = make(chan struct{})
)

// SetIdleSweep configures the age after which a complete, non-initialisation
// file is reclaimed. Zero disables sweeping.
func SetIdleSweep(d time.Duration) {
	idleSweepSeconds.Store(int64(d.Seconds()))
}

// SetInitPattern configures which names are exempt from sweeping. An empty
// expression disables the exemption, which is almost never what you want: an
// initialisation segment is PUT once and needed by every LATER viewer for the
// life of the stream, so it looks perfectly idle while being essential.
func SetInitPattern(expr string) error {
	if expr == "" {
		initPattern.Store((*regexp.Regexp)(nil))

		return nil
	}

	re, err := regexp.Compile(expr)
	if err != nil {
		return err
	}
	initPattern.Store(re)

	return nil
}

// IsInit reports whether a name is exempt from sweeping.
func IsInit(name string) bool {
	re, _ := initPattern.Load().(*regexp.Regexp)

	return re != nil && re.MatchString(name)
}

// StartSweeper runs the sweep on a ticker until StopSweeper is called. A zero
// interval or a disabled threshold makes it a no-op.
func StartSweeper(interval time.Duration) {
	if idleSweepSeconds.Load() <= 0 {
		logWarnf("idle sweep disabled: files the encoder never deletes will accumulate until the process restarts")

		return
	}
	if interval <= 0 {
		interval = time.Minute
	}

	log.Printf("idle sweep: reclaiming complete non-init files after %ds, checked every %s",
		idleSweepSeconds.Load(), interval)

	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case now := <-t.C:
				sweepOnce(now)
			case <-sweepStop:
				return
			}
		}
	}()
}

// StopSweeper halts the ticker. Safe to call once.
func StopSweeper() {
	select {
	case <-sweepStop:
	default:
		close(sweepStop)
	}
}

// sweepOnce reclaims eligible files and returns how many were removed.
//
// Three phases, and the split is not cosmetic. Each guards against a bug this
// server has actually had:
//
//  1. collect under RLock          — cheap, does not block ingest
//  2. delete under Lock, RE-CHECKING IDENTITY — a PUT between phases replaces
//     the map entry, and deleting the key blindly would destroy the new file
//  3. Discard outside FilesLock    — a reader parked on a reclaimed file must be
//     woken, or it waits forever on a condition variable nobody will signal.
//     Done after the map delete so no new reader can attach to a doomed file,
//     and outside the global lock so a broadcast never blocks every other request
func sweepOnce(now time.Time) int {
	maxAge := idleSweepSeconds.Load()
	if maxAge <= 0 {
		return 0
	}
	cutoff := now.Add(-time.Duration(maxAge) * time.Second)

	type candidate struct {
		key  string
		file *File
	}

	var candidates []candidate

	FilesLock.RLock()
	for key, f := range Files {
		if IsInit(key) {
			continue
		}

		f.lock.RLock()
		// A file still being written is never eligible, however old its entry
		// looks: reclaiming it would truncate a segment mid-playback.
		eligible := f.eof && f.receivedAt.Before(cutoff)
		f.lock.RUnlock()

		if eligible {
			candidates = append(candidates, candidate{key, f})
		}
	}
	FilesLock.RUnlock()

	if len(candidates) == 0 {
		return 0
	}

	swept := make([]candidate, 0, len(candidates))

	FilesLock.Lock()
	for _, c := range candidates {
		if cur, ok := Files[c.key]; ok && cur == c.file {
			delete(Files, c.key)
			swept = append(swept, c)
		}
	}
	FilesLock.Unlock()

	for _, c := range swept {
		c.file.Discard()
		logInfof("swept %s, idle longer than %ds", c.key, maxAge)
	}

	SweptTotal.Add(int64(len(swept)))

	return len(swept)
}
