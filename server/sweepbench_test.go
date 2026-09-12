package server

import (
	"net/http"
	"testing"
	"time"
)

func seed(n int, ageOffset time.Duration) {
	FilesLock.Lock()
	Files = map[string]*File{}
	FilesLock.Unlock()
	base := time.Now().Add(ageOffset)
	for i := 0; i < n; i++ {
		f := NewFile("/seg_"+time.Duration(i).String()+".m4s", http.Header{}, -1)
		f.receivedAt = base
		f.Write([]byte("payload"))
		f.Close()
		FilesLock.Lock()
		Files[f.Name] = f
		FilesLock.Unlock()
	}
}

// The realistic steady state: Elemental deletes its own window, so the sweep
// finds NOTHING eligible every time it runs.
func BenchmarkSweepNothingEligible100(b *testing.B) {
	SetIdleSweep(3600 * time.Second)
	SetInitPattern(DefaultInitPattern)
	seed(100, -10*time.Second)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sweepOnce(time.Now())
	}
}

func BenchmarkSweepNothingEligible10000(b *testing.B) {
	SetIdleSweep(3600 * time.Second)
	SetInitPattern(DefaultInitPattern)
	seed(10000, -10*time.Second)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sweepOnce(time.Now())
	}
}

// Worst case: everything is eligible, so it also does the delete + Discard work.
func BenchmarkSweepAllEligible1000(b *testing.B) {
	SetIdleSweep(1 * time.Second)
	SetInitPattern(DefaultInitPattern)
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		seed(1000, -2*time.Hour)
		b.StartTimer()
		sweepOnce(time.Now())
	}
}

// Does a sweep in progress slow down ingest? Measures PUT-path map writes while
// a sweep runs concurrently.
func BenchmarkIngestDuringSweep(b *testing.B) {
	SetIdleSweep(3600 * time.Second)
	SetInitPattern(DefaultInitPattern)
	seed(500, -10*time.Second)
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				sweepOnce(time.Now())
			}
		}
	}()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f := NewFile("/hot.m4s", http.Header{}, -1)
		f.Write([]byte("chunk"))
		f.Close()
		FilesLock.Lock()
		Files["/hot.m4s"] = f
		FilesLock.Unlock()
	}
	b.StopTimer()
	close(stop)
}
