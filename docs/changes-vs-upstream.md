# What changed, feature by feature

Every change against upstream `mjneil/go-chunked-streaming-server@fa4b736`.
Reviewable as a diff:

```bash
git log --oneline fa4b736..HEAD -- main.go server/
git diff fa4b736..HEAD -- server/file.go
```

**686 lines of production code across 8 files. 18 tests, race-clean.**

Each entry says what upstream did, what it does now, and — where a number exists —
what was measured rather than assumed.

---

## 1. Blocking read instead of `(0, nil)` — the one that matters

`2ba3c47` · `server/file.go`

**Upstream.** When a reader drained everything written so far but the writer had
not finished, `Read` returned `(0, nil)`.

```go
// upstream
if r.offset >= len(r.File.buffer) {
    return 0, nil          // io.Copy treats this as "retry immediately"
}
```

Go's `io.Copy` has no backoff for that case. Every viewer sitting at the live edge
span a **full CPU core** doing nothing. Worse, a spinning reader never attempts a
write, so it never discovers the viewer disconnected — it span **forever**.

**Now.** Park on a condition variable, woken when bytes arrive or the request is
cancelled.

```go
for r.offset >= len(r.File.buffer) && !r.File.eof {
    if err := r.ctx.Err(); err != nil { return 0, err }
    r.File.cond.Wait()
}
```

The `sync.Cond` is built on `lock.RLocker()`, which is safe because every waiter
is a reader holding `RLock`: `Wait` enqueues on the notify list *before*
unlocking, and a writer cannot append while a reader holds `RLock`, so a
`Broadcast` issued after the write lock is released can never be missed.

| Measured on a live 4 Mbps stream | Upstream | Now |
|---|---|---|
| CPU, idle, no test load | **1.07 cores** | **0.0036** |
| Lifetime CPU (systemd) | 9 h 3 s | 22 ticks in 60 s |
| RSS | 1.19 GB | **24.6 MB** |
| 12 readers at the live edge | ~12 cores | 0.014 cores |
| Ingest PUTs permanently wedged | 2 | 0 |

The 1.19 GB was not a separate leak: it was memory pinned by spinning readers
holding files the encoder had already deleted. Fixing the spin fixed both.

---

## 2. Release readers when the viewer disconnects

`da72bdd` · `server/file.go`

**Upstream.** A parked reader was invisible to `net/http`. Nothing woke it.

**Now.** `NewReadCloser` registers `context.AfterFunc(ctx, f.cond.Broadcast)`, and
`Read` checks `ctx.Err()` on every wake-up. A disconnected viewer's goroutine
terminates instead of waiting for a writer that may never return.

Measured: 12 readers disconnected mid-segment → **0.0025 cores**, goroutines
29 → 5.

---

## 3. Release readers when their file is removed

`da72bdd` · `server/file.go`

**Upstream.** `RemoveFromDisk` discarded the buffer without setting `eof`. Any
reader parked on that file waited on a condition variable that would never be
signalled again.

**Now.** Discarding the buffer sets `eof` and broadcasts, so parked readers wake
and observe end-of-file. Terminal for the read, which is correct — the bytes are
gone.

---

## 4. `FileReadCloser.Close` no longer truncates a live segment

`2ba3c47` · `server/file.go`

**Upstream.** `FileReadCloser` embeds `*File`, so `rc.Close()` resolved to the
promoted `File.Close` — which sets `eof`. **One viewer closing its connection
marked the segment complete for every other viewer**, silently truncating a
segment still being ingested.

**Now.** An explicit `FileReadCloser.Close` shadows it, releasing only that
reader. Test: `TestReadCloserCloseDoesNotSetEOF`.

This one produced no error anywhere. It corrupted playback quietly.

---

## 5. Bounded ingest silence

`557f75b` · `server/ingest.go` (new)

**Upstream.** No read deadline on a PUT body. A half-open connection — headers and
a partial body sent, then silence with the socket still open — pinned a goroutine
and a socket **forever**.

**Now.** The body is wrapped so the connection read deadline is reset **before
each read**, using `http.ResponseController.SetReadDeadline`. The deadline
therefore bounds *silence*, not total duration — a legitimate 2-second chunked PUT
is unaffected, but an abandoned one is reclaimed.

