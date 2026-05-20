#!/bin/bash
set -euo pipefail

echo "=== agentcage AMI provisioning (v${AGENTCAGE_VERSION}) ==="

ARCH="amd64"
REPO="https://github.com/okedeji/agentcage/releases/download/v${AGENTCAGE_VERSION}"

# ---------------------------------------------------------------
# System packages
# ---------------------------------------------------------------
echo "Installing system packages..."
sudo apt-get update -qq
sudo DEBIAN_FRONTEND=noninteractive apt-get upgrade -y -qq

sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq \
  curl jq unzip \
  e2fsprogs \
  nodejs npm \
  python3 python3-pip python3-venv \
  golang-go \
  bash iptables iproute2

# ---------------------------------------------------------------
# PostgreSQL 16 + TimescaleDB (both from PGDG)
# ---------------------------------------------------------------
# Ubuntu noble's archive lags PostgreSQL upstream by ~quarters
# (currently 16.13). TimescaleDB tracks the latest patch (≥ 16.14
# since 2.27.1), so noble's archive can't satisfy current TS
# packages. PGDG (apt.postgresql.org) is PostgreSQL's own apt repo,
# always carries the current patch, and is what TimescaleDB's own
# install docs recommend. Drop-in: same package names, paths,
# systemd units, and postgresql-common tooling as Ubuntu's build.
echo "Installing PostgreSQL from PGDG..."
sudo install -d /usr/share/postgresql-common/pgdg
sudo curl -fsSL -o /usr/share/postgresql-common/pgdg/apt.postgresql.org.asc \
  https://www.postgresql.org/media/keys/ACCC4CF8.asc
echo "deb [signed-by=/usr/share/postgresql-common/pgdg/apt.postgresql.org.asc] https://apt.postgresql.org/pub/repos/apt/ $(lsb_release -cs)-pgdg main" | \
  sudo tee /etc/apt/sources.list.d/pgdg.list

echo "Installing TimescaleDB..."
echo "deb https://packagecloud.io/timescale/timescaledb/ubuntu/ $(lsb_release -cs) main" | \
  sudo tee /etc/apt/sources.list.d/timescaledb.list
curl -fsSL https://packagecloud.io/timescale/timescaledb/gpgkey | \
  sudo gpg --dearmor -o /etc/apt/trusted.gpg.d/timescaledb.gpg

sudo apt-get update -qq
sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -q \
  postgresql-16 postgresql-client-16 \
  timescaledb-2-postgresql-16

# ---------------------------------------------------------------
# Disable system PostgreSQL (agentcage manages its own)
# ---------------------------------------------------------------
sudo systemctl stop postgresql || true
sudo systemctl disable postgresql || true

# ---------------------------------------------------------------
# agentcage binary
# ---------------------------------------------------------------
echo "Installing agentcage v${AGENTCAGE_VERSION}..."
sudo curl -fsSL -o /usr/local/bin/agentcage "${REPO}/agentcage-linux-${ARCH}"
sudo chmod +x /usr/local/bin/agentcage

# ---------------------------------------------------------------
# Config directory
# ---------------------------------------------------------------
sudo mkdir -p /etc/agentcage

# ---------------------------------------------------------------
# systemd service
# ---------------------------------------------------------------
echo "Installing systemd service..."
sudo tee /etc/systemd/system/agentcage.service > /dev/null << 'SVCEOF'
[Unit]
Description=agentcage orchestrator
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/agentcage init
ExecStop=/usr/local/bin/agentcage stop
TimeoutStopSec=120
Restart=on-failure
RestartSec=5
LimitNOFILE=65535

# systemd doesn't set HOME; agentcage needs it for ~/.agentcage
Environment=HOME=/root
Environment=AGENTCAGE_CONFIG=/etc/agentcage/config.yaml
Environment=AGENTCAGE_SECRETS=/etc/agentcage/secrets.env

[Install]
WantedBy=multi-user.target
SVCEOF

sudo systemctl daemon-reload
sudo systemctl enable agentcage

# ---------------------------------------------------------------
# Cleanup
# ---------------------------------------------------------------
echo "Cleaning up..."
sudo apt-get clean
sudo rm -rf /var/lib/apt/lists/* /tmp/* /var/tmp/*
sudo truncate -s 0 /var/log/*.log 2>/dev/null || true

echo "=== agentcage AMI provisioning complete ==="
