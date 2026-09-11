package server

import (
	"bytes"
	"strings"
	"sync/atomic"
)

// UTCTiming injection.
//
// A DASH player needs a trusted clock to work out where the live edge is. With
// no UTCTiming element it falls back to the local device clock, and a set-top box
// or phone whose clock is a few seconds out will request segments that do not
// exist yet, or that have already gone. Observed during testing as dash.js
// Error 16 (TIME_SYNC_FAILED) followed by a 6 second gap jump.
//
// Elemental Live does not emit UTCTiming. On the CDN-ingest pipeline a
// Lambda@Edge added it as the manifest passed through; direct ingest has no edge
// function, so the origin does it.
//
// WHY ONLY UTCTiming
//
// The Lambda also injected a ServiceDescription carrying Latency@target. That is
// deliberately NOT done here. It caused a real problem: the injected 4000ms
// disagreed with the encoder's own suggestedPresentationDelay of PT3S, and a
// player given two different targets hunts between them. The encoder stays the
// single source of truth for latency. UTCTiming is safe to add because it is a
// constant, not a value derived from the encoder's timing configuration - so it
// cannot fall out of step when the encoder is reconfigured.
//
// Deliberately string manipulation, not an XML parse: a quirky declaration,
// namespace or comment can never turn a working manifest into a broken one, and
// the transformation is auditable by reading it.

const defaultUTCTimingScheme = "urn:mpeg:dash:utc:http-iso:2014"

// manifestSizeCap bounds how much of a .mpd PUT is buffered for rewriting. A
// live manifest is a few kilobytes; anything beyond this is not a manifest we
// should be editing, so it is streamed through untouched rather than held in
// memory.
const manifestSizeCap = 1 << 20

var utcTimingValue atomic.Value // string

// SetUTCTiming configures the URL injected as the UTCTiming value. An empty
// string disables injection entirely.
func SetUTCTiming(url string) {
	utcTimingValue.Store(url)
}

func utcTiming() string {
	if v, ok := utcTimingValue.Load().(string); ok {
		return v
	}

	return ""
}

// isManifest reports whether a stored object is a DASH manifest, and so a
// candidate for rewriting. Name-based, because the encoder's Content-Type is not
// dependable and a false positive on a media segment would be far worse than a
// missed manifest: buffering a segment would destroy read-while-write.
func isManifest(name string) bool {
	return strings.HasSuffix(strings.ToLower(name), ".mpd")
}

// injectUTCTiming returns body with a UTCTiming element added before the first
// Period, or unchanged if it already has one, is not a recognisable MPD, or
// injection is disabled.
//
// Idempotent and fail-open by construction: every path that is not clearly safe
// returns the input untouched. The worst outcome is an un-enriched manifest,
// never a corrupt one.
func injectUTCTiming(body []byte) []byte {
	url := utcTiming()
	if url == "" {
		return body
	}
	if !bytes.Contains(body, []byte("<MPD")) {
		return body
	}
	// Already present, from the encoder or from an edge function. Leave it: two
	// timing sources is worse than one, and the operator chose the other.
	if bytes.Contains(body, []byte("UTCTiming")) {
		return body
	}

	element := []byte(`<UTCTiming schemeIdUri="` + defaultUTCTimingScheme +
		`" value="` + url + `"/>`)

	// Schema order puts UTCTiming among the MPD's children; before the first
	// Period is where the reference Lambda placed it and where dash.js expects
	// to find it. Players read it regardless of sibling order, but staying
	// schema-valid keeps stricter players happy.
	if i := bytes.Index(body, []byte("<Period")); i >= 0 {
		out := make([]byte, 0, len(body)+len(element)+4)
		out = append(out, body[:i]...)
		out = append(out, element...)
		out = append(out, '\n', ' ', ' ')
		out = append(out, body[i:]...)

		return out
	}

	if i := bytes.Index(body, []byte("</MPD>")); i >= 0 {
		out := make([]byte, 0, len(body)+len(element)+4)
		out = append(out, body[:i]...)
		out = append(out, element...)
		out = append(out, '\n')
		out = append(out, body[i:]...)

		return out
	}

	// Not a shape we recognise. Never emit a half-edited manifest.
	return body
}
