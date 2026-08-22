[Unit]
Description=miniagent WebUI server (miniagent -serve)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=${MINIAGENT_USER}
Group=${MINIAGENT_GROUP}
ExecStart=/usr/local/bin/miniagent -config ${MINIAGENT_CONFIG} -serve -workdir ${MINIAGENT_WORKDIR}
WorkingDirectory=${MINIAGENT_WORKDIR}
Restart=on-failure
RestartSec=5

# Hardening: agent 层零安全（shell 工具无约束），硬化仅防服务自身越界，不约束 agent 行为。
# ProtectHome 已省略：workdir 可能位于 /home 下，需可写；且 agent shell 工具本就无路径限制。
NoNewPrivileges=true
ProtectSystem=full
PrivateTmp=true
ReadWritePaths=${MINIAGENT_WORKDIR} ${MINIAGENT_SESSION_DIR}

[Install]
WantedBy=multi-user.target