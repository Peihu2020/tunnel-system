package models

import (
	"time"
)

type ServicePort struct {
	Port        int    `json:"port"`
	Protocol    string `json:"protocol"` // http, tcp, grpc
	Name        string `json:"name"`
	Description string `json:"description"`
	Internal    bool   `json:"internal"` // true for localhost access only
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
