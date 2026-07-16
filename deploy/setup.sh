#!/usr/bin/env bash
# bitch — provision & harden a fresh Hetzner CAX21 (Ubuntu LTS) box.
# Build phase 1 from DESIGN.md §13 (§4.5, §4.6, §5). Run as root on the box:
#
#   scp -r bitch/ root@BOX:/root/  &&  ssh root@BOX 'bash /root/bitch/deploy/setup.sh'
#
# Idempotent-ish: safe to re-run. It does the IN-BOX hardening; the Hetzner
# Cloud Firewall (egress limits, SSH-from-your-IP) is enforced OUTSIDE the VM
# and must be set in the Hetzner console / hcloud CLI — see the note at the end.
set -euo pipefail

ADMIN_USER="${ADMIN_USER:-admin}"        # non-root SSH user (§4.5)
GO_VERSION="${GO_VERSION:-1.26.3}"
# Pinned: the bridge is coupled to the CLI's stream-json shapes, hook contract,
# and flags. Upgrade deliberately (bump here, rebuild, retest) — never track
# npm latest. The bridge preflights `claude --version` at boot.
CLAUDE_VERSION="${CLAUDE_VERSION:-2.1.210}"
# Go release arch, auto-detected from the box (CAX=arm64, CX/CPX=amd64, §5).
# Override with ARCH=… if needed.
case "${ARCH:-$(dpkg --print-architecture 2>/dev/null || uname -m)}" in
    arm64|aarch64) ARCH="arm64" ;;
    amd64|x86_64)  ARCH="amd64" ;;
    *) echo "unsupported arch: ${ARCH:-unknown}"; exit 1 ;;
esac
REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

log() { printf '\n\033[1;36m== %s ==\033[0m\n' "$*"; }

require_root() { [ "$(id -u)" -eq 0 ] || { echo "run as root"; exit 1; }; }
require_root

log "System update + base packages"
export DEBIAN_FRONTEND=noninteractive
apt-get update -y
apt-get upgrade -y
apt-get install -y --no-install-recommends \
    git curl ca-certificates ufw fail2ban unattended-upgrades \
    build-essential jq

log "Unattended security upgrades (§4.5)"
dpkg-reconfigure -f noninteractive unattended-upgrades || true
cat >/etc/apt/apt.conf.d/20auto-upgrades <<'EOF'
APT::Periodic::Update-Package-Lists "1";
APT::Periodic::Unattended-Upgrade "1";
EOF

log "Admin user for SSH: ${ADMIN_USER} (§4.5)"
if ! id "$ADMIN_USER" &>/dev/null; then
    adduser --disabled-password --gecos "" "$ADMIN_USER"
    usermod -aG sudo "$ADMIN_USER"
fi
# Passwordless sudo for the admin user. Root SSH login is disabled below, and
# admin has no password (key-only login), so without this nobody could perform
# root actions over SSH — the box would be unadministerable. Access is already
# bounded by key-only SSH from your IP(s) via the Hetzner Cloud Firewall (§4.5).
cat >/etc/sudoers.d/90-"${ADMIN_USER}" <<EOF
${ADMIN_USER} ALL=(ALL) NOPASSWD:ALL
EOF
chmod 440 /etc/sudoers.d/90-"${ADMIN_USER}"
# Copy root's authorized_keys to the admin user if present, so you keep access.
if [ -f /root/.ssh/authorized_keys ]; then
    install -d -m 700 -o "$ADMIN_USER" -g "$ADMIN_USER" "/home/$ADMIN_USER/.ssh"
    install -m 600 -o "$ADMIN_USER" -g "$ADMIN_USER" \
        /root/.ssh/authorized_keys "/home/$ADMIN_USER/.ssh/authorized_keys"
fi

log "SSH hardening: key-only, no root login (§4.5, §4.6)"
install -d -m 755 /etc/ssh/sshd_config.d
cat >/etc/ssh/sshd_config.d/10-bitch.conf <<'EOF'
PermitRootLogin no
PasswordAuthentication no
KbdInteractiveAuthentication no
ChallengeResponseAuthentication no
EOF
systemctl reload ssh || systemctl reload sshd || true