A plain `ReadTimeout` on `http.Server` cannot do this: it bounds the whole
request, so it would kill every long chunked ingest.

Measured: 25 half-open connections held 25 file descriptors permanently →
reclaimed after 10 s. Tests: `TestHalfOpenIngestIsAborted`,
`TestSlowButSteadyIngestIsNotAborted`.

**Flag:** `-ingest-idle-timeout 10` (0 disables, which leaks per half-open
connection).

---

## 6. Aborted ingests are discarded, not served truncated

`159f298` · `server/handlers.go`

A consequence of #5: a timed-out ingest leaves a **truncated segment** in the map.
Serving it as if complete hands players corrupt media.

**Now.** On a copy error the entry is deleted (re-checking object identity first),
`PutAbortedTotal` is incremented, a warning is logged, and the request returns
`400`. A `404` the encoder retries; bad bytes it does not.

Test: `TestAbortedIngestIsDiscardedNotServed`.

---

## 7. A disk error no longer kills the process

`159f298` · `server/handlers.go`

**Upstream.**

```go
if err != nil {
    log.Fatalf("Error saving to disk: %v", err)   // exits the process
}
```

One failed write ended the origin, and with it every in-flight stream.

**Now.** Logged, returns `500`. Unreachable while `-r` is set, but it is one line
and the failure mode was total.

Also: `os.Open` failure on the disk path used to `panic`. Now logged, returns
`500`.

---

## 8. HTTP server timeouts

`557f75b` · `server/server.go`

**Upstream.** `http.ListenAndServe` with no timeouts. A client that opened a
connection and sent nothing held a slot indefinitely.

**Now.** An explicit `http.Server` with `ReadHeaderTimeout: 15s` and
`IdleTimeout: 120s`. **`ReadTimeout` stays zero on purpose** — it would bound the
whole request and kill legitimate long chunked PUTs. Silence is bounded per-read
instead, in #5.

---

## 9. Health, metrics and pprof

`159f298` · `server/admin.go` (new)

**Upstream.** None. No way to see any of the above happening.

**Now.** A separate listener on `127.0.0.1:9095`:

| Endpoint | |
|---|---|
| `/-/healthz` | JSON: `git_sha`, uptime, files, heap, last-ingest age, readers, aborted PUTs, goroutines |
| `/-/metrics` | Prometheus text, hand-rolled to keep the dependency tree at two modules |
| `/debug/pprof/` | this is how the spin in #1 was found — 87.67% of CPU in one function |

**Separate from the content port for two reasons:** `Files` is keyed by request
URL, so a `PUT /-/healthz` would shadow the endpoint; and pprof must never be
reachable from the internet.

**Memory is reported from `runtime.ReadMemStats`, deliberately not a
hand-maintained counter.** An earlier build incremented on write and decremented
on release, missed the decrement on one route, and drifted to **60× the true
value** in production — 6.1 GB reported against 81 MB actual. Worse than no
metric, because it looked authoritative. The runtime figure cannot drift.

`healthz` deliberately does **not** fail on stale ingest: a watchdog restarts the
service when this endpoint stops answering, and if staleness returned `503` a
legitimately ended event would cause a restart loop.

**Flag:** `-admin-addr 127.0.0.1:9095` (empty disables).

---

## 10. Per-request logging gated to debug

`159f298` · `server/logging.go` (new)

**Upstream.** Three lines per request, unconditionally — the request line, plus
`NEW File Content-Type` per PUT and `Reading from memory` per GET. At ULL segment
rates that is constant journald traffic for no diagnostic gain.

**Now.** Four levels, per-request logging at `debug`, default `warn`.
Deliberately no sampling: sampling was considered and dropped as complexity that
buys nothing once per-request logging is off by default.

**Flag:** `-log-level warn`.

---

## 11. `UTCTiming` injected on manifest ingest

`08edd1f` · `server/manifest.go` (new)

**Upstream.** Nothing. Elemental Live does not emit `UTCTiming` either — the
customer's existing Lambda@Edge exists precisely because it does not.

Without a timing source a player trusts the local device clock. A set-top box a
few seconds out then requests segments that do not exist yet or have already gone.
Observed in testing as `dash.js Error 16` (TIME_SYNC_FAILED) followed by a
**6.08 s gap jump**.

**Now.** On PUT of a `*.mpd`, before storing:

