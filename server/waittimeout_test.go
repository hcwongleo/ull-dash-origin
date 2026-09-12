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

// The default must exceed a typical availabilityTimeOffset of 1.8s. If someone
// lowers it below that in future, this test is the thing that objects.
func TestDefaultWaitTimeoutExceedsTypicalAvailabilityTimeOffset(t *testing.T) {
	const typicalATO = 1800 * time.Millisecond

	if defaultRequestExpirationFallback <= typicalATO {
		t.Fatalf("default hold is %v, which does not exceed a typical availabilityTimeOffset of %v: "+
			"players accepting the manifest's invitation would be stalled and then refused",
			defaultRequestExpirationFallback, typicalATO)
	}
}
