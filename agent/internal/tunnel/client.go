package tunnel

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"tunnel-agent/internal/config"
	"tunnel-agent/internal/models"

	"github.com/gorilla/websocket"
	"go.uber.org/zap"
)

type Client struct {
	config    *config.Config
	logger    *zap.Logger
	wsConn    *websocket.Conn
	mu        sync.RWMutex
	connected bool
	tunnels   map[string]*Tunnel
	ctx       context.Context
	cancel    context.CancelFunc
	lastError error
}

type Tunnel struct {
	ID         string
	TargetPort int
	LocalConn  net.Conn
	Closed     bool
}

func NewClient(cfg *config.Config, logger *zap.Logger) *Client {
	ctx, cancel := context.WithCancel(context.Background())

	return &Client{
		config:    cfg,
		logger:    logger,
		tunnels:   make(map[string]*Tunnel),
		connected: false,
		ctx:       ctx,
		cancel:    cancel,
	}
}

func (c *Client) Connect() error {
	// 先检查连接状态
	if c.isConnected() {
		c.logger.Info("Already connected, skipping reconnect")
		return nil
	}

	retries := 0
	maxRetries := 10

	for {
		if retries >= maxRetries {
			return fmt.Errorf("max reconnection attempts reached: %v", c.lastError)
		}

		c.logger.Info("Attempting to connect to control server",
			zap.Int("attempt", retries+1),
			zap.Int("max_attempts", maxRetries))

		if err := c.connect(); err != nil {
			c.lastError = err
			retries++

			c.logger.Error("Connection failed",
				zap.Error(err),
				zap.Int("retry", retries),
				zap.Duration("delay", c.config.Agent.ReconnectDelay))

			select {
			case <-c.ctx.Done():
				return c.ctx.Err()
			case <-time.After(c.config.Agent.ReconnectDelay):
				continue
			}
		}

		// 连接成功
		c.logger.Info("Connection established successfully")

		// 立即注册
		if err := c.register(); err != nil {
			c.logger.Error("Registration failed, reconnecting",
				zap.Error(err))
			c.disconnect()
			retries++
			continue
		}

		// 启动消息处理（带panic恢复）
		go func() {
			defer func() {
				if r := recover(); r != nil {
					c.logger.Error("handleMessages goroutine PANIC",
						zap.Any("panic", r),
						zap.String("stack", string(debug.Stack())))
					c.reconnect()
				}
			}()
			c.handleMessages()
		}()

		// 启动心跳
		go c.heartbeat()

		return nil
	}
}

func (c *Client) disconnect() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.connected = false

	if c.wsConn != nil {
		// 优雅关闭
		c.wsConn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
			time.Now().Add(5*time.Second))

		time.Sleep(100 * time.Millisecond)

		c.wsConn.Close()
		c.wsConn = nil
	}

	c.logger.Info("Disconnected from control server")
}

func (c *Client) reconnect() {
	c.logger.Warn("=== RECONNECTING ===")

	c.disconnect()

	// 等待一小段时间
	time.Sleep(1 * time.Second)

	// Close all tunnels
	c.mu.Lock()
	for _, tunnel := range c.tunnels {
		if tunnel.LocalConn != nil {
			tunnel.LocalConn.Close()
		}
	}
	c.tunnels = make(map[string]*Tunnel)
	c.mu.Unlock()

	// Try to reconnect
	go func() {
		c.logger.Info("Attempting reconnect...")
		if err := c.Connect(); err != nil {
			c.logger.Error("Reconnect failed", zap.Error(err))
		} else {
			c.logger.Info("Reconnect successful")
		}
	}()
}

