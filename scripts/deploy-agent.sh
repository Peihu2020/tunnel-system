#!/bin/bash
set -e

echo "Deploying Tunnel Agent..."

# Create directory structure
sudo mkdir -p /opt/tunnel-agent/{bin,configs,logs}

# Build agent
cd agent
go build -o ../bin/tunnel-agent ./cmd/agent
cd ..

# Copy binaries
sudo cp bin/tunnel-agent /opt/tunnel-agent/bin/

# Generate configuration
AGENT_ID="${HOSTNAME}-$(cat /etc/machine-id | cut -c1-8)"
TOKEN=$(openssl rand -hex 32)

sudo tee /opt/tunnel-agent/configs/agent.yaml << EOF
agent:
  id: "${AGENT_ID}"
  name: "${HOSTNAME}"
  group: "production"
  tags:
    - "vm"
    - "production"
  heartbeat_interval: "30s"
  reconnect_delay: "5s"
  max_retries: 10

control_server:
  url: "wss://10.6.31.19:8443"
  token: "${TOKEN}"
  insecure_tls: false
  timeout: "30s"

services:
  - port: 80
    protocol: "http"
    name: "nginx-http"
    internal: true

logging:
  level: "info"
  format: "json"
  output: "/opt/tunnel-agent/logs/agent.log"
EOF

# Create systemd service
sudo tee /etc/systemd/system/tunnel-agent.service << EOF
[Unit]
Description=Tunnel Agent
After=network.target

[Service]
Type=simple
User=tunnel-agent
Group=tunnel-agent
WorkingDirectory=/opt/tunnel-agent
ExecStart=/opt/tunnel-agent/bin/tunnel-agent /opt/tunnel-agent/configs/agent.yaml
Restart=always
RestartSec=10
Environment=GODEBUG=netdns=go

# Security
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ReadWritePaths=/opt/tunnel-agent/logs
ReadOnlyPaths=/opt/tunnel-agent/configs

# Logging
StandardOutput=journal
StandardError=journal
SyslogIdentifier=tunnel-agent

[Install]
WantedBy=multi-user.target
EOF

# Create user
sudo useradd -r -s /bin/false tunnel-agent

# Set permissions
sudo chown -R tunnel-agent:tunnel-agent /opt/tunnel-agent
sudo chmod 755 /opt/tunnel-agent/bin/tunnel-agent

# Enable and start service
sudo systemctl daemon-reload
sudo systemctl enable tunnel-agent
sudo systemctl start tunnel-agent

echo "Agent deployed successfully!"
echo "Agent ID: ${AGENT_ID}"
echo "Token: ${TOKEN}"
echo ""
echo "Check status: sudo systemctl status tunnel-agent"
echo "View logs: sudo journalctl -u tunnel-agent -f"
echo "Edit config: sudo vi /opt/tunnel-agent/configs/agent.yaml"