"""
Lambda@Edge function - enriches DASH .mpd manifests on the INGEST (PUT) path of
the go-chunked origin's ingest CloudFront distribution.

Architecture:
  Elemental Live -> CloudFront (ingest) -> [this Lambda, *.mpd only] -> EC2 origin
                                              origin-request, IncludeBody
  segments        -> CloudFront (ingest) ------------------------------> EC2 origin
                                              NO Lambda: IncludeBody would buffer

Derived from utc-timing-object-lambda.py, which is the S3 Object Lambda form of
the customer's original ingest Lambda@Edge UTCTiming pattern. This is that pattern
returned to the ingest path, where the origin can serve a partial object and so
the read-path workaround is unnecessary.

Deliberately keeps the conventions of the original:

  - String injection, never an XML parse, so a quirky declaration or namespace can
    never break it.
  - Insertion before the first <Period, falling back to before </MPD>. MPD children
    are meant to appear in schema order; UTCTiming before Period is what the
    original does and dash.js reads it regardless of sibling order.
  - Idempotent: an element already present is left alone.
  - Fail-safe: any error returns the ORIGINAL bytes untouched. The worst case is
    the pre-existing un-enriched state, never a broken manifest and never a failed
    ingest.

Two differences from the S3 form, both because THIS origin does read-while-write:

  - availabilityTimeOffset is NOT overridden. The S3 pipeline must zero it because
    S3 only makes an object readable once the PUT completes, so the encoder's
    "fetchable 1.8s early" is untrue there. Here it is true, and zeroing it would
    discard the latency the origin exists to provide.
  - timeShiftBufferDepth is NOT rewritten. The origin's retention cap bounds the
    window, so advertising a longer one would promise segments already reclaimed.

Adds ServiceDescription as well as UTCTiming. Elemental Live emits neither.
ServiceDescription gives the player an explicit convergence target and permission
to drift its playback rate slightly, rather than stalling or hard-seeking.
"""
import base64
import re

UTC_TIMING_SCHEME = 'urn:mpeg:dash:utc:http-iso:2014'
UTC_TIMING_VALUE = '@@UTC_TIMING_VALUE@@'

LATENCY_TARGET_MS = '@@LATENCY_TARGET_MS@@'
LATENCY_MIN_MS = '@@LATENCY_MIN_MS@@'
LATENCY_MAX_MS = '@@LATENCY_MAX_MS@@'

# Playback rate bounds let the player converge on the latency target smoothly.
PLAYBACK_RATE_MIN = '0.96'
PLAYBACK_RATE_MAX = '1.04'


def lambda_handler(event, context):
    """Lambda@Edge origin-request entry point. Never raises."""
    request = event['Records'][0]['cf']['request']
    try:
        return _enrich_request(request)
    except Exception as e:  # noqa: BLE001
        # Last-resort fail-safe: pass the request through untouched rather than
        # failing an ingest. Worst case = manifest not enriched.
        print('manifest enrichment error, passing through: %r' % e)
        return request


def _enrich_request(request):
    if request.get('method') not in ('PUT', 'POST'):
        return request

    body = request.get('body') or {}
    data = body.get('data')
    if not data:
        return request

    # A truncated body must never be written back: replacing it would store a
    # manifest with its tail cut off. Lambda@Edge caps the body it exposes, so
    # this is the guard that keeps an oversized manifest safe.
    if body.get('truncated'):
        print('body truncated, passing through untouched')
        return request

    if body.get('encoding') == 'base64':
        try:
            raw = base64.b64decode(data).decode('utf-8')
        except Exception:  # noqa: BLE001
            return request
    else:
        raw = data

    out = inject(raw)
    if out == raw:
        return request

    request['body'] = {
        'action': 'replace',
        'encoding': 'text',
        'data': out,
    }
    return request


def inject(body):
    """Return body with ServiceDescription and UTCTiming added where absent.

    Pure string work, mirroring utc-timing-object-lambda.py. Returns the input
    unchanged if it is not a recognisable MPD or if nothing needs adding.
    """
    if '<MPD' not in body:
        return body

    elements = ''

    if 'ServiceDescription' not in body:
        elements += (
            '<ServiceDescription id="0">'
            '<Latency target="%s" min="%s" max="%s" referenceId="0"/>'
            '<PlaybackRate min="%s" max="%s"/>'
            '</ServiceDescription>'
            % (LATENCY_TARGET_MS, LATENCY_MIN_MS, LATENCY_MAX_MS,
               PLAYBACK_RATE_MIN, PLAYBACK_RATE_MAX)
        )

    if 'UTCTiming' not in body:
        elements += (
            '<UTCTiming schemeIdUri="%s" value="%s"/>'
            % (UTC_TIMING_SCHEME, UTC_TIMING_VALUE)
        )

    if not elements:
        # Already enriched, e.g. a re-PUT of the same manifest.
        return body

    if '<Period' in body:
        return body.replace('<Period', elements + '\n  <Period', 1)

    if '</MPD>' in body:
        return body.replace('</MPD>', '  ' + elements + '\n</MPD>', 1)

    # Not a recognisable MPD -> untouched. Never emit a half-edited manifest.
    return body
