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
# Fall back: prefer 'ubuntu' if it exists, otherwise use root
if [[ -z "$SERVICE_USER" ]] || ! getent passwd "$SERVICE_USER" &>/dev/null; then
    if getent passwd ubuntu &>/dev/null; then
        SERVICE_USER="ubuntu"
    else
        SERVICE_USER="root"
    fi
fi
SERVICE_HOME=$(getent passwd "$SERVICE_USER" | cut -d: -f6)
CONFIG_DIR="${SERVICE_HOME}/.cc-connect"
CONFIG_FILE="${CONFIG_DIR}/config.toml"

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

npm install -g cc-connect --silent
CC_BIN=$(which cc-connect)
info "cc-connect $(${CC_BIN} --version 2>&1 | head -1)  →  ${CC_BIN}"

# ── Claude Code ───────────────────────────────────────────────────────────────
section "Installing Claude Code"

sudo -u "$SERVICE_USER" npm install -g @anthropic-ai/claude-code --silent 2>/dev/null \
    || sudo -u "$SERVICE_USER" npm install -g @anthropic-ai/claude-code

CLAUDE_BIN=$(sudo -u "$SERVICE_USER" bash -lc "which claude 2>/dev/null" \
    || find "${SERVICE_HOME}/.local/bin" -name "claude" 2>/dev/null | head -1 \
    || find "${SERVICE_HOME}" -name "claude" -type f 2>/dev/null | head -1)

[[ -n "$CLAUDE_BIN" ]] || die "Could not locate claude binary after installation"
info "Claude Code installed  →  ${CLAUDE_BIN}"

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
Restart=on-failure
RestartSec=10s
Environment="PATH=${NODE_BIN_DIR}:${SERVICE_HOME}/.local/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/snap/bin"

NoNewPrivileges=true
PrivateTmp=true

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable cc-connect
info "Service installed and enabled"

# ── sudoers rule ──────────────────────────────────────────────────────────────
# Allow the service user to manage cc-connect via systemctl without a password
# (needed by the setup wizard, which runs as the service user)
cat > /etc/sudoers.d/cc-connect <<EOF
${SERVICE_USER} ALL=(ALL) NOPASSWD: /bin/systemctl start cc-connect, /bin/systemctl restart cc-connect
EOF
chmod 440 /etc/sudoers.d/cc-connect
info "Sudoers rule written for ${SERVICE_USER}"

# ── setup wizard service ──────────────────────────────────────────────────────
section "Configuring setup wizard"

WIZARD_SCRIPT="/usr/lib/node_modules/cc-connect/wizard/server.js"
[[ -f "$WIZARD_SCRIPT" ]] || WIZARD_SCRIPT="$(dirname "$(realpath "$0")")/wizard/server.js"
[[ -f "$WIZARD_SCRIPT" ]] || die "wizard/server.js not found — ensure cc-connect is fully installed"

cat > /etc/systemd/system/cc-connect-wizard.service <<EOF
[Unit]
Description=cc-connect setup wizard
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=${SERVICE_USER}
WorkingDirectory=${CONFIG_DIR}
ExecStart=${NODE_BIN_DIR}/node ${WIZARD_SCRIPT}
Environment="PATH=${NODE_BIN_DIR}:${SERVICE_HOME}/.local/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/snap/bin"

[Install]
WantedBy=multi-user.target
EOF

# Open firewall port for wizard
if command -v ufw &>/dev/null && ufw status 2>/dev/null | grep -q "Status: active"; then
    ufw allow 8080/tcp comment "cc-connect wizard" >/dev/null
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
echo "  • Claude login"
echo "  • Connecting your messaging platform"
echo -e "${BOLD}${CYAN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