func (c *Client) handleMessages() {
	defer func() {
		if r := recover(); r != nil {
			c.logger.Error("handleMessages PANIC RECOVERED",
				zap.Any("panic", r),
				zap.String("stack", string(debug.Stack())))
			c.reconnect()
		}
	}()

	c.logger.Info("=== handleMessages STARTED ===",
		zap.String("agent_id", c.config.Agent.ID),
		zap.Time("start_time", time.Now()))

	defer func() {
		c.logger.Info("=== handleMessages ENDED ===",
			zap.String("agent_id", c.config.Agent.ID),
			zap.Time("end_time", time.Now()))
	}()

	// 立即检查连接状态
	if !c.isConnected() {
		c.logger.Error("handleMessages: not connected at start!",
			zap.Bool("c.connected", c.connected),
			zap.Bool("wsConn_exists", c.wsConn != nil))
		return
	}

	c.logger.Info("handleMessages: connection verified, starting read loop")

	for {
		// 每次循环都检查
		if !c.isConnected() {
			c.logger.Warn("handleMessages: connection lost during loop")
			return
		}

		c.logger.Debug("handleMessages: waiting for message...")

		// 设置读取超时
		c.wsConn.SetReadDeadline(time.Now().Add(60 * time.Second))

		messageType, data, err := c.wsConn.ReadMessage()
		if err != nil {
			// 检查错误类型
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				c.logger.Info("WebSocket closed normally by server")
				c.disconnect()
				return
			}

			// 检查是否是网络错误
			if isNetError(err) {
				c.logger.Error("Network error in WebSocket connection",
					zap.Error(err),
					zap.String("error_type", fmt.Sprintf("%T", err)))
				c.reconnect()
				return
			}

			// 检查是否是超时
			if isTimeout(err) {
				c.logger.Debug("WebSocket read timeout, continuing...")
				continue
			}

			// 其他错误
			c.logger.Error("handleMessages: ReadMessage failed",
				zap.Error(err),
				zap.String("error_type", fmt.Sprintf("%T", err)))

			c.reconnect()
			return
		}

		c.logger.Info("handleMessages: received message",
			zap.Int("message_type", messageType),
			zap.Int("data_length", len(data)))

		if messageType == websocket.TextMessage {
			c.handleTextMessage(data)
		} else if messageType == websocket.BinaryMessage {
			c.handleBinaryMessage(data)
		}
	}
}

// 辅助函数
func isNetError(err error) bool {
	if err == nil {
		return false
	}

	errStr := err.Error()
	netErrors := []string{
		"use of closed network connection",
		"connection reset by peer",
		"broken pipe",
		"connection refused",
		"network is unreachable",
		"i/o timeout",
		"websocket: close",
		"repeated read on failed websocket connection",
	}

	for _, netErr := range netErrors {
		if strings.Contains(strings.ToLower(errStr), strings.ToLower(netErr)) {
			return true
		}
	}

	return false
}

func (c *Client) connect() error {
	// Prepare WebSocket URL
	wsURL := fmt.Sprintf("%s/ws?service_id=%s&token=%s",
		c.config.ControlServer.URL,
		c.config.Agent.ID,
		c.config.ControlServer.Token)

	c.logger.Info("Dialing WebSocket", zap.String("url", wsURL))

	// Configure dialer
	dialer := websocket.Dialer{
		HandshakeTimeout: c.config.ControlServer.Timeout,
	}

	// Configure TLS if needed
	if c.config.ControlServer.InsecureTLS {
		dialer.TLSClientConfig = &tls.Config{
			InsecureSkipVerify: true,
		}
	}

	headers := http.Header{}
	headers.Set("User-Agent", fmt.Sprintf("Tunnel-Agent/%s", c.config.Agent.ID))

	conn, resp, err := dialer.DialContext(c.ctx, wsURL, headers)
	if err != nil {
		return fmt.Errorf("failed to dial WebSocket: %w", err)
	}
	defer resp.Body.Close()

	c.mu.Lock()
	c.wsConn = conn
	c.connected = true // 关键修复：设置connected标志为true
	c.mu.Unlock()

	c.logger.Info("Connected to control server",
		zap.String("url", c.config.ControlServer.URL),
		zap.String("agent_id", c.config.Agent.ID),
		zap.Bool("connected_flag", true),
		zap.Int("response_status", resp.StatusCode))

	// 设置合理的WebSocket参数
	conn.SetReadLimit(64 * 1024 * 1024) // 64MB
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	return nil
}

