# ULL-DASH origin — deployment

A read-while-write DASH origin. It serves a segment **while the encoder is still
uploading it**, which is what makes ~4 s live latency achievable; a conventional
whole-file origin cannot go below one segment duration.

Everything here runs in **your** account. Nothing depends on the supplier's.

## What is in this archive

```
bin/go-chunked-streaming-server-arm64    statically linked, no runtime deps
bin/go-chunked-streaming-server-amd64
install.sh                               idempotent installer
deploy/gochunked.service                 systemd unit
deploy/cors.json
cloudformation/direct-ingest.yaml         ← recommended for production
cloudformation/cdn-ingest-testing.yaml    ← testing only
cloudformation/cdn-ingest-testing-lambda.yaml
src/                                      full source, MIT
CHANGES-VS-UPSTREAM.txt                   our fixes, as commits
SHA256SUMS
```

## Deploy

See the [README](../README.md#deploy): download
`pipelines/direct-ingest/origin-stack.yaml`, upload it in the CloudFormation
console, enter your Elemental Live public IP, create. About 15 minutes.

The template builds the server from a pinned git tag at first boot. Dependencies
are vendored and the module proxy is disabled, so the only network dependency is
this repository — a later change upstream cannot alter what you deployed.

**Deploying by CLI instead:**

```bash
aws cloudformation deploy --region ap-east-1 \
  --stack-name ull-origin \
  --template-file pipelines/direct-ingest/origin-stack.yaml \
  --capabilities CAPABILITY_IAM \
  --parameter-overrides EncoderCidr=<elemental-public-ip>/32
```

**A note on stack updates.** The AMI is resolved from an SSM parameter path so
that nothing has to be looked up by hand. The cost is that a later Amazon Linux
release will replace the instance during an unrelated stack update. Rebuilding is
harmless in itself, but the replacement clears RAM — see the warning below. Pass
an explicit `AmiId=ami-xxxx` on updates to hold the instance still.

## Encoder settings

| Setting | Value | Why |
|---|---|---|
| **Chunked transfer encoding** | **ON** | Without it there is no low latency at all, and **nothing reports an error** |
| Fragment length | ~0.2 s | Sets chunk granularity, therefore latency |
| Segment length | 2 s | |
| Segment control | Segmented Files | not single-file |
| `availabilityTimeOffset` | ~1.8 s, `availabilityTimeComplete=false` | low-latency signalling |
| `suggestedPresentationDelay` | 3–4 s | the player's target |
| `UTCTiming` | leave it | the origin injects it — see below |
| Segment naming | must include a per-run token | see caching note |

**`UTCTiming` is handled by the origin.** With no timing source a player trusts
the local device clock, mis-computes the live edge, and jumps gaps — observed
during testing as `dash.js Error 16` followed by a 6 s gap jump. Elemental Live
does not emit `UTCTiming`, so the origin adds it as the manifest is ingested.

It defaults to `https://time.akamai.com/?iso`. Change it with `-utc-timing <url>`,
or set that to empty to disable injection if your encoder already supplies one.
The origin never overwrites a `UTCTiming` that is already present.

The origin deliberately does **not** inject `ServiceDescription`/`Latency@target`.
An earlier build did, and its hardcoded 4000 ms disagreed with the encoder's
`suggestedPresentationDelay` of `PT3S`; a player given two targets hunts between
them. The encoder stays the single source of truth for latency.

**Confirm chunked transfer is actually on** — this is the one silent failure:

```bash
curl -s -o /dev/null -w '%{time_starttransfer}\n' \
  'https://<playback-domain>/<newest-segment>.mp4'
```
**~0.1 s = working. ~2 s = not chunked.**

## Upgrading

`install.sh` is the same script UserData runs, so provisioning and upgrading are
one code path rather than two that drift.

```bash
aws s3 cp s3://.../gcss-origin-<new-sha>.zip /tmp/r.zip
unzip -q -o /tmp/r.zip -d /tmp/r && /tmp/r/gcss-origin-<new-sha>/install.sh
```

It verifies checksums, keeps the previous binary at
`bin/go-chunked-streaming-server.previous`, and restarts. Rollback is a move and
a restart.

**Also update `ArtifactKey` in the stack**, or the next instance replacement
boots the old version.

> **A restart requires an encoder restart.** Content lives in RAM, and Elemental
> Live sends initialisation segments only when an output group starts. After any
> restart, new viewers get 404 on `*init.mp4` until you restart the Elemental
> output. Use `install.sh --no-restart` to stage a build and pick the moment.
> This is the most important operational characteristic of this origin.

## Configuration

Five flags, in `deploy/gochunked.service`. **None depends on your DASH
configuration** — that is deliberate. A flag derived from
`availabilityTimeOffset` or `timeShiftBufferDepth` becomes silently wrong the
first time the encoder is reconfigured.

```
-r                        RAM only, nothing on disk
-i 9094                   content port
-o cors.json              CORS policy
-p content                content path
-ingest-idle-timeout 10   abort an ingest silent this long
-admin-addr 127.0.0.1:9095
-log-level warn
-utc-timing https://time.akamai.com/?iso    injected into manifests on ingest
```

**There is no retention setting, on purpose.** Memory is bounded by the encoder's
own DELETE requests — measured at 27,507 in a single 5-hour session. An earlier
build had a retention subsystem; it proved redundant and its tuning had to track
encoder settings, so it was removed.

## Monitoring

Localhost only, so nothing is exposed:

```bash
aws ssm start-session --target <instance-id>
curl -s http://127.0.0.1:9095/-/healthz
curl -s http://127.0.0.1:9095/-/metrics
```

Two figures are worth alarming on. **Sustained growth in either is the shape every
leak in this server has taken:**

| Metric | Healthy |
|---|---|
| `go_memstats_heap_alloc_bytes` | plateaus (~25 MB for a 4 Mbps stream) |
| `go_goroutines` | plateaus at a low number |
| `gcss_put_aborted_total` | 0 — non-zero means the encoder is dropping uploads |
| `gcss_last_ingest_age_seconds` | 0–2 while the encoder is running |

`healthz` reports `git_sha`, so you can always tell support exactly which build
is running.

## Operational setup (once per account)

### Confine AWS-initiated reboots to a maintenance window

AWS occasionally schedules reboots, stops and retirements. An Instance Event
Window confines them to hours you choose. Times are **UTC and whole hours only**,
so 03:00–07:00 Hong Kong is 19:00–23:00 UTC the previous day.

```bash
RANGES=$(for d in sunday monday tuesday wednesday thursday friday saturday; do
  printf '{"StartWeekDay":"%s","StartHour":19,"EndWeekDay":"%s","EndHour":22},' "$d" "$d"
done | sed 's/,$//')

aws ec2 create-instance-event-window --region ap-east-1 \
  --name ull-origin-maint --time-ranges "[$RANGES]"

aws ec2 associate-instance-event-window --region ap-east-1 \
  --instance-event-window-id <iew-id> \
  --association-target "InstanceIds=<instance-id>"
```

**Re-associate it whenever the instance is replaced.** The association is to an
instance ID, not to the stack, so a replacement leaves the window pointing at a
dead instance and silently protecting nothing. We found exactly this in
development: the window had been guarding an instance terminated days earlier.

Covers scheduled reboot, stop and terminate including retirement. Does **not**
cover expedited hardware events or network maintenance.

### Alarm on playback errors

Must be in **us-east-1** — that is the only region CloudFront publishes metrics to.

```bash
aws cloudwatch put-metric-alarm --region us-east-1 \
  --alarm-name ull-origin-playback-5xx \
  --namespace AWS/CloudFront --metric-name 5xxErrorRate \
  --dimensions Name=DistributionId,Value=<playback-distribution-id> Name=Region,Value=Global \
  --statistic Average --period 300 --evaluation-periods 1 --threshold 5 \
  --comparison-operator GreaterThanThreshold \
  --treat-missing-data notBreaching \
  --alarm-actions <sns-topic-arn>
```

It is a **rate**, so at low viewership it reports insufficient data rather than
alarming. The origin-side counters above fire regardless of viewer traffic and
generally earlier, because they see causes rather than symptoms.

### Consider EC2 auto-recovery

Nothing currently restarts a failed host. A `StatusCheckFailed_System` alarm with
a `recover` action closes that gap and costs nothing.

## Security — read before going live

**There is no authentication on ingest.** Anyone who can reach the content port
from a permitted address can PUT or DELETE segments in your live stream.

- `direct-ingest` restricts writes to `EncoderCidr`. Use a `/32`. The template
  rejects `0.0.0.0/0`.
- **Ingest is plaintext HTTP.** On the public internet the feed is readable in
  transit and injectable. If the encoder is on-premises, use Direct Connect or a
  VPN. TLS on the origin needs a real domain plus a publicly trusted certificate,
  because CloudFront rejects a self-signed certificate on a custom origin — worth
  doing, and a separate piece of work.
- Playback is restricted to CloudFront's origin-facing prefix list, GET/HEAD/
  OPTIONS only.
- Access is via SSM Session Manager. No inbound SSH.

**Long segment cache TTLs (6 h) are only safe while segment names carry a
per-run token.** If names are ever recycled, viewers are served stale video —
worse than a cache miss. Verify your naming before raising `SegmentCacheSeconds`.

## Verifying the binary

The build is reproducible: vendored dependencies, `-trimpath`, pinned compiler.
Rebuild from `src/` and compare:

```bash
cd src
docker run --rm -v "$PWD":/s -w /s \
  -e GOFLAGS=-mod=vendor -e GOPROXY=off -e GOCACHE=/tmp/gc -e GOTMPDIR=/tmp \
  -e CGO_ENABLED=0 -e GOOS=linux -e GOARCH=arm64 \
  golang:1.23 go build -trimpath -ldflags "-s -w -X main.gitSHA=<sha>" -o /s/verify main.go
sha256sum verify ../bin/go-chunked-streaming-server-arm64   # must match
```

`CHANGES-VS-UPSTREAM.txt` lists our changes as commits against upstream
`mjneil/go-chunked-streaming-server@fa4b736`. Upstream is MIT; so is this.
