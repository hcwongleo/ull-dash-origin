# direct-ingest — encoder straight to EC2

```
Elemental Live ──chunked PUT (plaintext, IP-restricted)──▶ EC2 origin
                                                              │
                                            playback CloudFront ──▶ players
```

No ingest CloudFront. No Lambda@Edge. One stack, one region.

## Does it actually improve latency?

**Partly known, partly not — and the unknown part is the part that matters.**

| Component removed | Saving | Confidence |
|---|---|---|
| Lambda@Edge on every `.mpd` PUT | ~5–30 ms warm, more on cold start | reasonably confident |
| POP hop (encoder → edge → origin) | ~5–20 ms | reasonably confident |
| TLS terminate + re-originate | few ms | reasonably confident |
| **Chunk coalescing by CloudFront** | **0 – ~500 ms** | **unknown** |

Against a 3–4 s target, the first three are noise. A viewer will not perceive
40 ms. The coalescing question is the only one that could matter.

Where it comes from: an earlier measurement of a chunked PUT through the ingest
distribution saw the origin receiving roughly 51 KB every ~0.5 s. If CloudFront
really is batching the encoder's 0.2 s fragments into 0.5 s writes, direct ingest
recovers up to half a second. But that 0.5 s could equally have been the sampling
interval of the measurement rather than CloudFront's behaviour. **It was never
isolated, so treat it as unresolved.**

### The measurement that settles it

Run both pipelines from the same encoder simultaneously, then compare the
*arrival profile* of one segment at each origin. Coalescing shows up as steps;
true pass-through shows up as a smooth ramp.

On each origin, over SSM:

```bash
# Watch one segment grow as it is ingested. 20ms sampling resolves 0.2s fragments.
SEG=$(curl -s localhost:9094/<stream>.mpd \
      | grep -o 'media="[^"]*"' | head -1)   # resolve $Number$ yourself
for i in $(seq 1 150); do
  printf '%s %s\n' "$(date +%s.%N)" \
    "$(curl -s -o /dev/null -w '%{size_download}' localhost:9094/$SEG)"
  sleep 0.02
done
```

- **Smooth ~0.2 s increments on both** → CloudFront was not coalescing, the gain
  is tens of milliseconds, and direct ingest is a simplification rather than a
  latency win.
- **Steps of ~0.5 s on `cdn-ingest`, ~0.2 s here** → CloudFront was coalescing,
  and this pipeline is worth ~0.5 s. That is a real improvement.

Do not ship a latency claim to the customer without this. The honest current
position is "equal or slightly better, and materially simpler".

## The stronger argument: it is the conventional design

A CDN's value is fan-out to many readers. Ingest has exactly one writer. Putting
CloudFront in front of ingest buys no fan-out and costs a hop, so the usual
architecture puts the CDN on playback only — which is what this does.

## Security: better in one way, worse in another

**Better.** With an ingest CDN, the security group must allow write methods from
CloudFront's entire origin-facing prefix list, so **any CloudFront distribution in
the world can PUT or DELETE segments in your stream.** Here, writes are allowed
only from `EncoderCidr`. That is a large improvement, and the template refuses
`0.0.0.0/0` outright.

**Worse.** Ingest is **plaintext HTTP**. On the public internet the stream is
readable in transit, and anyone who can spoof or reach that address can inject
segments. There is no authentication in the server.

TLS on the origin is deliberately not offered here: the server supports `-c`/`-k`,
but a custom CloudFront origin requires a publicly trusted certificate matching
its domain, which an EC2 public DNS name cannot have. Doing it properly needs a
real domain plus ACM or Let's Encrypt on the instance. That is worth doing and is
a separate change.

**If the encoder is on-premises, use Direct Connect or a VPN.** Do not push a
plaintext live feed across the open internet because a template made it easy.

## What you lose by dropping the Lambda

It injected two things Elemental Live does not emit:

- `UTCTiming` — configure this on the encoder instead. Without a timing source a
  player trusts the local device clock, mis-computes the live edge, and jumps
  gaps. This has already been observed in this project.
- `ServiceDescription` with `Latency@target` and a `PlaybackRate` range — gives
  the player an explicit convergence goal.

Losing the second is arguably a **gain**: the injected 4000 ms target conflicted
with the encoder's own `suggestedPresentationDelay="PT3S"`, and a player told two
different targets hunts between them. With no injection there is exactly one
source of truth — the encoder.

## Deploy

```bash
aws cloudformation deploy --region ap-east-1 \
  --stack-name ch18d-origin-direct \
  --template-file origin-stack.yaml \
  --capabilities CAPABILITY_IAM \
  --parameter-overrides \
    EncoderCidr=<encoder-public-ip>/32 \
    ArtifactBucket=<bucket> \
    ArtifactKey=gcss-min-arm64-<sha>.tgz \
    AmiId=<pinned-al2023-arm64-ami>
```

`AmiId` is pinned on purpose. It was previously an SSM parameter path, and
because `UsePreviousValue` preserves the *path* rather than the resolved value, a
new Amazon Linux release silently replaced a running origin during an unrelated
update — which also reverted the binary, since UserData runs only at first boot.

Then point Elemental at the `IngestDestination` output. **Chunked transfer
encoding must be on**; without it there is no low latency at all, and nothing
reports an error.

Verify it is actually on:

```bash
curl -s -o /dev/null -w '%{time_starttransfer}\n' \
  'https://<playback>/<newest-segment>.mp4'
# ~0.1s = working.  ~2s = not chunked.
```

## Still unfixed here, same as every other pipeline

Init segments live only in RAM, and Elemental sends them only when an output
group starts. **Any restart requires restarting the encoder output** before new
viewers can play.