func (c *Client) register() error {
	// Build service ports
	var ports []models.ServicePort
	for _, svcPort := range c.config.Services {
		ports = append(ports, models.ServicePort{
			Port:        svcPort.Port,
			Protocol:    svcPort.Protocol,
			Name:        svcPort.Name,
			Description: svcPort.Description,
			Internal:    svcPort.Internal,
		})
	}

	// Prepare registration request
	regReq := models.RegistrationRequest{
		ServiceID:   c.config.Agent.ID,
		ServiceName: c.config.Agent.Name,
		Hostname:    getHostname(),
		IP:          getLocalIP(),
		Ports:       ports,
		Tags: map[string]string{
			"group": c.config.Agent.Group,
			"os":    getOSInfo(),
		},
		Version:   "1.0.0",
		AuthToken: c.config.ControlServer.Token,
	}

	msg := models.Message{
		Type:    models.MsgTypeRegister,
		Payload: regReq,
	}

	return c.sendMessage(msg)
}

func (c *Client) heartbeat() {
	c.logger.Info("=== heartbeat STARTED ===")

	ticker := time.NewTicker(c.config.Agent.HeartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-c.ctx.Done():
			c.logger.Info("=== heartbeat STOPPED (context cancelled) ===")
			return
		case <-ticker.C:
			if !c.isConnected() {
				c.logger.Warn("heartbeat: not connected, stopping")
				return
			}

			hb := models.Heartbeat{
				ServiceID:   c.config.Agent.ID,
				Timestamp:   time.Now(),
				CPUUsage:    getCPUUsage(),
				MemoryUsage: getMemoryUsage(),
			}

			msg := models.Message{
				Type:    models.MsgTypeHeartbeat,
				Payload: hb,
			}

			c.logger.Debug("Sending heartbeat")
			if err := c.sendMessage(msg); err != nil {
				c.logger.Error("Failed to send heartbeat", zap.Error(err))
				c.reconnect()
				return
			}
		}
	}
}

func (c *Client) handleTextMessage(data []byte) {
	// 记录原始消息
	c.logger.Info("=== RAW WEBSOCKET MESSAGE RECEIVED ===",
		zap.Int("data_length", len(data)),
		zap.String("raw_data_preview", string(data[:min(500, len(data))])),
		zap.Time("received_at", time.Now()))

	// 尝试解析为通用map查看结构
	var rawMsg map[string]interface{}
	if err := json.Unmarshal(data, &rawMsg); err != nil {
		c.logger.Error("Failed to parse as generic map",
			zap.Error(err),
			zap.String("raw_preview", string(data[:min(200, len(data))])))
	} else {
		// 检查type字段
		if msgType, ok := rawMsg["type"].(string); ok {
			c.logger.Info("Found message type in raw map",
				zap.String("type", msgType))

			// 特别检查tunnel相关消息
			if strings.Contains(strings.ToLower(msgType), "tunnel") {
				c.logger.Warn("TUNNEL-RELATED MESSAGE DETECTED!",
					zap.String("type", msgType),
					zap.Any("payload_keys", getMapKeys(rawMsg)))
			}
		}
	}

	// 继续原有解析
	var msg models.Message
	if err := json.Unmarshal(data, &msg); err != nil {
		c.logger.Error("Failed to unmarshal as models.Message",
			zap.Error(err),
			zap.ByteString("raw_data_preview", data[:min(200, len(data))]))
		return
	}

	c.logger.Info("Parsed as models.Message",
		zap.String("type", msg.Type),
		zap.String("id", msg.ID),
		zap.Any("payload_keys", getMapKeys(msg.Payload)))

	// 处理所有消息类型
	switch msg.Type {
	case models.MsgTypeTunnelReq:
		c.logger.Info("Processing MsgTypeTunnelReq")
		c.handleTunnelRequest(msg)
	case "tunnel_req": // 另一个可能的变体
		c.logger.Info("Processing 'tunnel_req'")
		c.handleTunnelRequest(msg)
	case models.MsgTypeData:
		c.logger.Info("Processing MsgTypeData")
		c.handleTunnelData(msg)
	case models.MsgTypePing:
		c.logger.Info("Processing MsgTypePing")
		c.handlePing()
	case "registration_ack":
		c.logger.Info("Received registration_ack")
	case "error":
		c.logger.Error("Received error from server",
			zap.Any("message", msg.Payload))
	default:
		c.logger.Warn("Unknown message type received",
			zap.String("type", msg.Type),
			zap.Any("payload", msg.Payload))
	}
}

