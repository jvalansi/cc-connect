#!/usr/bin/env bash
# install.sh — cc-connect 1-click installer
# Usage: curl -fsSL https://example.com/install.sh | sudo bash
set -euo pipefail

UNATTENDED=false
for arg in "$@"; do [[ "$arg" == "--unattended" ]] && UNATTENDED=true; done

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; CYAN='\033[0;36m'; BOLD='\033[1m'; NC='\033[0m'
info()    { echo -e "${GREEN}[info]${NC}  $*"; }
warn()    { echo -e "${YELLOW}[warn]${NC}  $*" >&2; }
die()     { echo -e "${RED}[error]${NC} $*" >&2; exit 1; }
section() { echo -e "\n${BOLD}${CYAN}━━  $*${NC}"; }

# ── root check ────────────────────────────────────────────────────────────────
[[ $EUID -eq 0 ]] || die "Run with sudo: curl -fsSL <url> | sudo bash"

SERVICE_USER="${SUDO_USER:-}"
# Resolve or create a non-root service user (Claude Code blocks bypassPermissions as root)
if [[ -z "$SERVICE_USER" ]] || ! getent passwd "$SERVICE_USER" &>/dev/null; then
    if getent passwd ubuntu &>/dev/null; then
        SERVICE_USER="ubuntu"
    else
        SERVICE_USER="ubuntu"
        useradd -m -s /bin/bash ubuntu
        info "Created user 'ubuntu'"
    fi
fi
SERVICE_HOME=$(getent passwd "$SERVICE_USER" | cut -d: -f6)
CONFIG_DIR="${SERVICE_HOME}/.cc-connect"
CONFIG_FILE="${CONFIG_DIR}/config.toml"

# ── Wait for cloud-init ───────────────────────────────────────────────────────
section "Waiting for system to be ready"

if command -v cloud-init &>/dev/null; then
    info "Waiting for cloud-init to finish..."
    cloud-init status --wait >/dev/null 2>&1 || true
fi

# ── Swap ──────────────────────────────────────────────────────────────────────
# Small droplets (≤2 GiB) with no swap OOM-kill cc-connect under load.
# Provision 2 GiB of swap if the host has none.
section "Ensuring swap is configured"

if [[ "$(swapon --show --noheadings | wc -l)" -eq 0 ]]; then
    SWAPFILE=/swapfile
    if [[ ! -f "$SWAPFILE" ]]; then
        info "Allocating 2 GiB swapfile at ${SWAPFILE}…"
        fallocate -l 2G "$SWAPFILE" || dd if=/dev/zero of="$SWAPFILE" bs=1M count=2048 status=none
        chmod 600 "$SWAPFILE"
        mkswap "$SWAPFILE" >/dev/null
    fi
    swapon "$SWAPFILE" || warn "swapon failed; continuing without swap"
    if ! grep -q "^${SWAPFILE} " /etc/fstab; then
        echo "${SWAPFILE} none swap sw 0 0" >> /etc/fstab
    fi
    info "Swap enabled: $(swapon --show --noheadings | head -1)"
else
    info "Swap already configured: $(swapon --show --noheadings | head -1)"
fi

# ── Node.js ───────────────────────────────────────────────────────────────────
section "Checking Node.js"

NODE_OK=false
if command -v node &>/dev/null; then
    NODE_VER=$(node -e "process.exit(+process.version.slice(1).split('.')[0] < 18)" 2>/dev/null && echo ok || echo old)
    [[ "$NODE_VER" == "ok" ]] && NODE_OK=true
fi

if [[ "$NODE_OK" == "false" ]]; then
    info "Installing Node.js 20..."
    apt-get update -qq
    apt-get install -y -qq curl ca-certificates
    curl -fsSL https://deb.nodesource.com/setup_20.x | bash - >/dev/null 2>&1
    apt-get install -y -qq nodejs
fi

info "Node $(node --version)  npm $(npm --version)"

# ── cc-connect ────────────────────────────────────────────────────────────────
section "Installing cc-connect"

