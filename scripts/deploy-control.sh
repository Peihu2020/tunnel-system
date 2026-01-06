#!/bin/bash
set -e

echo "Deploying Tunnel Control Server..."

# Create directory structure
sudo mkdir -p /opt/tunnel-control/{bin,certs,configs,logs,data}

# Build control server
cd control-server
go build -o ../bin/tunnel-control ./cmd/server
cd ..

# Copy binaries
sudo cp bin/tunnel-control /opt/tunnel-control/bin/

# Copy configurations
sudo cp configs/tunnel-control.yaml /opt/tunnel-control/configs/

# Generate certificates (if not exists)
if [ ! -f /opt/tunnel-control/certs/server.crt ]; then
    echo "Generating self-signed certificates..."
    sudo openssl req -x509 -newkey rsa:4096 \
        -keyout /opt/tunnel-control/certs/server.key \
        -out /opt/tunnel-control/certs/server.crt \
        -days 365 -nodes \
        -subj "/C=US/ST=State/L=City/O=Company/CN=tunnel-control-server"
    sudo chmod 600 /opt/tunnel-control/certs/server.key
fi

# Create systemd service
sudo tee /etc/systemd/system/tunnel-control.service << EOF
[Unit]
Description=Tunnel Control Server
After=network.target

[Service]
Type=simple
User=tunnel
Group=tunnel
WorkingDirectory=/opt/tunnel-control
ExecStart=/opt/tunnel-control/bin/tunnel-control
Restart=always
RestartSec=10
Environment=GODEBUG=netdns=go

# Security
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ReadWritePaths=/opt/tunnel-control/data /opt/tunnel-control/logs
ReadOnlyPaths=/opt/tunnel-control/certs /opt/tunnel-control/configs

# Logging
StandardOutput=journal
StandardError=journal
SyslogIdentifier=tunnel-control

[Install]
WantedBy=multi-user.target
EOF

# Create user
# Check if user exists
if id "tunnel" &>/dev/null; then
    echo "User 'tunnel' already exists"
else
    echo "Creating user 'tunnel'..."
    sudo useradd -r -s /bin/false tunnel
fi

# Set permissions
sudo chown -R tunnel:tunnel /opt/tunnel-control
sudo chmod 755 /opt/tunnel-control/bin/tunnel-control

# Enable and start service
sudo systemctl daemon-reload
sudo systemctl enable tunnel-control
sudo systemctl start tunnel-control

echo "Control server deployed successfully!"
sudo systemctl status tunnel-control
sudo journalctl -u tunnel-control -f