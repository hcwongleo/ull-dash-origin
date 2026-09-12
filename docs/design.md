# Design and architecture

Everything about how this origin works and why, including the low-level design of
the Go server. If you are deploying or operating it, read
[operations.md](operations.md) instead. A visual walkthrough of the same material is
in [architecture.html](architecture.html).

---

## 1. The problem

A DASH origin normally hands out whole files. That puts a floor under live latency:
nothing can be served until a segment is complete, so you pay at least one full
segment duration — 2 s here — before a viewer sees anything, plus buffering.

**This origin serves a segment while the encoder is still uploading it.** A reader
chases the writer a fraction of a second behind. That is the entire reason it exists,
and it is what S3 cannot do at any price: S3 makes an object readable only after the
PUT completes.

Measured end to end through CloudFront: **~4 s glass-to-glass**, and a segment
delivered progressively over 2129 ms while it was still being written.

### Why not a managed service

| Option | Available in ap-east-1 | Latency |
|---|---|---|
| **MediaPackage** | **No** — endpoint does not exist; Hong Kong absent from its pricing | — |
| **MediaStore** | **No** — discontinued, and never in ap-east-1 | — |
| **S3** | Yes, ~$36/month | ~8–15 s: no read-while-write |
| **This origin** | Yes, ~$589/month | **~4 s** |

Verified, not assumed: `mediapackagev2.ap-east-1.amazonaws.com` does not resolve, and
`AWS/CloudFront`-style regional pricing lists Singapore, Tokyo, Seoul and others but
not Hong Kong. So in this region a self-managed origin is the only way to get ~4 s.

At scale the origin is **~0.1% of the CloudFront viewer egress bill**, so this was
never a cost decision. 100k viewers at 4.25 Mbps is ~190 TB/hour of CDN egress.

---

## 2. Architecture

```
                    plaintext HTTP, IP-restricted
Elemental Live ──────── chunked PUT ────────▶  EC2 origin  (all content in RAM)
                                                    │
                                                    │  GET, read-while-write
                                                    ▼
                                            CloudFront (playback)
                                          segments 6 h · manifest 1 s
                                                    │
                                                    ▼
                                                 players
```

One CloudFormation stack, one region, 29 resources.

### Why ingest goes direct, with no CDN in front

**A CDN's value is fan-out to many readers. Ingest has exactly one writer.** So an
ingest CDN adds a hop, a TLS terminate-and-re-originate, and a Lambda invocation per
manifest, for no fan-out benefit.

It also decides authorisation, and this is the stronger argument. With CloudFront in
front of ingest, the security group must accept `PUT`/`DELETE` from CloudFront's
**entire origin-facing prefix list** — because that is the only source address
CloudFront presents. **Every CloudFront distribution in the world is then inside your
write perimeter.** Direct ingest restricts writes to one address.