# Installed from this repository's own releases rather than the upstream npm
# package. The npm package ships upstream builds, so an install (or a later
# `cc-connect update`) would replace this fork's binary with one that lacks
# whatever the fork adds. One artifact source for both install and upgrade.
CC_REPO="${CC_REPO:-jvalansi/cc-connect}"
CC_INSTALL_PATH="${CC_INSTALL_PATH:-/usr/bin/cc-connect}"

case "$(uname -m)" in
    x86_64|amd64)  CC_ARCH=amd64 ;;
    aarch64|arm64) CC_ARCH=arm64 ;;
    *) die "Unsupported architecture: $(uname -m)" ;;
esac

if [[ -z "${CC_VERSION:-}" ]]; then
    CC_VERSION=$(curl -fsSL "https://api.github.com/repos/${CC_REPO}/releases/latest" \
        | grep -m1 '"tag_name"' | cut -d'"' -f4 || true)
fi
[[ -n "$CC_VERSION" ]] || die "Could not determine the latest ${CC_REPO} release. Set CC_VERSION=vX.Y.Z and re-run."

CC_TARBALL="cc-connect-${CC_VERSION}-linux-${CC_ARCH}.tar.gz"
CC_URL="https://github.com/${CC_REPO}/releases/download/${CC_VERSION}/${CC_TARBALL}"
CC_TMP=$(mktemp -d)

info "Downloading ${CC_VERSION} (linux/${CC_ARCH})…"
curl -fsSL "$CC_URL" -o "${CC_TMP}/${CC_TARBALL}" || die "Download failed: ${CC_URL}"
tar xzf "${CC_TMP}/${CC_TARBALL}" -C "$CC_TMP" || die "Could not unpack ${CC_TARBALL}"

CC_EXTRACTED=$(find "$CC_TMP" -type f -name 'cc-connect*' ! -name '*.tar.gz' | head -1)
[[ -n "$CC_EXTRACTED" ]] || die "No cc-connect binary inside ${CC_TARBALL}"

# Replace rather than overwrite: the running binary's inode stays busy.
[[ -f "$CC_INSTALL_PATH" ]] && mv "$CC_INSTALL_PATH" "${CC_INSTALL_PATH}.bak.$(date +%Y%m%d%H%M%S)"
install -m 755 "$CC_EXTRACTED" "$CC_INSTALL_PATH"
rm -rf "$CC_TMP"

CC_BIN="$CC_INSTALL_PATH"
info "cc-connect $(${CC_BIN} --version 2>&1 | head -1)  →  ${CC_BIN}"

# ── Claude Code ───────────────────────────────────────────────────────────────
section "Installing Claude Code"

# Ensure the service user has a user-local npm prefix so global installs don't
# require root-owned /usr/lib/node_modules / /usr/bin write access.
sudo -u "$SERVICE_USER" npm config set prefix "${SERVICE_HOME}/.local"

sudo -u "$SERVICE_USER" npm install -g @anthropic-ai/claude-code --silent 2>/dev/null \
    || sudo -u "$SERVICE_USER" npm install -g @anthropic-ai/claude-code

CLAUDE_BIN=$(sudo -u "$SERVICE_USER" bash -lc "which claude 2>/dev/null" \
    || find "${SERVICE_HOME}/.local/bin" -name "claude" 2>/dev/null | head -1 \
    || find "${SERVICE_HOME}" -name "claude" -type f 2>/dev/null | head -1)

[[ -n "$CLAUDE_BIN" ]] || die "Could not locate claude binary after installation"
info "Claude Code installed  →  ${CLAUDE_BIN}"

# ── Optional AI agent CLIs ────────────────────────────────────────────────────
section "Installing optional AI agent CLIs"

sudo -u "$SERVICE_USER" npm install -g @google/gemini-cli --silent 2>/dev/null \
    && info "Gemini CLI installed" || warn "Gemini CLI install skipped (optional)"

sudo -u "$SERVICE_USER" npm install -g @openai/codex --silent 2>/dev/null \
    && info "OpenAI Codex CLI installed" || warn "OpenAI Codex CLI install skipped (optional)"

# ── systemd service ───────────────────────────────────────────────────────────
section "Configuring systemd service"

