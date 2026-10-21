#!/usr/bin/env bash
# bitch — provision, harden, configure and start a fresh Debian/Ubuntu box.
# Build phase 1 from DESIGN.md §13 (§4.5, §4.6, §5). Run as root on the box:
#
#   git clone https://github.com/intob/bitch.git /root/bitch
#   bash /root/bitch/deploy/setup.sh
#
# It asks for the three things it needs (bot token, your Telegram user ID,
# Anthropic auth) up front, then runs unattended. For a non-interactive run,
# pass them as env vars instead of answering prompts:
#
#   BITCH_TELEGRAM_TOKEN=… BITCH_ALLOWED_USERS=… ANTHROPIC_API_KEY=… bash setup.sh
#   (or CLAUDE_CODE_OAUTH_TOKEN=… from `claude setup-token` instead of the API key;
#    optional: GIT_TOKEN=… for a fine-grained GitHub token, see step "Git")
#
# Safe to re-run: an existing /etc/bitch/bitch.env is kept, and only values that
# are missing (or that you pass explicitly) are asked for or rewritten. Re-run it
# when the systemd unit, env schema, or CLI pin changes.
#
# It does the IN-BOX hardening. An egress firewall enforced OUTSIDE the VM (your
# provider's cloud firewall) is the control root can't undo — see the note at
# the end (§4.5, §4.6).
set -euo pipefail

ADMIN_USER="${ADMIN_USER:-admin}"        # non-root SSH user (§4.5)
GO_VERSION="${GO_VERSION:-1.26.3}"
# Pinned: the bridge is coupled to the CLI's stream-json shapes, hook contract,
# and flags. Upgrade deliberately (bump here, rebuild, retest) — never track
# npm latest. The bridge preflights `claude --version` at boot.
CLAUDE_VERSION="${CLAUDE_VERSION:-2.1.210}"
# Go release arch, auto-detected from the box. Override with ARCH=… if needed.
case "${ARCH:-$(dpkg --print-architecture 2>/dev/null || uname -m)}" in
    arm64|aarch64) ARCH="arm64" ;;
    amd64|x86_64)  ARCH="amd64" ;;
    *) echo "unsupported arch: ${ARCH:-unknown}"; exit 1 ;;
esac
REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ENV_FILE=/etc/bitch/bitch.env

log()  { printf '\n\033[1;36m== %s ==\033[0m\n' "$*"; }
warn() { printf '\033[1;33m!! %s\033[0m\n' "$*"; }
die()  { printf '\033[1;31mxx %s\033[0m\n' "$*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || die "run as root"
command -v apt-get >/dev/null || die "this script targets Debian/Ubuntu (apt); see README for running the binary by hand"

# --- Lockout guard ---------------------------------------------------------
# Root SSH login and passwords are disabled below; the admin user inherits
# root's authorized_keys. With no key there, finishing this script locks you out.
if [ ! -s /root/.ssh/authorized_keys ] && [ ! -s "/home/$ADMIN_USER/.ssh/authorized_keys" ] \
    && [ "${ALLOW_NO_SSH_KEY:-}" != 1 ]; then
    die "no SSH key in /root/.ssh/authorized_keys — add yours first (or ALLOW_NO_SSH_KEY=1 if you have console access)"
fi

# --- Collect configuration (before the long, unattended part) --------------
INTERACTIVE=0
if [ -t 0 ] || { [ -r /dev/tty ] && : </dev/tty; } 2>/dev/null; then INTERACTIVE=1; fi

env_get() { [ -f "$ENV_FILE" ] && sed -n "s/^$1=//p" "$ENV_FILE" | tail -n1 || true; }
# Template placeholders from bitch.env.example count as unset.
is_unset() { case "$1" in ""|123456:ABC-your-bot-token|111111111|sk-ant-...) return 0 ;; esac; return 1; }
ask() { # ask VAR "prompt" [secret]
    local __v
    [ "$INTERACTIVE" = 1 ] || return 0
    if [ "${3:-}" = secret ]; then
        read -r -s -p "$2: " __v </dev/tty; echo >/dev/tty
    else
        read -r -p "$2: " __v </dev/tty
    fi
    printf -v "$1" '%s' "$__v"
}
tg_getme() { # prints the bot's @username, or fails. Token goes via stdin, not argv.
    printf 'url = "https://api.telegram.org/bot%s/getMe"\n' "$1" \
        | curl -fsS --max-time 15 -K - 2>/dev/null \
        | sed -n 's/.*"username":"\([^"]*\)".*/\1/p' | grep .
}