**The cost:** ingest is plaintext HTTP, and there is no authentication in the server.
See [Security](#7-security).

A second pipeline with the ingest CDN and a Lambda@Edge was built and measured. It is
kept out of this repository because it is not a deliverable. One question it exists to
answer remains open: whether an ingest CDN coalesces the encoder's 0.2 s fragments
into larger writes. An early observation saw ~51 KB arriving every ~0.5 s, but that
may have been the sampling interval. **Unresolved — do not ship a latency claim
either way.**

### Playback caching

| Path | TTL | Why |
|---|---|---|
| `*.mpd` | 1 s | At or below the encoder's `minimumUpdatePeriod`. A stale manifest moves the player's idea of the live edge |
| segments | 6 h | Immutable once written. **Safe only because segment names carry a per-run token** — recycled names would serve stale video |
| 404 | 1 s | ULL players request segments before they exist because the manifest invites it, and those requests are synchronised across the audience. Uncached, that is one origin hit per viewer in a burst every segment boundary. 1 s collapses it to one per POP |
| 403, 5xx | 0 | Never cache a negative at the live edge |

---

## 3. Low-level design of the origin

Upstream is `mjneil/go-chunked-streaming-server@fa4b736`, MIT. 28 commits on top.
Every change is reviewable as `git log fa4b736..HEAD`.

```
main.go                97   flags, wiring
server/handlers.go    301   GET / PUT / DELETE / HEAD / OPTIONS
server/file.go        263   the File type, the read path — the core
server/persist.go     365   initialisation segments across a restart
server/waiting_requests.go 250   holding a GET for a not-yet-arrived segment
server/sweep.go       201   reclaiming what the encoder never deletes
server/admin.go       168   health, metrics, pprof
server/manifest.go    117   UTCTiming injection
server/ingest.go       67   bounding ingest silence
server/logging.go      62   level gating
```

### The data model

```go
var Files = map[string]*File{}      // keyed by the request URL, verbatim
var FilesLock = new(sync.RWMutex)   // guards the map, not the files

type File struct {
    Name       string
    headers    http.Header    // copied from the PUT, minus User-Agent
    lock       *sync.RWMutex  // guards this file's buffer and eof
    cond       *sync.Cond     // built on lock.RLocker()
    buffer     []byte         // the whole object, in memory
    eof        bool           // the writer has finished
    receivedAt time.Time
}
```

Two lock levels, and the distinction matters: `FilesLock` protects the *map*, each
`File.lock` protects *that file's bytes*. A reader holds only the file's read lock
while streaming, so ingest of other files is never blocked.

**A map entry is a strong reference.** Go's collector frees what is *unreachable*,
not what is *unused*, so nothing here is ever reclaimed by the runtime. Verified: four
test objects were still resident after 18.9 hours including a forced GC. Everything in
[§5 Reclamation](#5-reclamation) exists because of that one fact.

### The read path — the load-bearing fix

Upstream, when a reader drained everything written so far but the writer had not
finished:

```go
if r.offset >= len(r.File.buffer) {
    return 0, nil                    // io.Copy reads this as "retry immediately"
}
```

`io.Copy` has no backoff for `(0, nil)`. **Every viewer at the live edge span a full
CPU core doing nothing** — a profiler put 87.67% of all server CPU inside that one
function. Worse, a spinning reader never attempts a write, so it never discovers the
viewer disconnected: it span **forever**. Four disconnected readers left an origin
burning 2.43 cores permanently with two ingests wedged.

Now:

```go
func (r *FileReadCloser) Read(p []byte) (int, error) {
    r.File.lock.RLock()
    defer r.File.lock.RUnlock()
    for r.offset >= len(r.File.buffer) && !r.File.eof {
        if err := r.ctx.Err(); err != nil { return 0, err }   // viewer gone
        r.File.cond.Wait()                                     // sleep until bytes
    }
    if r.offset >= len(r.File.buffer) { return 0, io.EOF }
    n := copy(p, r.File.buffer[r.offset:])
    r.offset += n
    return n, nil
}
```

**Why the condition variable is built on `lock.RLocker()`.** Every waiter is a reader
holding `RLock`. `Wait` enqueues on the notify list *before* unlocking, and a writer
cannot append while any reader holds `RLock` — so a `Broadcast` issued after the write
lock is released can never be missed. That is the whole correctness argument for the
read path.

| Measured, live 4 Mbps stream | Upstream | Now |
|---|---|---|
| CPU, idle | **1.07 cores** | **0.0036** |
| 12 readers at the live edge | ~12 cores | 0.014 cores |
| Disconnect all 12 | never recovers | 0.0025, goroutines 29→5 |
| RSS | 1.19 GB | 24.6 MB |
| Wedged ingests | 2 | 0 |

The 1.19 GB was not a separate leak: it was memory pinned by spinning readers holding
files the encoder had already deleted. One fix removed both.

### Waking a parked reader

Three things must wake a reader, or it waits on a signal nobody will send:

| Event | Mechanism |
|---|---|
| Bytes arrive | `Write` broadcasts |
| Writer finishes | `Close` sets `eof`, broadcasts |
| **Viewer disconnects** | `context.AfterFunc(ctx, cond.Broadcast)`, and `Read` re-checks `ctx.Err()` on every wake |
| **File reclaimed** | `Discard` sets `eof`, nils the buffer, broadcasts |

### One goroutine per request

`net/http` runs a goroutine per request, so the count *is* the number of requests in
flight. Measured on the deployed origin: **5 idle → 25 with 20 readers → 85 with 80
readers → 5 again.** A count that ratchets up and never falls means requests are
stuck, which is the shape every leak here has had. That is why it is a dashboard
metric and an alarm.

### A subtle one: `Close` used to truncate live segments

`FileReadCloser` embeds `*File`, so `rc.Close()` resolved to the promoted
`File.Close` — which sets `eof`. **One viewer closing its connection marked the
segment complete for every other viewer**, silently truncating a segment still being
ingested. An explicit `FileReadCloser.Close` now shadows it. This produced no error
anywhere; it corrupted playback quietly.

### Ingest: bounding silence, not duration

A half-open PUT — headers and a partial body, then silence with the socket open —
pinned a goroutine and a socket **forever**. Measured: 25 such connections held 25
file descriptors permanently.

The body is wrapped so the connection read deadline is **reset before every read**.
The deadline therefore bounds *silence*, not total duration, so a legitimate 2 s
chunked PUT is unaffected. A plain `ReadTimeout` on `http.Server` cannot do this: it
bounds the whole request and would kill every long ingest. That is also why
`ReadTimeout` stays zero while `ReadHeaderTimeout` is 15 s.

A timed-out ingest leaves a **truncated** segment, so it is deleted from the map and
returns `400` rather than being served as if complete. A `404` the encoder retries;
bad bytes it does not.

### Holding early requests instead of refusing them

The manifest advertises `availabilityTimeOffset="1.800"` — an invitation to request a
segment 1.8 s early. With `-w`, such a request is held until the data arrives.

**The hold must exceed what the manifest invites.** Upstream hardcoded 1000 ms.
Against 1.8 s that stalled players for ~1.2 s and then refused them, and every retry
moved the player's position — the fluctuating live latency seen in production. It is
now `-wait-timeout-ms`, default **2500 ms**, with a test that fails if anyone lowers
the default below 1.8 s.

Configurable rather than derived from the manifest, deliberately: every coupling
between this origin and the encoder's DASH configuration has silently broken when the
encoder was reconfigured. A generous fixed value cannot.

### Manifest rewriting: `UTCTiming` only

A player with no `UTCTiming` trusts the local device clock, mis-computes the live edge
and jumps gaps — observed as `dash.js Error 16` followed by a 6.08 s gap jump.
Elemental Live does not emit it.

So on PUT of a `*.mpd`, before storing, the origin inserts:

```xml
<UTCTiming schemeIdUri="urn:mpeg:dash:utc:http-iso:2014" value="https://time.akamai.com/?iso"/>
```

**Deliberately not `ServiceDescription`/`Latency@target`.** An earlier build injected
one via Lambda@Edge with a hardcoded 4000 ms that disagreed with the encoder's
`suggestedPresentationDelay="PT3S"`, and a player given two targets hunts between
them. `UTCTiming` is a **constant**, not a value derived from encoder timing, so
unlike a latency target it cannot fall out of step.

Safety properties, each with a test:

- **Only `*.mpd` is buffered**, matched on **filename** because the encoder's
  `Content-Type` is not dependable. A false positive on a media segment would buffer
  it and destroy read-while-write — the entire purpose of the server.
- Bodies over 1 MB stream through unmodified rather than being held.
- Idempotent — the manifest is re-PUT every second; verified still one element after
  six PUTs.
- Fail-open on everything: not XML, no `<MPD`, empty, truncated, already present.
- String manipulation, never an XML parse, so a quirky declaration or namespace can
  never turn a working manifest into a broken one.

A bug the tests caught: rewriting the body invalidated the `Content-Length` copied
from the PUT, so `net/http` truncated the response — **zero bytes to the player**.
Every unit test on the injection function passed; only the end-to-end test found it.
The stale header is now dropped whenever the body changes.

---

## 4. Persistence: surviving a restart

Everything is in RAM, and **Elemental Live sends initialisation segments only when an
output group starts.** So any restart lost them and no encoder resent them: viewers
already watching kept playing, and only **new** viewers failed — which is why it
looked healthy for hours. This caused two outages in development.

It also undid every automatic recovery in the system. systemd restarts a crash in
~2 s, the watchdog catches a hang within a minute, AWS recovers a failed host unasked
— and all three then handed back a stream that would not accept new viewers.

**Init segments are written to disk; media segments never are.** They are a few
hundred bytes and arrive once per encoder run, so it costs nothing on the hot path.
Media would be a disk write per segment for no benefit, and the encoder replaces them
within seconds anyway.

### The security work, which is most of the code

The filename comes from an **unauthenticated request URL**, and this is the only place
in the server that turns a request into a filesystem path.

```go
filepath.Join("/opt/gochunked/content", "/../../etc/cron.d/x")  →  "/etc/cron.d/x"
```

`filepath.Join` cleans a path but resolves outside its base quite happily. systemd's
`ProtectSystem=strict` would block that write today — but **that protection lives in
the unit file, not the program.** Run the binary by hand and it is gone.

So the name is percent-encoded to one flat filename with no separators, never exactly
`.` or `..`, and the resolved path is re-checked to be inside the directory
regardless. Reversible, so the original URL is recovered exactly on reload.

```
/ch18live-v_init.mp4   →   %2Fch18live-v_init.mp4
```

Also: 16 MB per file, 4096 files, **512 MB in aggregate**. The first two are
deliberately loose so nothing real is ever rejected; the aggregate is what actually
bounds the disk, because 4096 × 16 MB would be 68 GB on a 30 GB volume. Eviction is
**oldest-first**: with several channels on one origin, refusing the newest would deny
protection to the channel currently on air while keeping files from runs that ended
weeks ago.

Stored HTTP-style — header lines, a blank line, then the body — so `Content-Type`
survives and a human can see what the file is. Written then renamed, so a restart can
never read a half-written file. Every load failure is skipped rather than fatal: the
origin must come up regardless.

Verified on the live instance: `SIGKILL`, systemd restart, **init segment served
`200` and byte-identical, media segment correctly `404`.**

---

## 5. Reclamation

Memory is bounded three independent ways.

**1. The encoder's own DELETEs.** Elemental deletes each segment as it leaves the
manifest window — 27,507 DELETEs measured in one five-hour session. This does all the
work in normal operation, and steady state is ~25 MB.

**2. `-idle-sweep 3600`.** A garbage collector, not a DVR window. Reclaims files that
are complete, not initialisation segments, and untouched for an hour. It covers two
cases the encoder does not:

- An output that **stops**: the DELETE for segment N is triggered by segment N+6
  arriving, so the last window's worth — ~6.4 MB — is never deleted.
- DELETEs **stopping altogether** — a reconfigured output, a different encoder, a bug.
  Growth then runs at the full ingest rate, ~1.8 GB/hour.

3600 s is ~300× a typical 12 s `timeShiftBufferDepth`, so it cannot reach live
content. Three phases, each guarding a bug this server has had: collect under
`RLock`; delete under `Lock` **re-checking object identity**, so a PUT landing between
phases does not have its replacement destroyed; then `Discard` outside `FilesLock`, so
a parked reader is woken rather than left waiting. Cost measured at **5.1 µs per
sweep** at ~100 files, once a minute.

**3. `MemoryMax=25%`.** A cgroup ceiling, expressed as a percentage so it scales with
the instance. Its job is to make a runaway die inside its own cgroup rather than
invoking the kernel OOM killer, which might take sshd or the SSM agent instead. A
fixed 4 G was wrong in both directions — too small for a large ladder, where the
sweeper's own bound is bitrate × idle-sweep.

`GOMEMLIMIT` is deliberately **not** set: in a real runaway the memory is live, so a
harder-working collector cannot reclaim it and would only burn CPU delaying an
inevitable kill.

---

## 6. Observability

`/-/metrics` on `127.0.0.1:9095`, plus `/-/healthz` and `pprof`. Separate from the
content port for two reasons: `Files` is keyed by request URL, so a `PUT /-/healthz`
would shadow the endpoint; and pprof must never be reachable from the internet.

Memory is reported from `runtime.ReadMemStats`, **deliberately not a hand-maintained
counter**. An earlier build incremented on write and decremented on release, missed
the decrement on one route, and drifted to **60× the true value** in production —
6.1 GB reported against 81 MB actual. Worse than no metric, because it looked
authoritative.

A systemd timer publishes six of them to CloudWatch every five minutes, then five
alarms and a dashboard. **No CloudWatch agent**: a separate daemon with its own config
and update cycle is not justified by six numbers on a host that otherwise runs one
static binary.

| Alarm | Fires when |
|---|---|
| `ingest-stale` | no ingest for >60 s **or the origin stops answering** |
| `heap` | >1 GB for 15 min |
| `goroutines` | >30000 for 15 min — high because held requests are legitimate |
| `cpu` | >50% for 15 min, against a measured 0.0036 cores |
| `host-check-failed` | AWS hardware event; **memory was cleared, check init segments** |

`ingest-stale` is the highest-value one and has nothing to do with leaks: it is the
only signal that the stream is off the air without a viewer complaining. The publisher
sends **nothing** when the origin is not answering — a zero would be
indistinguishable from healthy — so that alarm treats missing data as breaching.

**There is deliberately no CloudFront alarm.** CloudWatch alarms are regional and
CloudFront publishes only to us-east-1: `AWS/CloudFront` has 0 metrics in ap-east-1
and 100 in us-east-1. An alarm in the origin's region would sit in `OK` forever and
never fire, which is worse than none because it looks like coverage. CloudFront errors
remain on the dashboard, which *can* graph across regions.

`pprof` is how the spin in §3 was found, and it stays for the same reason.

---

## 7. Security

**There is no authentication on ingest.** Anyone who can reach the content port from a
permitted address can PUT or DELETE segments in the live stream.

| | |
|---|---|
| Write access | `EncoderCidr` only, no default, and `0.0.0.0/0` rejected by a template assertion |
| Read access | CloudFront's origin-facing prefix list, `GET`/`HEAD`/`OPTIONS` only |
| **Ingest encryption** | **none — plaintext HTTP** |
| Shell access | SSM Session Manager; no inbound SSH |
| Process | non-root `gochunked`, `ProtectSystem=strict`, `NoNewPrivileges`, `PrivateTmp` |
| Volume | encrypted |

TLS on the origin is not offered, deliberately: a custom CloudFront origin requires a
publicly trusted certificate matching its domain, which an EC2 public DNS name cannot
have. Doing it properly needs a real domain plus ACM or Let's Encrypt. **If the
encoder is on-premises, use Direct Connect or a VPN** rather than plaintext across the
public internet.

Long segment TTLs are safe **only** while segment names carry a per-run token. If names
are ever recycled, viewers are served stale video — worse than a cache miss.

---

## 8. Configuration

Eleven flags. **None depends on the encoder's DASH configuration**, and that is the
design rule, not a coincidence: a flag derived from `availabilityTimeOffset` or
`timeShiftBufferDepth` becomes silently wrong the first time the encoder is
reconfigured. An earlier build had six such couplings and two were wrong in
production.

```
-r                          RAM only for media
-i 9094                     content port
-o cors.json                CORS policy
-p content                  content path
-w                          hold a GET for a not-yet-arrived segment
-wait-timeout-ms 2500       the hold ceiling; MUST exceed availabilityTimeOffset
-ingest-idle-timeout 10     abort an ingest silent this long
-idle-sweep 3600            reclaim complete non-init files idle this long
-init-pattern <regex>       what is exempt from sweeping, and what is persisted
-persist-init               keep initialisation segments across a restart
-utc-timing <url>           injected into manifests on ingest
-admin-addr 127.0.0.1:9095
-log-level warn
```

`deploy/verify-flags.sh` checks every flag the unit file passes exists in the binary.
It exists because one did not, once: `-wait-timeout-ms` was added to the unit file
before the flag existed, so the binary exited immediately and CloudFormation rolled
the stack back 15 minutes later with nothing useful in the failure message.

---

## 9. Removed on purpose

These were built, measured, found redundant or harmful, and deleted. Recorded because
the reasoning is harder to recover than the code, and because the obvious next move
for anyone reading is to add them back.

| Removed | Why |
|---|---|
| **Retention window** (`-max-age-floor/cap/default`, ~500 lines) | Sized against `timeShiftBufferDepth`. The encoder correctly sends `Cache-Control: max-age=2` on the manifest; retention honoured it and **deleted the origin's own manifest 2 s after ingest.** Found in production by its own detector |
| **Byte-ceiling eviction** | Depended on the per-file counter that drifted 60× |
| **`ServiceDescription` injection** | Its hardcoded 4000 ms conflicted with the encoder's `PT3S`, and a player given two targets hunts |
| **Ingest CloudFront + Lambda@Edge** | No fan-out benefit, and it forced write access for every CloudFront distribution in the world |
| **Release-archive tooling** | Unreachable once the template built from a git tag; a second untested deployment path that would rot |
| **A CloudFront alarm** | Regionally impossible to make work from the origin's region — see §6 |
| **`GOMEMLIMIT`** | Had to be kept consistent with `MemoryMax` by hand, and cannot reclaim live memory |

---

## 10. Known gaps

| Gap | Status |
|---|---|
| **No HA** | Single instance, single AZ. Accepted requirement. A whole-AZ loss is unhandled |
| **No ingest authentication** | The server has none; `EncoderCidr` is mitigation, not authentication |
| **Plaintext ingest** | See §7 |
| **Multi-channel monitoring** | `last_ingest_age` is global. If two channels share an origin and one stops, the other keeps the alarm quiet. One origin per channel avoids it and buys independent restarts, alarms and sizing |
| **No `Range` support** | Confirmed zero handling. CloudFront copes; a player fetching ranges directly gets `200` + full body instead of `206` |
| **Ingest-CDN coalescing** | Unresolved, §2 |
| **Not soaked** | **The most important one.** Hours against one live encoder, not weeks under load |

On that last row: **two defects in this project were found only by leaving it
running** — a per-manifest accounting drift and readers parked on reclaimed files.
Neither was visible to the test suite or to a short benchmark. 39 race-clean tests
are necessary and not sufficient.

---

## 11. Sizing

Measured on the deployed origin with a live stream: **0.0036 cores, 24.6 MB RSS,
~1 MB/s of unique content.**

`c8g.4xlarge` (16 vCPU / 32 GB) is therefore ~2000× the measured CPU. It is sized for
**concurrency, not memory**: with `-w`, a held request occupies a goroutine and a
connection for up to 2.5 s, and at 100k viewers with ~10% early at any instant that is
~15,000 concurrent held requests — about 3.5 cores by measurement (2000 readers =
0.39 cores). Memory cannot be the constraint: `MemoryMax` is 25% and steady state is
25 MB.

**Origin load does not scale with audience.** Segments cache 6 h at the edge with
request collapsing, so the origin serves roughly one fetch per POP per segment, not
per viewer. 100k viewers and 100 look nearly identical to it.

Storage is 30 GB gp3 against 2.4 GB in use. gp3 decouples performance from capacity —
8 GB and 100 GB both give 3000 IOPS and 125 MB/s — so a larger volume buys capacity
only.

The 100k figure is extrapolated from 2000 synthetic readers. It has not been tested.
