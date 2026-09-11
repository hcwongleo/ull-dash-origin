# ull-dash-origin

Ultra-low-latency DASH origin for AWS. Read-while-write chunked CMAF on a single
EC2 instance behind CloudFront, deployed by CloudFormation.

The origin serves a segment **while the encoder is still uploading it**. That is
what makes ~4 s live latency reachable: a conventional whole-file origin cannot go
below one segment duration, because it has nothing to send until the segment is
complete.

```
Elemental Live ──chunked PUT──▶ EC2 origin ──▶ CloudFront ──▶ players
                                (in RAM)        (playback)
```

Based on [mjneil/go-chunked-streaming-server](https://github.com/mjneil/go-chunked-streaming-server)
(MIT). Our changes sit on top of upstream `fa4b736`, so they are reviewable as a
diff:

```bash
git log --oneline fa4b736..HEAD -- main.go server/
```

## Status — read this first

This has run for hours against one live Elemental Live encoder, not for weeks
under production load. It is **not** claimed to be battle-tested.

**Known limitation, and it matters operationally:** content lives in RAM, and
Elemental Live sends initialisation segments only when an output group *starts*.
So after any restart of this process — a crash, a deploy, an AWS host event — new
viewers get 404 on `*init.mp4` until the **encoder output is restarted**. This
caused two outages during development. It is not yet fixed; see
[Known gaps](#known-gaps).

## Deploy

```
pipelines/direct-ingest/          ★ production. Elemental → EC2
pipelines/cdn-ingest-testing/       testing only. Elemental → CloudFront+Lambda@Edge → EC2
```

Customer-facing instructions: **[release/CUSTOMER-DEPLOY.md](release/CUSTOMER-DEPLOY.md)**

The only parameter with no safe default is the encoder's public IP. Ingest is
unauthenticated, so `0.0.0.0/0` would let anyone inject or delete segments in a
live stream; the template rejects it.

## What was fixed

Nine changes against upstream. The first is the one that matters.

| Fix | Measured effect |
|---|---|
| **`Read()` returned `(0, nil)` when a reader caught the writer.** Go's `io.Copy` treats that as *retry immediately*, so every viewer at the live edge span a full CPU core — and never attempted a write, so it never noticed the viewer had disconnected and spun **forever**. Now parks on a condition variable. | **1.07 cores → 0.0036.** 12 live-edge readers: 0.014 cores. Disconnect all 12: 0.0025 cores, goroutines 29→5 |
| Reader parked on a removed file waited forever | no permanently parked readers |
| `rc.Close()` reached the promoted `File.Close`, setting `eof` and **truncating a live segment for every other viewer** | silent corruption removed |
| Half-open ingest pinned a goroutine and socket forever | 25 dead uploads held 25 fds permanently → reclaimed in 10 s |
| A timed-out ingest left a truncated segment served as complete | discarded, 400, counted |
| `log.Fatalf` on a disk-write error **killed the whole process** | returns 500 |
| `panic()` on a disk-open failure | logged, 500 |
| `http.ListenAndServe` with no header timeout | `ReadHeaderTimeout`, `IdleTimeout` |
| No health or metrics endpoint; 3 log lines per request unconditionally | localhost `/-/healthz`, `/-/metrics`, pprof; per-request logging at debug |

Memory is reported from `runtime.ReadMemStats`, deliberately not a
hand-maintained counter. An earlier build incremented on write and decremented on
release, missed the decrement on one route, and drifted to **60× the true value**
in production — worse than no metric, because it looked authoritative.

## Configuration

Five flags. **None depends on the encoder's DASH configuration**, which is
deliberate: a flag derived from `availabilityTimeOffset` or `timeShiftBufferDepth`
becomes silently wrong the first time the encoder is reconfigured. An earlier
build had six such couplings and two of them were wrong in production.

```
-r                        RAM only
-i 9094                   content port
-o cors.json              CORS policy
-p content                content path
-ingest-idle-timeout 10   abort an ingest silent this long
-admin-addr 127.0.0.1:9095
-log-level warn
```

**There is no retention setting, on purpose.** Memory is bounded by the encoder's
own DELETE requests — 27,507 measured in a single 5-hour session. An earlier build
had a ~500-line retention subsystem; it proved redundant, and its tuning had to
track encoder settings, so it was removed.

## Build

Docker only; no Go toolchain assumed. Dependencies are vendored, so the build
needs no module proxy and is reproducible — which is how you verify a binary
matches the source.

```bash
docker run --rm -v "$PWD":/src -w /src \
  -e GOFLAGS=-mod=vendor -e GOPROXY=off -e GOCACHE=/tmp/gc -e GOTMPDIR=/tmp \
  -e CGO_ENABLED=0 -e GOOS=linux -e GOARCH=arm64 \
  golang:1.23 go build -trimpath \
  -ldflags "-s -w -X main.gitSHA=$(git rev-parse --short HEAD)" -o gcss-arm64 main.go
```

Release archive for handover: `./release/build-release.sh`

## Test

```bash
docker run --rm -v "$PWD":/src -w /src \
  -e GOFLAGS=-mod=vendor -e GOPROXY=off -e GOCACHE=/tmp/gc -e GOTMPDIR=/tmp \
  golang:1.23 go test -race -count=1 ./...
```

9 tests, race-clean. `testing/` holds the soak harnesses — worth keeping, because
**two defects were found only by those and not by the unit tests**: a per-manifest
accounting drift, and readers left parked on reclaimed files. Neither is visible
to a test suite or a short benchmark.

## Monitoring

```bash
curl -s http://127.0.0.1:9095/-/healthz
curl -s http://127.0.0.1:9095/-/metrics
```

| Metric | Healthy |
|---|---|
| `go_memstats_heap_alloc_bytes` | plateaus (~25 MB for a 4 Mbps stream) |
| `go_goroutines` | plateaus at a low number |
| `gcss_put_aborted_total` | 0 |
| `gcss_last_ingest_age_seconds` | 0–2 while the encoder runs |

Sustained growth in the first two is the shape every leak in this server has
taken.

## Known gaps

- **Init segments are lost on restart** (above). Fixable in roughly 30 lines by
  persisting them to disk and reloading at startup. This is the most valuable
  outstanding change.
- **No ingest authentication.** The server has none. `direct-ingest` restricts
  writes to the encoder's address, which is mitigation, not authentication.
- **Ingest is plaintext HTTP.** TLS on the origin needs a real domain and a
  publicly trusted certificate, because CloudFront rejects a self-signed
  certificate on a custom origin. Use Direct Connect or a VPN from an on-premises
  encoder.
- **No EC2 auto-recovery.** A failed host needs a human.
- **`UTCTiming` must be configured on the encoder** when using `direct-ingest`.
  Without a timing source a player trusts the device clock, mis-computes the live
  edge and jumps gaps — observed as `dash.js Error 16` followed by a 6 s gap jump.

## Licence

MIT, as upstream. See [LICENSE.txt](LICENSE.txt) — copyright is retained by the
original authors, Matthew Neil and Jorge Cenzano.