```xml
<UTCTiming schemeIdUri="urn:mpeg:dash:utc:http-iso:2014"
           value="https://time.akamai.com/?iso"/>
```

Inserted before the first `<Period>`, falling back to before `</MPD>`.

**Only `UTCTiming` — deliberately not `ServiceDescription`.** The Lambda also
injected `Latency@target`, and its hardcoded 4000 ms disagreed with the encoder's
`suggestedPresentationDelay="PT3S"`; a player given two targets hunts between
them. `UTCTiming` is a **constant**, not a value derived from encoder timing, so
unlike a latency target it cannot fall out of step when the encoder is
reconfigured. The encoder stays the single source of truth for latency.

**Safety properties, each with a test:**

- **Only `*.mpd` is buffered**, matched on **filename** because the encoder's
  `Content-Type` is not dependable. A false positive on a media segment would
  buffer it and destroy read-while-write — the entire purpose of this server.
- Bodies over **1 MB** are streamed through unmodified rather than held.
- **Idempotent** — the encoder re-PUTs the manifest every second; verified still
  one element after six PUTs.
- **Fail-open** — not XML, no `<MPD`, empty, truncated, or an existing
  `UTCTiming` all return the input untouched.
- **String manipulation, not an XML parse**, so a quirky declaration or namespace
  can never turn a working manifest into a broken one.

**A bug the tests caught.** Rewriting the body invalidated the `Content-Length`
copied from the PUT request. The response declared 63 bytes while writing 158, so
`net/http` truncated it — `unexpected EOF`, **zero bytes to the player**. Playback
would have been completely broken. The stale header is now dropped whenever the
body changes (`File.DropHeader`). Every unit test on the injection function passed;
only the end-to-end test found it.

**Flag:** `-utc-timing <url>` (empty disables).

---

## 12. Deployment: one CloudFormation template

`79f005e`, `894f569`, `264d4d3` · `origin-stack.yaml`, `deploy/`

**Upstream.** No deployment at all.

**Now.** One template. It clones a pinned git tag on the instance and builds it at
first boot, so there is nothing to upload, compile or look up beforehand.

- **`EncoderCidr` is the only parameter without a default.** Ingest is
  unauthenticated, so that address is the only control protecting it, and the
  template **rejects `0.0.0.0/0`** via a `Rules` assertion.
- **`CreationPolicy` + `cfn-signal`** — the stack does not report
  `CREATE_COMPLETE` until `/-/healthz` answers. Without it an operator sees
  success and points an encoder at an origin that is not listening yet.
- **`aws-cfn-bootstrap` is installed first.** `cfn-signal` ships preinstalled on
  Amazon Linux 2 but **not on 2023**. Every deployment would have timed out after
  15 minutes and rolled back — and the error trap called the same missing binary,
  so a real failure would have looked identical. Found only by deploying.
- **Vendored dependencies, `GOPROXY=off`** — the only network dependency is the
  repository, not a module proxy that could resolve differently later. Also makes
  the build reproducible, which is how a customer verifies the binary matches the
  source.
- **`install.sh` lives in the repo, not in UserData.** UserData runs *only at
  first boot*, so keeping the logic there would make provisioning a new instance
  and upgrading a running one two code paths that drift. It keeps the previous
  binary for rollback and has `--no-restart` for staging.
- **systemd:** non-root `gochunked` user, `StartLimitIntervalSec=0` (the default
  gives up after 5 restarts in 10 s and leaves the unit dead — precisely the
  behaviour you do not want at 03:00), `MemoryMax=4G`, `GOMEMLIMIT`, filesystem
  hardening.
- **Playback caching:** segments 6 h, manifest 1 s (at or below the encoder's
  `minimumUpdatePeriod`, because a stale manifest moves the player's idea of the
  live edge), and **403/404/5xx never cached** — a segment asked for a moment
  early exists a moment later.

A second pipeline with the ingest CDN and Lambda@Edge in front was built for
measurement and is kept out of this repository, since it is not a deliverable.

---

## Removed on purpose

An earlier build had these. They were measured, found redundant or harmful, and
deleted. Recorded because the reasoning matters more than the code.

