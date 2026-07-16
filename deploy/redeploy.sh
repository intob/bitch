#!/usr/bin/env bash
# deploy/redeploy.sh — fast, safe redeploy of the bitch binary to the live box.
#
# Run from the repo root (or anywhere — it locates itself):  ./deploy/redeploy.sh
#
# What it does: runs the test suite, cross-compiles for the box's architecture,
# ships the single binary over SSH, installs it, restarts the service, and
# verifies it came back online — auto-rolling back to the previous binary if it
# didn't. This is the everyday code-change path.
#
# The box's /root/bitch is a plain source tree (scp'd at provisioning, not a git
# clone), so we build here and copy the binary rather than pulling on the box.
# Use deploy/setup.sh (re-run) only when the systemd unit, env schema, or system
# packages change — this script deliberately touches nothing but the binary.
#
# Connection details default to the 2026-07-16 deployment (DEPLOY.md §0); override
# via env for a rebuilt/moved box:
#   BITCH_HOST=1.2.3.4 BITCH_SSH_USER=admin BITCH_SSH_KEY=~/.ssh/bitch_hetzner ./deploy/redeploy.sh
set -euo pipefail

HOST="${BITCH_HOST:-<box-ip>}"
SSH_USER="${BITCH_SSH_USER:-admin}"
SSH_KEY="${BITCH_SSH_KEY:-$HOME/.ssh/bitch_hetzner}"
SSH=(ssh -o ConnectTimeout=15 -i "$SSH_KEY" "${SSH_USER}@${HOST}")

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_DIR"

echo "==> preflight: build, vet, test (must be green before shipping)"
go build ./... >/dev/null
go vet ./... >/dev/null
go test ./... >/dev/null

echo "==> detecting box architecture"
ARCH="$("${SSH[@]}" uname -m)"
case "$ARCH" in
	x86_64) GOARCH=amd64 ;;
	aarch64 | arm64) GOARCH=arm64 ;;
	*)
		echo "unknown box arch: $ARCH" >&2
		exit 1
		;;
esac
echo "    box is $ARCH -> building linux/$GOARCH"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
CGO_ENABLED=0 GOOS=linux GOARCH="$GOARCH" go build -trimpath -o "$TMP/bitch" ./cmd/bitch

echo "==> shipping binary ($(du -h "$TMP/bitch" | cut -f1))"
scp -q -i "$SSH_KEY" "$TMP/bitch" "${SSH_USER}@${HOST}:/tmp/bitch.new"

echo "==> installing + restarting (rolls back on failure)"
"${SSH[@]}" 'sudo bash -s' <<'REMOTE'
set -euo pipefail
[ -x /opt/bitch/bitch ] && cp -a /opt/bitch/bitch /opt/bitch/bitch.bak || true
install -m 755 /tmp/bitch.new /opt/bitch/bitch
rm -f /tmp/bitch.new
systemctl restart bitch
# Wait for the service to settle, then confirm it actually printed its online
# banner (Restart=always would mask a crash-loop, so is-active alone isn't enough).
for _ in $(seq 1 8); do
	sleep 1
	systemctl is-active --quiet bitch && break
done
if systemctl is-active --quiet bitch && journalctl -u bitch -n 40 --no-pager | grep -q "online as @"; then
	journalctl -u bitch -n 40 --no-pager | grep "online as @" | tail -1
else
	echo "!! service did not come online — rolling back to previous binary" >&2
	[ -x /opt/bitch/bitch.bak ] && cp -a /opt/bitch/bitch.bak /opt/bitch/bitch && systemctl restart bitch
	journalctl -u bitch -n 20 --no-pager >&2 || true
	exit 1
fi
REMOTE

echo "==> deployed OK"
