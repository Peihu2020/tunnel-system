package models

import (
	"time"
)

type Service struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Hostname    string            `json:"hostname"`
	IP          string            `json:"ip"`
	Ports       []ServicePort     `json:"ports"`
	Tags        map[string]string `json:"tags"`
	Status      string            `json:"status"` // online, offline, degraded
	LastSeen    time.Time         `json:"last_seen"`
	ConnectedAt time.Time         `json:"connected_at"`
	Conn        interface{}       `json:"-"` // WebSocket connection
	Version     string            `json:"version"`
	Metadata    map[string]string `json:"metadata"`
}

type ServicePort struct {
	Port        int    `json:"port"`
	Protocol    string `json:"protocol"` // http, tcp, grpc
	Name        string `json:"name"`
	Description string `json:"description"`
	Internal    bool   `json:"internal"` // true for localhost access only
}

type TunnelRequest struct {
	ID            string    `json:"id"`
	ServiceID     string    `json:"service_id"`
	SourceIP      string    `json:"source_ip"`
	TargetPort    int       `json:"target_port"`
	Protocol      string    `json:"protocol"`
	RequestedAt   time.Time `json:"requested_at"`
	EstablishedAt time.Time `json:"established_at,omitempty"`
	ClosedAt      time.Time `json:"closed_at,omitempty"`
	Duration      float64   `json:"duration,omitempty"`
}

type Message struct {
	Type    string      `json:"type"`
	Payload interface{} `json:"payload"`
	Error   string      `json:"error,omitempty"`
	ID      string      `json:"id,omitempty"`
}

// Message types
const (
	MsgTypeRegister   = "register"
	MsgTypeHeartbeat  = "heartbeat"
	MsgTypeData       = "data"
	MsgTypeTunnelReq  = "tunnel_request"
	MsgTypeTunnelResp = "tunnel_response"
	MsgTypeError      = "error"
	MsgTypePing       = "ping"
	MsgTypePong       = "pong"
)

type RegistrationRequest struct {
	ServiceID   string            `json:"service_id"`
	ServiceName string            `json:"service_name"`
	Hostname    string            `json:"hostname"`
	IP          string            `json:"ip"`
	Ports       []ServicePort     `json:"ports"`
	Tags        map[string]string `json:"tags"`
	Version     string            `json:"version"`
	AuthToken   string            `json:"auth_token"`
}

type Heartbeat struct {
	ServiceID   string    `json:"service_id"`
	Timestamp   time.Time `json:"timestamp"`
	CPUUsage    float64   `json:"cpu_usage,omitempty"`
	MemoryUsage float64   `json:"memory_usage,omitempty"`
}

type DataPacket struct {
	TunnelID  string `json:"tunnel_id"`
	Sequence  int64  `json:"sequence"`
	Data      []byte `json:"data"`
	IsClosing bool   `json:"is_closing"`
}