func (c *Client) handleTunnelData(msg models.Message) {
	// Parse data packet
	var dataPacket models.DataPacket
	payload, _ := json.Marshal(msg.Payload)
	if err := json.Unmarshal(payload, &dataPacket); err != nil {
		c.logger.Error("Failed to parse data packet",
			zap.Error(err),
			zap.Any("payload", msg.Payload))
		return
	}

	c.logger.Info("Received tunnel data",
		zap.String("tunnel_id", dataPacket.TunnelID),
		zap.Int("data_size", len(dataPacket.Data)),
		zap.Bool("is_closing", dataPacket.IsClosing))

	// Find the tunnel
	c.mu.RLock()
	tunnel, exists := c.tunnels[dataPacket.TunnelID]
	c.mu.RUnlock()

	if !exists {
		c.logger.Warn("Received data for unknown tunnel",
			zap.String("tunnel_id", dataPacket.TunnelID),
			zap.Any("existing_tunnels", c.getTunnelIDs()))
		return
	}

	if dataPacket.IsClosing {
		// Close tunnel
		c.mu.Lock()
		delete(c.tunnels, dataPacket.TunnelID)
		c.mu.Unlock()

		if tunnel.LocalConn != nil {
			tunnel.LocalConn.Close()
		}
		c.logger.Info("Tunnel closed via data message",
			zap.String("tunnel_id", dataPacket.TunnelID))
		return
	}

	// Write data to local connection
	if tunnel.LocalConn != nil {
		c.logger.Debug("Writing data to local connection",
			zap.String("tunnel_id", dataPacket.TunnelID),
			zap.Int("bytes_to_write", len(dataPacket.Data)))

		start := time.Now()
		n, err := tunnel.LocalConn.Write(dataPacket.Data)
		duration := time.Since(start)

		if err != nil {
			c.logger.Error("Failed to write to local connection",
				zap.String("tunnel_id", dataPacket.TunnelID),
				zap.Error(err),
				zap.Duration("write_duration", duration))

			// Close tunnel on error
			c.mu.Lock()
			delete(c.tunnels, dataPacket.TunnelID)
			c.mu.Unlock()
			tunnel.LocalConn.Close()
		} else {
			c.logger.Debug("Successfully wrote to local connection",
				zap.String("tunnel_id", dataPacket.TunnelID),
				zap.Int("bytes_written", n),
				zap.Duration("write_duration", duration))
		}
	}
}

func (c *Client) handleTunnelRequest(msg models.Message) {
	c.logger.Info("=== handleTunnelRequest ENTER ===",
		zap.Any("message", msg))

	// Parse tunnel request
	payload, _ := json.Marshal(msg.Payload)
	var tunnelReq struct {
		TunnelID   string `json:"tunnel_id"`
		TargetPort int    `json:"target_port"`
		LocalAddr  string `json:"local_addr"`
	}

	if err := json.Unmarshal(payload, &tunnelReq); err != nil {
		c.logger.Error("Invalid tunnel request",
			zap.Error(err),
			zap.Any("payload", msg.Payload))
		return
	}

	c.logger.Info("Received tunnel request",
		zap.String("tunnel_id", tunnelReq.TunnelID),
		zap.Int("target_port", tunnelReq.TargetPort),
		zap.String("local_addr", tunnelReq.LocalAddr))

	// Check if port is allowed
	allowed := false
	for _, port := range c.config.Services {
		if port.Port == tunnelReq.TargetPort {
			allowed = true
			break
		}
	}

	if !allowed {
		c.logger.Error("Port not allowed",
			zap.Int("port", tunnelReq.TargetPort),
			zap.Any("allowed_ports", c.config.Services))
		return
	}

	// Create tunnel
	go c.createTunnel(tunnelReq.TunnelID, tunnelReq.TargetPort)
}

