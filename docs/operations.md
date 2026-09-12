# ULL-DASH origin — deployment

A read-while-write DASH origin. It serves a segment **while the encoder is still
uploading it**, which is what makes ~4 s live latency achievable; a conventional
whole-file origin cannot go below one segment duration.

Everything here runs in **your** account. Nothing depends on the supplier's.

## What you need

One file: **`origin-stack.yaml`**, from the root of the repository. The template
builds the server on the instance from a pinned git tag at first boot, so there is
nothing to compile, upload or stage.

## Deploy

See the [README](../README.md#deploy): download
`origin-stack.yaml`, upload it in the CloudFormation
console, enter your Elemental Live public IP, create. About 15 minutes.

The template builds the server from a pinned git tag at first boot. Dependencies
are vendored and the module proxy is disabled, so the only network dependency is
this repository — a later change upstream cannot alter what you deployed.

**Deploying by CLI instead:**

```bash
aws cloudformation deploy --region ap-east-1 \
  --stack-name ull-origin \
  --template-file origin-stack.yaml \
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

## If something looks wrong — start here

**Do not reboot the server.** For the most common problem, rebooting makes it worse.
There are two buttons, and the first one tells you which one you need.

### Step 1 — always do this first

AWS console → **Systems Manager** → **Run Command** → **Run command** →
choose the document ending **`1-CHECK-stream-status`** → pick the origin instance →
**Run**.

It changes nothing. It is safe to run at any time, as often as you like. Open the
output and it will say one of four things:

| It says | What it means | What you do |
|---|---|---|
| **OK — the stream is healthy** | The server is fine | The problem is not here. Check the player, or the dashboard for viewer errors |
| **no video is arriving from the encoder** | The server is waiting for video | **Check Elemental Live.** Its output has stopped. Do *not* touch this server |
| **NEW viewers cannot start** | A start-up file is missing after a restart | **Restart the output group in Elemental Live.** Do *not* reboot — that makes it worse |
| **this server is not responding** | The software has stopped | Go to step 2 |

Three of those four answers do **not** involve touching this server. That is why you
run the check first.

### Step 2 — only if the check told you to

Same place: **Run Command** → the document ending **`2-RESTART-origin-service`** →
same instance → **Run**. Takes about 5 seconds.

**Then restart the output group in Elemental Live.** The restart clears this
server's memory, so until you do, people already watching are fine but **new
viewers cannot start**. The document reminds you.

Then run the check again to confirm.

### Why not just reboot?

A reboot and a service restart have the **same** effect on the stream — both clear
the server's memory, and both need the Elemental output restarted afterwards. But
a reboot takes 1–2 minutes and a restart takes 5 seconds.

So a reboot is slower for no benefit. Only reboot if someone technical tells you to.

### The one trap worth remembering

If **people already watching are fine but new viewers cannot start**, that means the
server restarted at some point. The fix is *entirely* in Elemental Live — restart
the output group. Rebooting or restarting this server again will clear its memory
once more and leave you in exactly the same place.

### What is watched automatically

You do not need to poll anything. Emails arrive at the address subscribed to the
alarm topic when:

| Alarm | Means |
|---|---|
| `ingest-stale` | no video arriving for over a minute, or the server stopped answering |
| `heap` | memory unexpectedly high |
| `goroutines` | requests are getting stuck |
| `cpu` | the server is working far harder than it should |
| `host-check-failed` | AWS hardware problem. **After it recovers, run the check** — memory will have been cleared |

The software also restarts itself if it crashes (about 2 seconds) or stops
responding (within about a minute), without anyone doing anything.

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

### Why there is no CloudFront alarm

CloudWatch alarms are regional and CloudFront publishes its metrics **only** to
us-east-1 — verified: `AWS/CloudFront` has 0 metrics in ap-east-1 and 100 in
us-east-1. An alarm in the origin's region would sit in `OK` forever and never
fire, which is worse than none because it looks like coverage.

Putting one in us-east-1 means a second stack, a second SNS topic and a second
email confirmation, for a single metric that is a **rate** — so it is silent until
you have an audience. The origin-side alarms have no such blind spot: if the origin
stops answering, `ingest-stale` fires, because the publisher deliberately sends
nothing and missing data is treated as breaching.

CloudFront errors remain fully visible on the dashboard, which *can* graph across
regions. You lose the notification, not the visibility.

### EC2 auto-recovery is already on

Nothing to do. Verified on both instances: `MaintenanceOptions.AutoRecovery` is
`default`, which AWS enables on supported instance types. If the underlying host
fails, AWS migrates the instance to new hardware keeping the **same instance ID,
private IP and Elastic IP** — so the encoder's destination never changes.

Two things it does *not* do. It does not notify, which is what the
`host-check-failed` alarm is for. And recovery is a stop/start, so **memory is
cleared** — run the CHECK document afterwards, because new viewers may need the
Elemental output restarted.

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