NODE_BIN_DIR=$(dirname "$(which node)")

cat > /etc/systemd/system/cc-connect.service <<EOF
[Unit]
Description=cc-connect - AI Agent Chat Bridge
After=network-online.target
Wants=network-online.target
StartLimitIntervalSec=300
StartLimitBurst=10

[Service]
Type=simple
User=${SERVICE_USER}
WorkingDirectory=${CONFIG_DIR}
ExecStart=${CC_BIN}
Restart=always
RestartSec=10s
Environment="PATH=${NODE_BIN_DIR}:${SERVICE_HOME}/.local/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/snap/bin"
EnvironmentFile=-${CONFIG_DIR}/agent.env

NoNewPrivileges=true
PrivateTmp=true

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable cc-connect
info "Service installed and enabled"

# ── sudoers rule ──────────────────────────────────────────────────────────────
# Single-tenant box owned by the customer — the service user (who runs Claude
# Code) gets full passwordless sudo so the agent can install packages, manage
# services, configure networking, etc. on the customer's behalf.
cat > /etc/sudoers.d/cc-connect <<EOF
${SERVICE_USER} ALL=(ALL) NOPASSWD: ALL
EOF
chmod 440 /etc/sudoers.d/cc-connect
info "Sudoers rule written for ${SERVICE_USER}"

# ── setup wizard service ──────────────────────────────────────────────────────
section "Configuring setup wizard"

WIZARD_SCRIPT="/usr/lib/node_modules/cc-connect/wizard/server.js"
if [[ ! -f "$WIZARD_SCRIPT" ]]; then
    WIZARD_SCRIPT="$(dirname "$(realpath "$0")")/wizard/server.js"
fi
if [[ ! -f "$WIZARD_SCRIPT" ]]; then
    info "Downloading wizard from GitHub…"
    mkdir -p "$(dirname "$WIZARD_SCRIPT")"
    WIZARD_URL="https://raw.githubusercontent.com/jvalansi/cc-connect/main/wizard/server.js"
    curl -fsSL "$WIZARD_URL" -o "$WIZARD_SCRIPT" || die "Failed to download wizard/server.js"
fi

# Ensure config dir exists so the wizard's WorkingDirectory is valid
mkdir -p "${CONFIG_DIR}"
chown "${SERVICE_USER}:${SERVICE_USER}" "${CONFIG_DIR}" 2>/dev/null || true

cat > /etc/systemd/system/cc-connect-wizard.service <<EOF
[Unit]
Description=cc-connect setup wizard
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=root
WorkingDirectory=${CONFIG_DIR}
ExecStart=${NODE_BIN_DIR}/node ${WIZARD_SCRIPT}
Restart=always
RestartSec=10s
Environment="PATH=${NODE_BIN_DIR}:${SERVICE_HOME}/.local/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/snap/bin"

[Install]
WantedBy=multi-user.target
EOF

# The wizard authenticates nothing and runs as root, so port 8080 is
# deliberately NOT opened here. Reach it over an SSH tunnel, or restrict it to
# a management address (see scripts/wizard_firewall.sh in the hosting repo).
if command -v ufw &>/dev/null && ufw status 2>/dev/null | grep -q "Status: active"; then
    ufw delete allow 8080/tcp >/dev/null 2>&1 || true
fi

systemctl daemon-reload
systemctl enable cc-connect-wizard
systemctl restart cc-connect-wizard
info "Wizard service started"

# ── done ─────────────────────────────────────────────────────────────────────
SERVER_IP=$(curl -s --max-time 3 ifconfig.me 2>/dev/null || hostname -I | awk '{print $1}')

echo ""
echo -e "${BOLD}${CYAN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
echo -e "${BOLD}  Installation complete!${NC}"
echo ""
echo "  Open the setup wizard in your browser:"
echo -e "  ${BOLD}http://${SERVER_IP}:8080${NC}"
echo ""
echo "  The wizard will guide you through:"
echo "  • Choosing your AI (Claude, Gemini, or OpenAI Codex)"
echo "  • Logging in or entering your API key"
echo "  • Connecting your messaging platform"
echo -e "${BOLD}${CYAN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
