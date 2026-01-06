tunnel-system/
├── control-server/
│   ├── cmd/
│   │   └── server/
│   │       └── main.go
│   ├── internal/
│   │   ├── auth/
│   │   │   └── jwt.go
│   │   ├── config/
│   │   │   └── config.go
│   │   ├── models/
│   │   │   └── models.go
│   │   ├── registry/
│   │   │   └── service_registry.go
│   │   └── tunnel/
│   │       └── manager.go
│   ├── static/
│   │   └── index.html
│   ├── go.mod
│   └── go.sum
├── agent/
│   ├── cmd/
│   │   └── agent/
│   │       └── main.go
│   ├── internal/
│   │   ├── config/
│   │   │   └── config.go
│   │   ├── tunnel/
│   │   │   └── client.go
│   │   └── utils/
│   │       └── utils.go
│   ├── go.mod
│   └── go.sum
├── client/
│   └── tunnel-cli/
│       └── main.go
├── certs/
│   ├── generate_certs.sh
│   └── README.md
├── docker/
│   ├── Dockerfile.control
│   └── Dockerfile.agent
├── configs/
│   ├── control-server.yaml
│   └── agent.yaml
├── scripts/
│   ├── deploy-control.sh
│   └── deploy-agent.sh
└── README.md