func (c *Client) createTunnel(tunnelID string, targetPort int) {
	c.logger.Info("=== createTunnel START ===",
		zap.String("tunnel_id", tunnelID),
		zap.Int("target_port", targetPort))

	// Connect to local service
	localAddr := fmt.Sprintf("127.0.0.1:%d", targetPort)

	// 先测试端口是否可访问
	c.logger.Info("Testing connection to local service...")
	testConn, err := net.DialTimeout("tcp", localAddr, 5*time.Second)
	if err != nil {
		c.logger.Error("Local service connection test FAILED",
			zap.String("tunnel_id", tunnelID),
			zap.String("local_addr", localAddr),
			zap.Error(err))
		return
	}
	testConn.Close()
	c.logger.Info("Local service connection test PASSED")

	// 实际连接
	c.logger.Info("Establishing tunnel connection...")
	conn, err := net.DialTimeout("tcp", localAddr, 30*time.Second)
	if err != nil {
		c.logger.Error("Failed to connect to local service",
			zap.String("tunnel_id", tunnelID),
			zap.String("local_addr", localAddr),
			zap.Error(err),
			zap.String("error_type", fmt.Sprintf("%T", err)))
		return
	}

	// 存储隧道
	tunnel := &Tunnel{
		ID:         tunnelID,
		TargetPort: targetPort,
		LocalConn:  conn,
	}

	c.mu.Lock()
	c.tunnels[tunnelID] = tunnel
	c.mu.Unlock()

	defer func() {
		c.logger.Info("Closing local connection",
			zap.String("tunnel_id", tunnelID))
		conn.Close()

		c.mu.Lock()
		delete(c.tunnels, tunnelID)
		c.mu.Unlock()
	}()

	c.logger.Info("Connected to local service",
		zap.String("tunnel_id", tunnelID),
		zap.String("local_addr", localAddr),
		zap.String("connection_info", fmt.Sprintf("%v", conn)))

	// 设置长超时
	conn.SetDeadline(time.Time{})

	// Handle bidirectional data transfer
	wg := sync.WaitGroup{}
	wg.Add(2)

	// Local -> WebSocket
	go func() {
		defer wg.Done()
		c.forwardLocalToWebSocket(tunnelID, conn)
	}()

	// WebSocket -> Local (handled by handleBinaryMessage)
	go func() {
		defer wg.Done()
		c.forwardWebSocketToLocal(tunnelID, conn)
	}()

	wg.Wait()

	c.logger.Info("=== createTunnel END ===",
		zap.String("tunnel_id", tunnelID))
}

