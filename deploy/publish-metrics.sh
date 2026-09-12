#!/bin/bash
# Publish the origin's internal metrics to CloudWatch.
#
# WHY THIS EXISTS
#
# /-/metrics is bound to 127.0.0.1, so the only numbers worth alarming on are
# unreachable from outside the box. EC2 publishes no memory metric at all without
# an agent, and the CloudFront 5xxErrorRate alarm is a RATE, which reports
# insufficient data at low viewership. So today a slow problem is invisible until
# the process is OOM-killed - and that kill empties RAM, taking the initialisation
# segments with it, which then needs an encoder restart.
#
# WHY NOT THE CLOUDWATCH AGENT
#
# It is a separate daemon with its own configuration file and update cycle, on a
# host whose entire appeal is that it runs one static binary. Three numbers do not
# justify it.
#
# Deliberately fail-soft: every failure path exits 0. A monitoring script that
# takes the service down with it is worse than no monitoring, and this runs as a
# timer that must not enter a failed state and stop firing.

set -uo pipefail

ADMIN=${ADMIN_URL:-http://127.0.0.1:9095/-/metrics}
NAMESPACE=${METRIC_NAMESPACE:-ULLOrigin}

log() { logger -t publish-metrics "$*"; }

IMDS_TOKEN=$(curl -sf -X PUT "http://169.254.169.254/latest/api/token" \
  -H "X-aws-ec2-metadata-token-ttl-seconds: 60" --max-time 3 2>/dev/null) || IMDS_TOKEN=""
imds() {
  curl -sf --max-time 3 -H "X-aws-ec2-metadata-token: $IMDS_TOKEN" \
    "http://169.254.169.254/latest/meta-data/$1" 2>/dev/null
}

INSTANCE_ID=$(imds instance-id)
REGION=$(imds placement/region)
[ -z "$INSTANCE_ID" ] && { log "no instance id from IMDS, skipping"; exit 0; }
[ -z "$REGION" ] && { log "no region from IMDS, skipping"; exit 0; }

SCRAPE=$(curl -sf --max-time 5 "$ADMIN") || {
  # The origin is not answering. Deliberately do NOT publish a zero here: a zero
  # is indistinguishable from a healthy reading, and would mask the outage rather
  # than reveal it. Absent data is the honest signal, and the alarms below treat
  # missing data as breaching.
  log "origin admin endpoint not answering, publishing nothing"
  exit 0
}

# Pull one gauge out of the Prometheus text format, ignoring HELP/TYPE lines.
metric() { awk -v k="$1" '$1==k {print $2; exit}' <<<"$SCRAPE"; }

HEAP=$(metric go_memstats_heap_alloc_bytes)
GOROUTINES=$(metric go_goroutines)
INGEST_AGE=$(metric gcss_last_ingest_age_seconds)
FILES=$(metric gcss_files)
SWEPT=$(metric gcss_swept_total)
ABORTED=$(metric gcss_put_aborted_total)

# -1 means "no ingest yet since start". Publishing it as a negative would make the
# graph nonsense, so it is dropped rather than translated into a fake value.
[ "${INGEST_AGE:-}" = "-1" ] && INGEST_AGE=""

DIMS="Name=InstanceId,Value=$INSTANCE_ID"
DATA=""
add() {
  [ -z "${2:-}" ] && return 0
  DATA="$DATA MetricName=$1,Value=$2,Unit=$3,Dimensions=$DIMS"
}

add HeapAllocBytes      "${HEAP:-}"       Bytes
add Goroutines          "${GOROUTINES:-}" Count
add LastIngestAgeSeconds "${INGEST_AGE:-}" Seconds
add FilesHeld           "${FILES:-}"      Count
add SweptTotal          "${SWEPT:-}"      Count
add PutAbortedTotal     "${ABORTED:-}"    Count

[ -z "$DATA" ] && { log "no metrics parsed from $ADMIN, publishing nothing"; exit 0; }

# shellcheck disable=SC2086
if ! aws cloudwatch put-metric-data --region "$REGION" --namespace "$NAMESPACE" \
       --metric-data $DATA 2>/dev/null; then
  log "put-metric-data failed (check cloudwatch:PutMetricData on the instance role)"
  exit 0
fi

exit 0
