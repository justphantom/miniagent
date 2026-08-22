#!/bin/sh
# deploy.sh: install miniagent as a systemd WebUI service.
# Sources deploy/.env.example (tracked) for defaults, then overlays deploy/.env (git-ignored)
# for per-host overrides. Variables: MINIAGENT_WORKDIR / MINIAGENT_CONFIG / MINIAGENT_USER / MINIAGENT_GROUP.
# Run: sudo make deploy
set -eu

SERVICE=miniagent
SCRIPT_DIR="$(dirname "$0")"
ENV_EXAMPLE="$SCRIPT_DIR/.env.example"
ENV_FILE="$SCRIPT_DIR/.env"

# ---- source .env.example (tracked defaults) ----
if [ ! -f "$ENV_EXAMPLE" ]; then
	echo "deploy: $ENV_EXAMPLE not found (corrupted checkout)" >&2
	exit 1
fi
. "$ENV_EXAMPLE"

# ---- optional .env override (git-ignored, per-host) ----
if [ -f "$ENV_FILE" ]; then
	. "$ENV_FILE"
fi

: "${MINIAGENT_WORKDIR:?}" "${MINIAGENT_CONFIG:?}" "${MINIAGENT_USER:?}" "${MINIAGENT_GROUP:?}" "${MINIAGENT_SESSION_DIR:?}"

if ! command -v systemctl >/dev/null 2>&1; then
	echo "deploy: systemctl not found (systemd only)" >&2
	exit 1
fi

# Dedicated system user/group: agent 层 shell 无约束，至少让进程不以 root 运行。
if ! getent group "$MINIAGENT_GROUP" >/dev/null 2>&1; then
	groupadd --system "$MINIAGENT_GROUP"
fi
if ! id -u "$MINIAGENT_USER" >/dev/null 2>&1; then
	useradd --system --no-create-home --home-dir "$MINIAGENT_WORKDIR" --shell /usr/sbin/nologin -g "$MINIAGENT_GROUP" "$MINIAGENT_USER"
fi

# Ensure envsubst is available (gettext package).
if ! command -v envsubst >/dev/null 2>&1; then
	echo "deploy: envsubst not found (install gettext)" >&2
	exit 1
fi

# Render unit from template, substituting the four variables.
UNIT_TPL="$SCRIPT_DIR/miniagent.service.tpl"
UNIT_DST=/etc/systemd/system/$SERVICE.service
export MINIAGENT_WORKDIR MINIAGENT_CONFIG MINIAGENT_USER MINIAGENT_GROUP MINIAGENT_SESSION_DIR
envsubst < "$UNIT_TPL" > "$UNIT_DST"
chmod 0644 "$UNIT_DST"

# L12: verify the render consumed every placeholder — a leftover ${VAR} means the template
# referenced a variable envsubst couldn't substitute (typo in template vs the export list).
if grep -q '\${' "$UNIT_DST"; then
	echo "deploy: WARNING rendered unit still contains unsubstituted \${VAR} placeholders:" >&2
	grep -n '\${' "$UNIT_DST" >&2 || true
fi

# Install config file (seed from example if absent).
CONFIG_DIR=$(dirname "$MINIAGENT_CONFIG")
install -d -m 0755 -o root -g root "$CONFIG_DIR"
if [ ! -f "$MINIAGENT_CONFIG" ]; then
	install -m 0640 -o root -g "$MINIAGENT_GROUP" "$SCRIPT_DIR/../config.example.json" "$MINIAGENT_CONFIG"
	# L11: point the seeded config's session.dir at the deployed session dir so the
	# unit's authorized directory (MINIAGENT_SESSION_DIR) is authoritative, not the
	# example's relative ".sessions".
	sed -i 's#"dir": *".sessions"#"dir": "'"$MINIAGENT_SESSION_DIR"'"#' "$MINIAGENT_CONFIG"
	echo "deploy: seeded $MINIAGENT_CONFIG from config.example.json — edit before starting"
fi

# Install data and session dirs, and binary.
install -d -m 0750 -o "$MINIAGENT_USER" -g "$MINIAGENT_GROUP" "$MINIAGENT_WORKDIR"
install -d -m 0750 -o "$MINIAGENT_USER" -g "$MINIAGENT_GROUP" "$MINIAGENT_SESSION_DIR"
install -m 0755 "$SCRIPT_DIR/../bin/miniagent" /usr/local/bin/miniagent

systemctl daemon-reload
systemctl enable --now $SERVICE
systemctl restart $SERVICE
systemctl status $SERVICE --no-pager || true
