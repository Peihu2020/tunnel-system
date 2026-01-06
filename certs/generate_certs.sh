#!/bin/bash
set -e

echo "Generating SSL certificates for Tunnel Control Server..."

# Create certificates directory
mkdir -p certs
cd certs

# Generate CA private key
echo "Generating CA private key..."
openssl genrsa -out ca.key 4096

# Generate CA certificate
echo "Generating CA certificate..."
openssl req -x509 -new -nodes -key ca.key -sha256 -days 3650 \
    -out ca.crt -subj "/C=US/ST=State/L=City/O=Tunnel Org/CN=Tunnel CA"

# Generate server private key
echo "Generating server private key..."
openssl genrsa -out server.key 2048

# Generate server CSR
echo "Generating server CSR..."
openssl req -new -key server.key -out server.csr \
    -subj "/C=US/ST=State/L=City/O=Tunnel Org/CN=tunnel-control-server"

# Create server certificate config
cat > server.ext << EOF
authorityKeyIdentifier=keyid,issuer
basicConstraints=CA:FALSE
keyUsage = digitalSignature, nonRepudiation, keyEncipherment, dataEncipherment
subjectAltName = @alt_names

[alt_names]
DNS.1 = localhost
DNS.2 = tunnel-control-server
DNS.3 = tunnel-control-server.local
IP.1 = 127.0.0.1
EOF

# Sign server certificate
echo "Signing server certificate..."
openssl x509 -req -in server.csr -CA ca.crt -CAkey ca.key -CAcreateserial \
    -out server.crt -days 3650 -sha256 -extfile server.ext

# Generate client certificate for agents
echo "Generating client certificate..."
openssl genrsa -out client.key 2048

# Generate client CSR
openssl req -new -key client.key -out client.csr \
    -subj "/C=US/ST=State/L=City/O=Tunnel Org/CN=tunnel-agent"

# Create client certificate config
cat > client.ext << EOF
authorityKeyIdentifier=keyid,issuer
basicConstraints=CA:FALSE
keyUsage = digitalSignature, keyEncipherment
extendedKeyUsage = clientAuth
EOF

# Sign client certificate
openssl x509 -req -in client.csr -CA ca.crt -CAkey ca.key -CAcreateserial \
    -out client.crt -days 3650 -sha256 -extfile client.ext

# Generate agent token (for demo purposes)
echo "Generating agent tokens..."
AGENT_TOKEN=$(openssl rand -hex 32)
ADMIN_TOKEN=$(openssl rand -hex 32)

cat > tokens.env << EOF
# Agent authentication token
AGENT_TOKEN=$AGENT_TOKEN

# Admin API token
ADMIN_TOKEN=$ADMIN_TOKEN

# JWT secret
JWT_SECRET=$(openssl rand -hex 32)
EOF

# Create combined certificates for Java/Node.js compatibility
echo "Creating combined certificates..."
cat server.crt ca.crt > server-chain.crt
cat client.crt ca.crt > client-chain.crt

# Set proper permissions
chmod 600 ca.key server.key client.key
chmod 644 ca.crt server.crt client.crt

# Create PKCS12 bundle for browsers/Java
echo "Creating PKCS12 bundles..."
openssl pkcs12 -export -out server.p12 -inkey server.key -in server.crt -certfile ca.crt \
    -password pass:changeit
openssl pkcs12 -export -out client.p12 -inkey client.key -in client.crt -certfile ca.crt \
    -password pass:changeit

echo ""
echo "Certificates generated successfully!"
echo ""
echo "Generated files:"
echo "  ca.key          - CA private key (KEEP SECRET!)"
echo "  ca.crt          - CA certificate (distribute to all systems)"
echo "  server.key      - Server private key"
echo "  server.crt      - Server certificate"
echo "  client.key      - Client private key"
echo "  client.crt      - Client certificate"
echo "  server-chain.crt- Server certificate chain"
echo "  client-chain.crt- Client certificate chain"
echo "  server.p12      - Server PKCS12 bundle"
echo "  client.p12      - Client PKCS12 bundle"
echo "  tokens.env      - Authentication tokens"
echo ""
echo "To verify certificates:"
echo "  openssl verify -CAfile ca.crt server.crt"
echo "  openssl verify -CAfile ca.crt client.crt"
echo ""
echo "Agent configuration example:"
echo "  control_server:"
echo "    url: \"wss://your-server:8443\""
echo "    token: \"$AGENT_TOKEN\""
echo "    cert_file: \"certs/client-chain.crt\""
echo "    key_file: \"certs/client.key\""