log "Configuration"
# Explicit env vars win; otherwise keep what's already in $ENV_FILE; otherwise ask.
TOKEN="${BITCH_TELEGRAM_TOKEN:-}"
USERS="${BITCH_ALLOWED_USERS:-}"
API_KEY="${ANTHROPIC_API_KEY:-}"
OAUTH="${CLAUDE_CODE_OAUTH_TOKEN:-}"
GIT_TOKEN="${GIT_TOKEN:-}"

if [ -z "$TOKEN" ] && is_unset "$(env_get BITCH_TELEGRAM_TOKEN)"; then
    echo "Telegram bot token — create a bot with @BotFather (/newbot)."
    while :; do
        ask TOKEN "  bot token" secret
        [ -n "$TOKEN" ] || break
        [[ "$TOKEN" =~ ^[0-9]+:[A-Za-z0-9_-]+$ ]] && break
        warn "that doesn't look like a bot token (digits:letters)"; TOKEN=""
        [ "$INTERACTIVE" = 1 ] || break
    done
fi
if [ -n "$TOKEN" ] && command -v curl >/dev/null; then
    if BOT_NAME="$(tg_getme "$TOKEN")"; then
        echo "  ✓ token works: @$BOT_NAME"
    else
        die "Telegram rejected that bot token (getMe failed)"
    fi
fi

if [ -z "$USERS" ] && is_unset "$(env_get BITCH_ALLOWED_USERS)"; then
    echo "Your numeric Telegram user ID — message @userinfobot to get it. Comma-separate several."
    ask USERS "  user ID(s)"
fi
if [ -n "$USERS" ] && ! [[ "$USERS" =~ ^[0-9]+(,[0-9]+)*$ ]]; then
    die "BITCH_ALLOWED_USERS must be numeric IDs, comma-separated (not @usernames)"
fi

if [ -z "$API_KEY$OAUTH" ] && is_unset "$(env_get ANTHROPIC_API_KEY)" \
    && is_unset "$(env_get CLAUDE_CODE_OAUTH_TOKEN)"; then
    echo "Anthropic auth — an API key (sk-ant-api…), or a subscription token from"
    echo "running \`claude setup-token\` on your own machine (sk-ant-oat…)."
    ask API_KEY "  key or token" secret
    case "$API_KEY" in sk-ant-oat*) OAUTH="$API_KEY"; API_KEY="" ;; esac
fi

if [ -z "$GIT_TOKEN" ] && [ ! -f /root/.git-credentials ] && [ "$INTERACTIVE" = 1 ]; then
    echo "Optional: a fine-grained GitHub token scoped to the repos the agent may use"
    echo "(contents:read, or contents:write for gated pushes). Enter to skip."
    ask GIT_TOKEN "  GitHub token" secret
fi
echo "Thanks — the rest runs unattended."

# --- System -----------------------------------------------------------------
umask 022
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
chmod 644 /etc/apt/apt.conf.d/20auto-upgrades

log "Admin user for SSH: ${ADMIN_USER} (§4.5)"
if ! id "$ADMIN_USER" &>/dev/null; then
    adduser --disabled-password --gecos "" "$ADMIN_USER"
    usermod -aG sudo "$ADMIN_USER"
fi
# Passwordless sudo for the admin user. Root SSH login is disabled below, and
# admin has no password (key-only login), so without this nobody could perform
# root actions over SSH — the box would be unadministerable. Access is already
# bounded by key-only SSH (ideally from your IPs only, via the external firewall).
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
chmod 644 /etc/ssh/sshd_config.d/10-bitch.conf
systemctl reload ssh || systemctl reload sshd || true

log "Host firewall (ufw) — secondary to an external cloud firewall"
# ufw is a defence-in-depth layer only; a root compromise can flush it, which is
# exactly why a firewall enforced outside the VM is the real control (§4.5).
# No inbound app ports — long-poll is outbound (§3).
ufw --force reset
ufw default deny incoming
ufw default allow outgoing
ufw allow OpenSSH
ufw --force enable

log "fail2ban"
systemctl enable --now fail2ban

# --- Toolchain + build --------------------------------------------------------
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

