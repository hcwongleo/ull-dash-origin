#!/bin/bash
# Install or upgrade the ULL-DASH origin. Idempotent: safe to re-run.
#
# This lives in the release archive rather than inside CloudFormation UserData on
# purpose. UserData runs ONLY at first boot, so if the install logic lives there,
# upgrading a running instance and provisioning a new one are two different code
# paths that drift apart. Here they are the same script.
#
#   ./install.sh              install or upgrade, then restart
#   ./install.sh --no-restart stage the files, leave the running process alone
#
# WARNING: restarting clears the in-memory content, including initialisation
# segments. Elemental Live sends those only when an output group starts, so after
# a restart you MUST restart the encoder output or new viewers cannot play.
# --no-restart exists so you can stage a build and choose the moment.

set -euo pipefail

PREFIX=/opt/gochunked
SERVICE=gochunked
RESTART=1
[ "${1:-}" = "--no-restart" ] && RESTART=0

here=$(cd "$(dirname "$0")" && pwd)

case "$(uname -m)" in
  aarch64|arm64) arch=arm64 ;;
  x86_64|amd64)  arch=amd64 ;;
  *) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac

binary="$here/bin/go-chunked-streaming-server-$arch"
[ -f "$binary" ] || { echo "missing $binary" >&2; exit 1; }

# Verify the artefact before installing it. A truncated download that still
# exits 0 is a real failure mode for S3 pulls at boot.
if [ -f "$here/SHA256SUMS" ] && command -v sha256sum >/dev/null 2>&1; then
  (cd "$here" && sha256sum -c --quiet --ignore-missing SHA256SUMS) \
    || { echo "checksum mismatch - refusing to install" >&2; exit 1; }
  echo "checksums verified"
fi

# Unprivileged service account. The origin is reachable from the internet via
# CloudFront, so it should not be root.
id "$SERVICE" >/dev/null 2>&1 || \
  useradd --system --no-create-home --shell /sbin/nologin "$SERVICE"

install -d -m 0755 "$PREFIX/bin"
install -d -o "$SERVICE" -g "$SERVICE" -m 0755 "$PREFIX/content"

# Keep the previous binary. Rollback is then a move and a restart, which is the
# only thing you want to be doing at 03:00.
if [ -f "$PREFIX/bin/go-chunked-streaming-server" ]; then
  cp -a "$PREFIX/bin/go-chunked-streaming-server" \
        "$PREFIX/bin/go-chunked-streaming-server.previous"
fi

install -o root -g root -m 0755 "$binary" "$PREFIX/bin/go-chunked-streaming-server"
install -o root -g root -m 0644 "$here/deploy/cors.json" "$PREFIX/cors.json"
install -m 0644 "$here/deploy/$SERVICE.service" "/etc/systemd/system/$SERVICE.service"

systemctl daemon-reload
systemctl enable "$SERVICE" >/dev/null 2>&1 || true

version=$("$PREFIX/bin/go-chunked-streaming-server" -h 2>&1 | head -1 || true)
echo "installed $arch binary to $PREFIX/bin/"

if [ "$RESTART" = "1" ]; then
  systemctl restart "$SERVICE"
  sleep 2
  systemctl is-active "$SERVICE"
  echo
  curl -s --max-time 4 http://127.0.0.1:9095/-/healthz || echo "admin endpoint not answering yet"
  echo
  echo "NOTE: in-memory content was cleared. Restart the Elemental output group"
  echo "      or new viewers will get 404 on the initialisation segments."
else
  echo "staged only; run: systemctl restart $SERVICE"
fi
