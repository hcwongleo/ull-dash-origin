#!/bin/bash
# Install or upgrade the ULL-DASH origin. Idempotent: safe to re-run.
#
#   ./deploy/install.sh                     use ./go-chunked-streaming-server
#   ./deploy/install.sh /path/to/binary     use a specific build
#   ./deploy/install.sh --no-restart [path] stage only, leave the process running
#
# Lives here rather than inside CloudFormation UserData because UserData runs
# ONLY at first boot. Keeping the logic there would make provisioning a new
# instance and upgrading a running one two code paths that drift apart.
#
# WARNING: restarting clears in-memory content, including initialisation
# segments. Elemental Live sends those only when an output group starts, so after
# a restart the encoder output must be restarted or new viewers cannot play.
# --no-restart exists so a build can be staged and the moment chosen.

set -euo pipefail

PREFIX=/opt/gochunked
SERVICE=gochunked
RESTART=1

if [ "${1:-}" = "--no-restart" ]; then
  RESTART=0
  shift
fi

here=$(cd "$(dirname "$0")" && pwd)          # .../deploy
repo=$(cd "$here/.." && pwd)
binary="${1:-$repo/go-chunked-streaming-server}"

[ -f "$binary" ] || {
  echo "no binary at $binary" >&2
  echo "build one first, or pass its path as an argument" >&2
  exit 1
}
[ -x "$binary" ] || chmod +x "$binary"

# Integrity comes from git: the deployment clones a signed, pinned tag, so the
# commit hash covers the source the binary was built from. A separate checksum
# file would only restate that, and an unverifiable one is worse than none.

# Unprivileged service account. The origin is reachable from the internet via
# CloudFront, so it should not run as root.
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
install -o root -g root -m 0644 "$here/cors.json" "$PREFIX/cors.json"
install -m 0644 "$here/$SERVICE.service" "/etc/systemd/system/$SERVICE.service"

# Metrics publisher and health watchdog. Both are timers, so a failure in either
# cannot take the origin down with it.
install -o root -g root -m 0755 "$here/publish-metrics.sh" "$PREFIX/bin/publish-metrics.sh"
for unit in publish-metrics.service publish-metrics.timer \
            gochunked-healthcheck.service gochunked-healthcheck.timer; do
  install -m 0644 "$here/$unit" "/etc/systemd/system/$unit"
done

systemctl daemon-reload
systemctl enable "$SERVICE" >/dev/null 2>&1 || true
systemctl enable --now publish-metrics.timer >/dev/null 2>&1 || true
systemctl enable --now gochunked-healthcheck.timer >/dev/null 2>&1 || true
echo "installed $(uname -m) binary to $PREFIX/bin/"

if [ "$RESTART" = "1" ]; then
  systemctl restart "$SERVICE"
  sleep 2
  systemctl is-active "$SERVICE"
  curl -s --max-time 4 http://127.0.0.1:9095/-/healthz || echo "admin endpoint not answering yet"
  echo
  echo "NOTE: in-memory content was cleared. Restart the Elemental output group,"
  echo "      or new viewers will get 404 on the initialisation segments."
else
  echo "staged only; run: systemctl restart $SERVICE"
fi