# --- Secrets + service --------------------------------------------------------
umask 077
log "Secrets: $ENV_FILE (§4.2, §8)"
install -d -m 700 /etc/bitch
[ -f "$ENV_FILE" ] || install -m 600 "$REPO_DIR/deploy/bitch.env.example" "$ENV_FILE"
chmod 600 "$ENV_FILE"

env_set() { # replace the first KEY= line (commented or not) in place, else append
    K="$1" V="$2" awk 'BEGIN { k = ENVIRON["K"]; v = ENVIRON["V"] }
        !done && $0 ~ ("^#? *" k "=") { print k "=" v; done = 1; next }
        { print }
        END { if (!done) print k "=" v }' "$ENV_FILE" >"$ENV_FILE.tmp"
    cat "$ENV_FILE.tmp" >"$ENV_FILE" && rm -f "$ENV_FILE.tmp"
}
env_comment() { sed -i "s/^$1=/# $1=/" "$ENV_FILE"; }

[ -n "$TOKEN" ] && env_set BITCH_TELEGRAM_TOKEN "$TOKEN"
[ -n "$USERS" ] && env_set BITCH_ALLOWED_USERS "$USERS"
# Exactly one auth var stays live: a leftover placeholder API key would win.
if [ -n "$OAUTH" ]; then
    env_set CLAUDE_CODE_OAUTH_TOKEN "$OAUTH"; env_comment ANTHROPIC_API_KEY
elif [ -n "$API_KEY" ]; then
    env_set ANTHROPIC_API_KEY "$API_KEY"; env_comment CLAUDE_CODE_OAUTH_TOKEN
fi
for k in BITCH_TRANSCRIBE_URL BITCH_TRANSCRIBE_KEY BITCH_TRANSCRIBE_MODEL BITCH_MODEL \
         BITCH_EFFORT BITCH_DEFAULT_TIER BITCH_DAILY_BUDGET_USD BITCH_ROOTS; do
    [ -n "${!k:-}" ] && env_set "$k" "${!k}"
done

if [ -n "$GIT_TOKEN" ]; then
    log "Git credentials (§4.5)"
    # The agent runs as root with HOME=/root; git reads this regardless of the
    # child env (which is built from scratch). The policy gate blocks cat-ing it,
    # but root can still reach it — keep the token scoped.
    git config --global credential.helper store
    printf 'https://x-access-token:%s@github.com\n' "$GIT_TOKEN" >/root/.git-credentials
    chmod 600 /root/.git-credentials
fi

log "systemd unit (§8)"
umask 022
install -m 644 "$REPO_DIR/deploy/bitch.service" /etc/systemd/system/bitch.service
systemctl daemon-reload
systemctl enable bitch

READY=1
is_unset "$(env_get BITCH_TELEGRAM_TOKEN)" && READY=0
is_unset "$(env_get BITCH_ALLOWED_USERS)" && READY=0
is_unset "$(env_get ANTHROPIC_API_KEY)" && is_unset "$(env_get CLAUDE_CODE_OAUTH_TOKEN)" && READY=0

if [ "$READY" = 1 ]; then
    log "Starting bitch"
    STAMP="$(date '+%Y-%m-%d %H:%M:%S')"
    systemctl restart bitch
    ONLINE=""
    for _ in $(seq 1 30); do
        ONLINE="$(journalctl -u bitch --since "$STAMP" --no-pager 2>/dev/null | grep -m1 'online as @' || true)"
        [ -n "$ONLINE" ] && break
        sleep 1
    done
    if [ -n "$ONLINE" ]; then
        echo "  ✓ ${ONLINE#*: }"
    else
        warn "bitch didn't report online within 30s — check: journalctl -u bitch -n 50"
    fi
else
    warn "config incomplete — edit $ENV_FILE (token, BITCH_ALLOWED_USERS, Anthropic auth), then: systemctl restart bitch"
fi

cat <<EOF

$(log "Done — remaining steps")
1. Confirm SSH works as '${ADMIN_USER}@<this box>' BEFORE closing this session
   (root login is now disabled).
2. Strongly recommended: attach your provider's cloud firewall to this VM:
     - inbound: SSH (22) from your IP(s) only; no app ports.
     - egress:  restrict outbound to what the agent actually needs. This is the
                one control root cannot undo from inside the box (§4.5, §4.6).
3. Clone the repos you want it to work on under /srv/work.
4. Message your bot. Logs: journalctl -u bitch -f   Config: $ENV_FILE
   Optional settings (voice, model, budget…) are documented in that file.
EOF