log "Host firewall (ufw) — secondary to the Hetzner Cloud Firewall"
# ufw is a defence-in-depth layer only; a root compromise can flush it, which is
# exactly why the Hetzner Cloud Firewall (enforced outside the VM) is the real
# control (§4.5). No inbound app ports — long-poll is outbound (§3).
ufw --force reset
ufw default deny incoming
ufw default allow outgoing
ufw allow OpenSSH
ufw --force enable

log "fail2ban"
systemctl enable --now fail2ban

log "Go ${GO_VERSION} (${ARCH})"
if ! command -v go >/dev/null || [ "$(go version 2>/dev/null | awk '{print $3}')" != "go${GO_VERSION}" ]; then
    curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${ARCH}.tar.gz" -o /tmp/go.tgz
    rm -rf /usr/local/go && tar -C /usr/local -xzf /tmp/go.tgz
    ln -sf /usr/local/go/bin/go /usr/local/bin/go
    ln -sf /usr/local/go/bin/gofmt /usr/local/bin/gofmt
fi

log "Node.js LTS + Claude Code CLI ${CLAUDE_VERSION} (pinned)"
if ! command -v node >/dev/null; then
    curl -fsSL https://deb.nodesource.com/setup_lts.x | bash -
    apt-get install -y nodejs
fi
if ! command -v claude >/dev/null || ! claude --version 2>/dev/null | grep -q "^${CLAUDE_VERSION}"; then
    npm install -g "@anthropic-ai/claude-code@${CLAUDE_VERSION}"
fi

log "Build bitch"
cd "$REPO_DIR"
/usr/local/bin/go build -o /tmp/bitch ./cmd/bitch
install -d -m 755 /opt/bitch
install -m 755 /tmp/bitch /opt/bitch/bitch
install -d -m 700 /opt/bitch/data

log "Workspace jail root (§4.3)"
install -d -m 755 /srv/work

log "Secrets + systemd unit (§8)"
install -d -m 700 /etc/bitch
if [ ! -f /etc/bitch/bitch.env ]; then
    install -m 600 "$REPO_DIR/deploy/bitch.env.example" /etc/bitch/bitch.env
    echo "  -> EDIT /etc/bitch/bitch.env and fill in your token, user ID, and API key."
fi
install -m 644 "$REPO_DIR/deploy/bitch.service" /etc/systemd/system/bitch.service
systemctl daemon-reload
systemctl enable bitch

cat <<EOF

$(log "Done — remaining manual steps")
1. Edit /etc/bitch/bitch.env (token, BITCH_ALLOWED_USERS, ANTHROPIC_API_KEY).
2. Provision Git credentials for your repos (§4.5). Use a fine-grained PAT
   scoped to just the repos you clone — never your full-access token. The agent
   runs as root with HOME=/root, so store the credential on disk (git reads it
   regardless of the child env, which is built from scratch):
     git config --global credential.helper store
     printf 'https://x-access-token:%s@github.com\n' "\$PAT" \\
       > /root/.git-credentials
     chmod 600 /root/.git-credentials
   (\$PAT = the fine-grained token; contents:read is enough to clone/pull,
   contents:write only if you want the agent to push.) The policy gate blocks
   the agent from cat-ing this file, but root can still reach it — keep the PAT
   scoped so the blast radius stays bounded. Then clone your repos under
   /srv/work. Pushes still go through the approval gate (git push is a risky op).
3. In the Hetzner console (or hcloud CLI), attach a Cloud Firewall to this VM:
     - inbound: SSH (22) from your IP(s) only; no app ports.
     - egress:  restrict outbound to what the agent actually needs. This is the
                one control root cannot flush from inside the box (§4.5, §4.6).
4. Confirm SSH still works as '${ADMIN_USER}@<box>' BEFORE closing this session.
5. Start it:  systemctl start bitch   &&   journalctl -u bitch -f
EOF
