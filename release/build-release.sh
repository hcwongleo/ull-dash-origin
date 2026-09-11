#!/bin/bash
# Build a self-contained release archive for handover to a customer.
#
# Produces ONE file the customer uploads to their own S3 bucket. Nothing in the
# deployment then depends on our AWS account, which matters: cross-account S3
# reads mean bucket policies and IAM on both sides, and an artefact that lives in
# a supplier's account is a supply-chain dependency a customer security review
# will rightly object to.
#
#   ./release/build-release.sh              build from origin/ HEAD
#   ./release/build-release.sh <git-ref>    build a specific commit
#
# Requires Docker. There is deliberately no assumption of a Go toolchain on the
# workstation: the container pins the compiler version, so the build is
# reproducible by anyone, including the customer.

set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
# The Go source is the repository root: the server and the deployment templates
# live in one repo so that a boot-time build clones a single thing.
originsrc="$root"
ref="${1:-HEAD}"

[ -d "$originsrc/.git" ] || { echo "no git repo at $originsrc" >&2; exit 1; }

sha=$(git -C "$originsrc" rev-parse --short "$ref")
name="gcss-origin-$sha"
stage="$root/release/.stage/$name"

echo "building $name from $originsrc @ $ref"
rm -rf "$root/release/.stage"
mkdir -p "$stage/bin" "$stage/deploy" "$stage/cloudformation"

# -trimpath plus vendored dependencies plus a pinned compiler makes the build
# reproducible: the customer can rebuild from source and get identical bytes,
# which is how they verify the binary matches the source they reviewed.
for arch in arm64 amd64; do
  echo "  compiling linux/$arch"
  docker run --rm -v "$originsrc":/src -w /src \
    -e GOFLAGS=-mod=vendor -e GOPROXY=off -e GOCACHE=/tmp/gc -e GOTMPDIR=/tmp \
    -e CGO_ENABLED=0 -e GOOS=linux -e GOARCH="$arch" \
    golang:1.23 go build -trimpath -ldflags "-s -w -X main.gitSHA=$sha" \
      -o "/src/release-$arch" main.go
  mv "$originsrc/release-$arch" "$stage/bin/go-chunked-streaming-server-$arch"
done

cp "$originsrc/deploy/gochunked.service" "$stage/deploy/"
cp "$originsrc/deploy/cors.json"         "$stage/deploy/"
cp "$root/release/install.sh"            "$stage/"
chmod +x "$stage/install.sh"

cp "$root/pipelines/direct-ingest/origin-stack.yaml" \
   "$stage/cloudformation/direct-ingest.yaml"
cp "$root/pipelines/cdn-ingest-testing/origin-stack.yaml" \
   "$stage/cloudformation/cdn-ingest-testing.yaml"
cp "$root/pipelines/cdn-ingest-testing/manifest-lambda-stack.yaml" \
   "$stage/cloudformation/cdn-ingest-testing-lambda.yaml"

cp "$root/release/CUSTOMER-DEPLOY.md" "$stage/README.md"

# Full source, so the customer can review and rebuild. MIT upstream, so this is
# permitted and it is what makes the binary auditable rather than opaque.
git -C "$originsrc" archive --format=tar --prefix=src/ "$ref" \
  | tar xf - -C "$stage"
git -C "$originsrc" log --oneline fa4b736.."$ref" > "$stage/CHANGES-VS-UPSTREAM.txt"

(cd "$stage" && find . -type f ! -name SHA256SUMS -exec sha256sum {} + \
  | sed 's| \./| |' > SHA256SUMS)

(cd "$root/release/.stage" && zip -qr "$root/release/$name.zip" "$name")
rm -rf "$root/release/.stage"

echo
echo "release/$name.zip"
unzip -l "$root/release/$name.zip" | tail -1
echo "sha256: $(sha256sum "$root/release/$name.zip" | cut -d' ' -f1)"
echo
echo "Hand over the zip AND that sha256 by a separate channel, so the customer"
echo "can verify the archive was not modified in transit."