| Removed | Why |
|---|---|
| **Retention window** (`-max-age-floor/cap/default`, ~500 lines) | Its tuning had to sit above `timeShiftBufferDepth`. The encoder correctly sends `max-age=2` on the manifest; retention honoured it and **the origin deleted its own manifest 2 s after ingest**. Found in production by its own detector. Elemental issues DELETEs anyway — 27,507 measured in one 5 h session. |
| **`-w` read-wait + `-wait-timeout-ms`** | Bought 0.2 s of latency (3.81 s vs 4.01 s) and cost visible jitter, because the timeout was 1000 ms while the manifest advertised `availabilityTimeOffset="1.800"` — players doing exactly what the manifest invited got a `404` after a 1.2 s stall. |
| **Byte-ceiling eviction** | Depended on the per-file counter that drifted 60×. |
| **`ServiceDescription` injection** | Its 4000 ms target conflicted with the encoder's `PT3S`. |
| **Release-archive tooling** | Unreachable once the template built from a git tag. A second, untested deployment path that would rot. |

**Flag count: upstream 8, now 11 — but only 3 are new**, and none of the three
depends on the encoder's DASH configuration. That is the design rule: a flag
derived from `availabilityTimeOffset` or `timeShiftBufferDepth` becomes silently
wrong the first time the encoder is reconfigured. An earlier build had six such
couplings and two of them were wrong in production.

---

## Known gaps

| Gap | Status |
|---|---|
| **No storage garbage collection** | Files in a Go map are never freed by the GC — the map entry *is* the reference. Verified: a forced GC left 4 test objects resident after 18.9 h. Anything the encoder never re-PUTs or DELETEs is **permanent**. Planned: [idle sweeper](#planned-idle-sweeper). |
| **Init segments lost on restart** | Content is in RAM and Elemental sends init segments only at output start, so any restart needs an encoder output restart before new viewers can play. Caused two outages in development. |
| **No monitoring reachable off-box** | `/-/metrics` is localhost-only and the CloudFront `5xxErrorRate` alarm is a *rate* that reports insufficient data at low viewership. A slow leak is invisible until the OOM kill. |
| **No ingest authentication** | The server has none. `direct-ingest` restricts writes to one address — mitigation, not authentication. |
| **Ingest is plaintext HTTP** | TLS on the origin needs a real domain and a publicly trusted certificate, because CloudFront rejects a self-signed certificate on a custom origin. |
| **No `Range` support** | Confirmed zero handling. CloudFront copes, but a player fetching ranges directly gets `200` + full body instead of `206`. |
| **No EC2 auto-recovery** | A failed host needs a human. |
| **Single instance** | Accepted customer requirement. |

### Planned: idle sweeper

Not a DVR window — a garbage collector. `-idle-sweep 300` would reclaim files that
are **complete**, **not matching the init pattern**, and untouched for 300 s.

At 300 s a 4.25 Mbps stream is bounded at ~160 MB and a 20 Mbps ladder at ~750 MB,
so it bounds memory even if the encoder stops sending DELETEs entirely. 300 s is
~25× a typical 12 s `timeShiftBufferDepth`, so the dependency is theoretical
rather than operational — unlike the removed retention window, which had to sit
*just above* it.

Precedent: **both AWS-managed origins ship this.** MediaStore had age-based Object
Lifecycle Policies with `seconds_since_create`; MediaPackage has a managed window.
Neither expects the encoder to be the sole reclaimer.

Three hazards, each already hit in this project and each needing a test:
in-progress files must never be swept; parked readers must be released, not left
waiting; and candidates must be collected under `RLock` then deleted under `Lock`
**re-checking object identity**, or a file replaced in between is destroyed.

---

## Testing

18 tests, race-clean:

```bash
docker run --rm -v "$PWD":/src -w /src \
  -e GOFLAGS=-mod=vendor -e GOPROXY=off -e GOCACHE=/tmp/gc -e GOTMPDIR=/tmp \
  golang:1.23 go test -race -count=1 ./...
```

`testing/` holds soak harnesses. Worth keeping, because **two defects were found
only by those and not by the unit tests**: a per-manifest accounting drift, and
readers left parked on reclaimed files. Neither is visible to a test suite or a
short benchmark.

**Status: not battle-tested.** Hours against one live encoder, not weeks under
load. Verified end to end on both deployed pipelines — real PUT, real playback
through CloudFront, `UTCTiming` present, segments byte-identical, and
read-while-write confirmed paced (360 bytes over 2129 ms through a real
distribution).
