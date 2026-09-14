package server

import (
	"testing"
	"time"
)

// Upstream hardcoded 1000ms. Against an availabilityTimeOffset of 1.800 that
// stalled players for ~1.2s and then refused them, and every retry moved the
// player's position - the fluctuating live latency seen in production. The value
// must therefore be configurable, and the default must be generous.
func TestWaitTimeoutIsConfigurable(t *testing.T) {
	prev := requestExpirationMs.Load()
	t.Cleanup(func() { requestExpirationMs.Store(prev) })

	SetWaitTimeout(2500 * time.Millisecond)
	if got := requestExpiration(); got != 2500*time.Millisecond {
		t.Errorf("requestExpiration() = %v, want 2.5s", got)
	}

	// Zero and negative fall back rather than becoming an instant 404.
	for _, d := range []time.Duration{0, -1 * time.Second} {
		SetWaitTimeout(d)
		if got := requestExpiration(); got != defaultRequestExpirationFallback {
			t.Errorf("SetWaitTimeout(%v) gave %v, want the fallback %v",
				d, got, defaultRequestExpirationFallback)
		}
	}
}

// The ceiling has to sit between two measured facts, and the old rule - "exceed
// availabilityTimeOffset" - is no longer one of them.
//
// That rule was correct only while waiters were released at PUT completion: a hold
// then had to outlast the whole segment. Since waiters are released on the FIRST
// chunk, a hold only has to outlast the wait for that chunk, measured at 360-540ms
// on the deployed origin.
//
// The upper bound is what the old rule got wrong. A ceiling longer than one segment
// means a stalled encoder pins every arriving request for longer than the interval
// at which new ones arrive, so held requests accumulate across segment boundaries
// instead of draining - each one holding a goroutine, a socket and a CloudFront
// connection.
func TestDefaultWaitTimeoutSitsBetweenFirstChunkLatencyAndSegmentDuration(t *testing.T) {
	const (
		measuredFirstChunkHold = 540 * time.Millisecond
		segmentDuration        = 2000 * time.Millisecond
		wantMargin             = 300 * time.Millisecond
	)

	if defaultRequestExpirationFallback < measuredFirstChunkHold+wantMargin {
		t.Errorf("default hold is %v, too close to the measured %v wait for a first chunk: "+
			"requests the manifest invited would be refused whenever the encoder jitters",
			defaultRequestExpirationFallback, measuredFirstChunkHold)
	}

	if defaultRequestExpirationFallback >= segmentDuration {
		t.Errorf("default hold is %v, which is not shorter than one %v segment: with a stalled "+
			"encoder, held requests would arrive faster than they drain and pile up",
			defaultRequestExpirationFallback, segmentDuration)
	}
}
