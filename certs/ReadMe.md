# SSL Certificates for Tunnel System

This directory contains SSL/TLS certificates for secure communication between
the Tunnel Control Server and Agents.

## File Structure
certs/
├── generate_certs.sh # Certificate generation script
├── ca.key # CA private key (SECRET!)
├── ca.crt # CA public certificate
├── server.key # Server private key
├── server.crt # Server certificate
├── server-chain.crt # Server certificate chain
├── client.key # Client private key
├── client.crt # Client certificate
├── client-chain.crt # Client certificate chain
├── server.p12 # PKCS12 bundle for server
├── client.p12 # PKCS12 bundle for client
└── tokens.env # Authentication tokens


## Generation

To generate new certificates:

```bash
cd certs
chmod +x generate_certs.sh
./generate_certs.sh