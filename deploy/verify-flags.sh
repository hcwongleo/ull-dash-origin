#!/bin/bash
# Check that every flag the systemd unit passes actually exists in the binary.
#
# This exists because it did not, once. -wait-timeout-ms was added to the unit file
# while the binary had no such flag, so Go's flag package printed "flag provided but
# not defined" and exited. The service never started, the health check never
# answered, and CloudFormation rolled the whole stack back 15 minutes later - with
# nothing in the failure message to say why.
#
# Run it before tagging a release. It takes seconds and it catches a class of
# failure that otherwise only appears at deploy time.
#
#   ./deploy/verify-flags.sh            build in Docker and check
#   ./deploy/verify-flags.sh /path/bin  check an existing native binary
set -uo pipefail
cd "$(dirname "$0")/.."

if [ $# -ge 1 ]; then
  known=$("$1" -h 2>&1 | grep -oE '^  -[a-z-]+' | tr -d ' ')
else
  # Built AND executed inside the container: the binary is Linux, the workstation
  # may not be.
  known=$(docker run --rm -v "$PWD":/src -w /src \
    -e GOFLAGS=-mod=vendor -e GOPROXY=off -e GOCACHE=/tmp/gc -e GOTMPDIR=/tmp \
    golang:1.23 sh -c 'go build -o /tmp/vb main.go && /tmp/vb -h 2>&1' \
    | grep -oE '^  -[a-z-]+' | tr -d ' ')
fi

if [ -z "$known" ]; then
  echo "could not determine the binary's flags - build failed?" >&2
  exit 1
fi

passed=$(grep -oE '^[[:space:]]+-[a-z-]+' deploy/gochunked.service | tr -d ' ')

bad=0
for f in $passed; do
  printf '%s\n' "$known" | grep -qxF -- "$f" || { echo "MISSING FROM BINARY: $f"; bad=1; }
done

if [ "$bad" != 0 ]; then
  echo
  echo "The unit file passes a flag the binary does not define. The service would exit"
  echo "immediately on start, and the deploy would roll back with no useful message."
  exit 1
fi

echo "ok: all $(printf '%s\n' "$passed" | wc -l | tr -d ' ') unit-file flags exist in the binary"