func (c *Client) forwardLocalToWebSocket(tunnelID string, conn net.Conn) {
	c.logger.Info("=== forwardLocalToWebSocket START ===",
		zap.String("tunnel_id", tunnelID),
		zap.String("local_addr", conn.LocalAddr().String()),
		zap.String("remote_addr", conn.RemoteAddr().String()))

	buffer := make([]byte, 32*1024) // 32KB buffer
	totalBytes := 0

	for {
		c.logger.Debug("Waiting to read from local connection",
			zap.String("tunnel_id", tunnelID))

		n, err := conn.Read(buffer)
		if err != nil {
			if err != io.EOF {
				c.logger.Error("Local connection read error",
					zap.String("tunnel_id", tunnelID),
					zap.Error(err))
			} else {
				c.logger.Info("Local connection EOF (end of response)",
					zap.String("tunnel_id", tunnelID),
					zap.Int("total_bytes_read", totalBytes))
			}
			break
		}

		if n == 0 {
			continue
		}

		totalBytes += n
		c.logger.Info("Read data from local service",
			zap.String("tunnel_id", tunnelID),
			zap.Int("bytes", n),
			zap.Int("total_bytes", totalBytes),
			zap.ByteString("data_preview", buffer[:min(100, n)]))

		// Create data packet
		dataPacket := models.DataPacket{
			TunnelID: tunnelID,
			Data:     buffer[:n],
		}

		msg := models.Message{
			Type:    models.MsgTypeData,
			Payload: dataPacket,
		}

		c.logger.Info("Sending data to control server",
			zap.String("tunnel_id", tunnelID),
			zap.Int("data_size", n))

		sendStart := time.Now()
		if err := c.sendMessage(msg); err != nil {
			c.logger.Error("Failed to send data to server",
				zap.String("tunnel_id", tunnelID),
				zap.Error(err),
				zap.Duration("duration", time.Since(sendStart)))
			break
		}

		c.logger.Info("Data sent successfully",
			zap.String("tunnel_id", tunnelID),
			zap.Int("bytes_sent", n),
			zap.Duration("duration", time.Since(sendStart)))
	}

	// Send close message
	dataPacket := models.DataPacket{
		TunnelID:  tunnelID,
		IsClosing: true,
	}

	msg := models.Message{
		Type:    models.MsgTypeData,
		Payload: dataPacket,
	}

	c.logger.Info("Sending tunnel close message",
		zap.String("tunnel_id", tunnelID))

	c.sendMessage(msg)

	c.logger.Info("=== forwardLocalToWebSocket END ===",
		zap.String("tunnel_id", tunnelID),
		zap.Int("total_bytes_forwarded", totalBytes))
}

func (c *Client) forwardWebSocketToLocal(tunnelID string, conn net.Conn) {
	c.logger.Info("=== forwardWebSocketToLocal START ===",
		zap.String("tunnel_id", tunnelID))

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-c.ctx.Done():
			c.logger.Info("forwardWebSocketToLocal: context cancelled",
				zap.String("tunnel_id", tunnelID))
			return
		case <-ticker.C:
			// Check if tunnel still exists
			c.mu.RLock()
			_, exists := c.tunnels[tunnelID]
			c.mu.RUnlock()

			if !exists {
				c.logger.Info("forwardWebSocketToLocal: tunnel no longer exists",
					zap.String("tunnel_id", tunnelID))
				return
			}
		}
	}
}

func (c *Client) handleBinaryMessage(data []byte) {
	c.logger.Info("=== handleBinaryMessage ===",
		zap.Int("data_length", len(data)))

	// Binary messages are tunnel data
	if len(data) < 36 {
		c.logger.Warn("Binary data too short",
			zap.Int("length", len(data)))
		return
	}

	tunnelID := string(data[:36])
	tunnelData := data[36:]

	c.logger.Info("Processing binary tunnel data",
		zap.String("tunnel_id", tunnelID),
		zap.Int("data_length", len(tunnelData)))

	c.mu.RLock()
	tunnel, exists := c.tunnels[tunnelID]
	c.mu.RUnlock()

	if !exists {
		c.logger.Warn("Received binary data for unknown tunnel",
			zap.String("tunnel_id", tunnelID))
		return
	}

	if tunnel.LocalConn == nil {
		c.logger.Error("Tunnel local connection is nil",
			zap.String("tunnel_id", tunnelID))
		return
	}

	c.logger.Debug("Writing binary data to local connection",
		zap.String("tunnel_id", tunnelID),
		zap.Int("bytes_to_write", len(tunnelData)))

	start := time.Now()
	n, err := tunnel.LocalConn.Write(tunnelData)
	duration := time.Since(start)

	if err != nil {
		c.logger.Error("Failed to write binary data to local connection",
			zap.String("tunnel_id", tunnelID),
			zap.Error(err),
			zap.Duration("write_duration", duration))

		// Close tunnel on error
		c.mu.Lock()
		delete(c.tunnels, tunnelID)
		c.mu.Unlock()
		tunnel.LocalConn.Close()
	} else {
		c.logger.Debug("Successfully wrote binary data",
			zap.String("tunnel_id", tunnelID),
			zap.Int("bytes_written", n),
			zap.Duration("write_duration", duration))
	}
}

