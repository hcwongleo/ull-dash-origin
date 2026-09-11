# cdn-ingest — encoder via CloudFront + Lambda@Edge

```
Elemental Live ──chunked PUT──▶ ingest CloudFront ──▶ EC2 origin
                                 [Lambda@Edge, *.mpd only]      │
                                                 playback CloudFront ──▶ players
```

**Testing only — do not use for production.** Its purpose is to measure the
ingest CDN's effect on chunk delivery, and to exercise the Lambda@Edge manifest
enrichment. `direct-ingest` is the production path.

Two stacks in two regions; deploy order matters, because the Lambda@Edge stack
must exist before the origin stack can reference its version ARN.

| | Region | Template |
|---|---|---|
| 1 | **us-east-1** | `manifest-lambda-stack.yaml` — Lambda@Edge only replicates from there |
| 2 | ap-east-1 | `origin-stack.yaml` |

## What the Lambda does

Rewrites the manifest as the encoder PUTs it, on `*.mpd` **only** — never on
segments, where `IncludeBody` would buffer the chunked body and destroy
read-while-write. It adds:

- `UTCTiming`, only if absent — without a timing source a player trusts the local
  device clock, mis-computes the live edge and jumps gaps.
- `ServiceDescription` with `Latency@target` and a `PlaybackRate` range, which
  Elemental Live does not emit.

It is **fail-open**: any exception, any truncated body, anything unrecognisable,
and the request passes through untouched. The failure mode is "manifest not
enriched", never "ingest broken".

`lambda/ingest-manifest-enrich.py` is the source of truth. The template's inline
code is generated from it:

```bash
python3 lambda/build-template.py          # regenerate
python3 lambda/build-template.py --check  # fail if they have drifted
```

**Never edit the code inside the YAML.** The generator also stamps the source hash
into the Lambda version description, which is what forces a new version — without
it, a code change deploys as `UPDATE_COMPLETE` while the edge keeps running the
old version.

## Known issue: the latency target conflicts with the encoder

The Lambda injects `Latency target="4000"` while the encoder sends
`suggestedPresentationDelay="PT3S"`. A player given two different targets hunts
between them, which shows up as fluctuating live latency.

Fix: set `LatencyTargetMs=3000` to match, or better, have the Lambda read
`suggestedPresentationDelay` from the manifest it already has in hand so the
encoder remains the single source of truth. Deploys with no stream impact.

## Ingest authorisation is weak here

The security group must allow write methods from CloudFront's origin-facing
prefix list, so **any CloudFront distribution can PUT or DELETE** segments in your
stream. This was accepted by the customer as a known risk. `direct-ingest` does
not have this property — it restricts writes to the encoder's address.
