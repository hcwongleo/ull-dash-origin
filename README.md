# ull-dash-origin

Ultra-low-latency DASH origin for AWS. Read-while-write chunked CMAF on a single
EC2 instance behind CloudFront, deployed by one CloudFormation template.

The origin serves a segment **while the encoder is still uploading it**. That is what
makes ~4 s live latency reachable: a conventional whole-file origin has nothing to send
until a segment is complete, so it cannot go below one segment duration.

```
Elemental Live ──chunked PUT──▶ EC2 origin ──▶ CloudFront ──▶ players
                                (in RAM)        (playback)
```

Based on [mjneil/go-chunked-streaming-server](https://github.com/mjneil/go-chunked-streaming-server)
(MIT). Our changes sit on top of upstream `fa4b736`, so they read as a diff:

```bash
git log --oneline fa4b736..HEAD -- main.go server/
```

## Deploy

One template. It builds the server from this repository at a pinned tag on the
instance, so there is nothing to upload, compile or look up first.

1. **Download** [`origin-stack.yaml`](origin-stack.yaml) — open it, then use GitHub's
   *Download raw file* button
2. **AWS console → CloudFormation → Create stack → Upload a template file**
3. **Enter your Elemental Live public IP** — the only required field
4. **Create stack**

About 15 minutes. The stack does not report success until the origin is answering, so
`CREATE_COMPLETE` means it is ready. The **Outputs** tab gives the ingest URL for
Elemental and the player URL.

**Chunked transfer encoding must be ON in Elemental Live.** Without it there is no low
latency at all and nothing reports an error. `UTCTiming` needs nothing configured —
the origin injects it.

Everything else has a working default: `c8g.4xlarge`, 30 GB encrypted gp3, five alarms,
a dashboard, and three operator buttons in Systems Manager.

## Documentation

| | |
|---|---|
| **[docs/operations.md](docs/operations.md)** | deploy, operate, upgrade, troubleshoot. Written for whoever runs the stream |
| **[docs/design.md](docs/design.md)** | every feature and why, the architecture, and the low-level design of the server |
| **[docs/architecture.html](docs/architecture.html)** | the same design, animated. Open it locally |
| **[docs/segment-journey.html](docs/segment-journey.html)** | one segment traced from encoder to viewer — every endpoint, what Go does at each step, and the CPU it costs |

## Status — read this first

This has run for hours against one live Elemental Live encoder, **not for weeks under
production load.** It is not claimed to be battle-tested.

What is measured: the CPU spin that made upstream unusable is fixed (1.07 cores →
0.0036), memory is bounded three ways, initialisation segments survive a restart, and
39 tests pass race-clean.

What is not: no HA — single instance, single AZ, by requirement. No ingest
authentication, and ingest is plaintext HTTP. See
[Known gaps](docs/design.md#10-known-gaps).

## Configuration

Eleven flags, in `deploy/gochunked.service` (the binary defines two more for origin
TLS, which is not used). **None depends on your DASH configuration** — that is deliberate. A flag derived from `availabilityTimeOffset` or
`timeShiftBufferDepth` becomes silently wrong the first time the encoder is
reconfigured; an earlier build had six such couplings and two were wrong in production.

```
-r -i 9094 -o cors.json -p content
-w -wait-timeout-ms 2500        hold early requests instead of refusing them
-ingest-idle-timeout 10         bound ingest silence, not duration
-idle-sweep 3600                reclaim what the encoder never deletes
-persist-init                   initialisation segments survive a restart
-utc-timing <url>               injected into manifests on ingest
-admin-addr 127.0.0.1:9095 -log-level warn
```

## Build and test

Docker only; no Go toolchain assumed. Dependencies are vendored, so the build needs no
module proxy and is reproducible — which is how you verify a binary matches the source.

```bash
docker run --rm -v "$PWD":/src -w /src \
  -e GOFLAGS=-mod=vendor -e GOPROXY=off -e GOCACHE=/tmp/gc -e GOTMPDIR=/tmp \
  golang:1.23 go test -race -count=1 ./...

./deploy/verify-flags.sh    # every unit-file flag exists in the binary
```

That last check exists because one did not, once: a flag was added to the unit file
before it existed in the binary, so the service exited immediately and CloudFormation
rolled back 15 minutes later with nothing useful in the failure message.

## Health

```bash
aws ssm start-session --target <instance-id>
curl -s http://127.0.0.1:9095/-/healthz
```

| Metric | Healthy |
|---|---|
| `go_memstats_heap_alloc_bytes` | plateaus, ~25 MB for a 4 Mbps stream |
| `go_goroutines` | plateaus; rises with viewers and **falls back** |
| `gcss_last_ingest_age_seconds` | 0–2 while the encoder runs |
| `gcss_put_aborted_total` | 0 |

Sustained growth in the first two is the shape every leak in this server has taken.

## Licence

MIT, as upstream. See [LICENSE.txt](LICENSE.txt) — copyright is retained by the
original authors, Matthew Neil and Jorge Cenzano.