func (c *Client) handlePing() {
	c.logger.Debug("Received ping, sending pong")

	msg := models.Message{
		Type: models.MsgTypePong,
	}

	c.sendMessage(msg)
}

func (c *Client) sendMessage(msg models.Message) error {
	c.logger.Info("=== sendMessage ENTER ===",
		zap.String("msg_type", msg.Type),
		zap.String("tunnel_id", msg.ID),
		zap.Time("timestamp", time.Now()))

	c.mu.RLock()
	conn := c.wsConn
	c.mu.RUnlock()

	if conn == nil {
		c.logger.Error("=== sendMessage FAIL: no WebSocket connection ===")
		return fmt.Errorf("not connected")
	}

	data, err := json.Marshal(msg)
	if err != nil {
		c.logger.Error("=== sendMessage FAIL: marshal error ===",
			zap.Error(err))
		return fmt.Errorf("failed to marshal message: %w", err)
	}

	c.logger.Debug("Message prepared",
		zap.Int("data_len", len(data)),
		zap.String("data_preview", string(data[:min(200, len(data))])))

	conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	start := time.Now()
	err = conn.WriteMessage(websocket.TextMessage, data)

	if err != nil {
		c.logger.Error("=== sendMessage FAIL: write error ===",
			zap.Error(err),
			zap.Duration("duration", time.Since(start)))
		return err
	}

	c.logger.Info("=== sendMessage SUCCESS ===",
		zap.String("msg_type", msg.Type),
		zap.Int("bytes_sent", len(data)),
		zap.Duration("duration", time.Since(start)))

	return nil
}

func (c *Client) isConnected() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	connected := c.connected && c.wsConn != nil

	if !connected {
		c.logger.Debug("isConnected: false",
			zap.Bool("c.connected", c.connected),
			zap.Bool("c.wsConn_nil", c.wsConn == nil))
	}

	return connected
}

func (c *Client) Disconnect() {
	c.logger.Info("=== Disconnecting client ===")

	c.cancel()

	c.mu.Lock()
	defer c.mu.Unlock()

	c.connected = false

	if c.wsConn != nil {
		c.wsConn.Close()
		c.wsConn = nil
	}

	for _, tunnel := range c.tunnels {
		if tunnel.LocalConn != nil {
			tunnel.LocalConn.Close()
		}
	}
	c.tunnels = make(map[string]*Tunnel)

	c.logger.Info("Client disconnected")
}

// Helper functions
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func getMapKeys(m interface{}) []string {
	switch v := m.(type) {
	case map[string]interface{}:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		return keys
	default:
		return []string{fmt.Sprintf("%T", m)}
	}
}

func (c *Client) getTunnelIDs() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	ids := make([]string, 0, len(c.tunnels))
	for id := range c.tunnels {
		ids = append(ids, id)
	}
	return ids
}

func isTimeout(err error) bool {
	if err == nil {
		return false
	}

	// Check for net.Error timeout
	if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
		return true
	}

	// Check for timeout in error string
	if strings.Contains(strings.ToLower(err.Error()), "timeout") ||
		strings.Contains(strings.ToLower(err.Error()), "deadline exceeded") {
		return true
	}

	return false
}

// Utility functions
func getHostname() string {
	hostname, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return hostname
}

func getLocalIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "127.0.0.1"
	}

	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if ipnet.IP.To4() != nil {
				return ipnet.IP.String()
			}
		}
	}

	return "127.0.0.1"
}

func getOSInfo() string {
	return runtime.GOOS
}

func getCPUUsage() float64 {
	// Simplified CPU usage calculation
	// In production, use proper metrics collection
	return 0.0
}

func getMemoryUsage() float64 {
	// Simplified memory usage calculation
	// In production, use proper metrics collection
	return 0.0
}
