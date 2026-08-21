#!/bin/sh
# deploy.sh: install miniagent as a systemd WebUI service. Run: sudo make deploy
set -eu

SERVICE=miniagent
UNIT_SRC="$(dirname "$0")/miniagent.service"
UNIT_DST=/etc/systemd/system/$SERVICE.service
CONFIG_DST=/etc/miniagent/miniagent.json
DATA_DIR=/var/lib/miniagent

if ! command -v systemctl >/dev/null 2>&1; then
	echo "deploy: systemctl not found (systemd only)" >&2
	exit 1
fi

# Dedicated system user: agent 层 shell 无约束，至少让进程不以 root 运行。
if ! id -u $SERVICE >/dev/null 2>&1; then
	useradd --system --no-create-home --home-dir $DATA_DIR --shell /usr/sbin/nologin $SERVICE
fi

install -d -m 0750 -o $SERVICE -g $SERVICE $DATA_DIR
install -d -m 0755 -o root -g root /etc/miniagent
if [ ! -f "$CONFIG_DST" ]; then
	install -m 0640 -o root -g $SERVICE config.example.json "$CONFIG_DST"
	echo "deploy: seeded $CONFIG_DST from config.example.json — edit before starting"
fi

install -m 0755 bin/miniagent /usr/local/bin/miniagent
install -m 0644 "$UNIT_SRC" "$UNIT_DST"

systemctl daemon-reload
systemctl enable --now $SERVICE
systemctl status $SERVICE --no-pager || true
