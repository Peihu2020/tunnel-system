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
	mu         sync.RWMutex
	forwarding bool // 标记是否正在转发数据
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
			zap.Int("attempt", retries+1))

		if err := c.connect(); err != nil {
			c.lastError = err
			retries++

			c.logger.Error("Connection failed",
				zap.Error(err),
				zap.Int("retry", retries))

			select {
			case <-c.ctx.Done():
				return c.ctx.Err()
			case <-time.After(c.config.Agent.ReconnectDelay):
				continue
			}
		}

		c.logger.Info("Connection established successfully")

		if err := c.register(); err != nil {
			c.logger.Error("Registration failed, reconnecting", zap.Error(err))
			c.disconnect()
			retries++
			continue
		}

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

		go c.heartbeat()

		return nil
	}
}

func (c *Client) connect() error {
	wsURL := fmt.Sprintf("%s/ws?service_id=%s&token=%s",
		c.config.ControlServer.URL,
		c.config.Agent.ID,
		c.config.ControlServer.Token)

	c.logger.Info("Dialing WebSocket", zap.String("url", wsURL))

	dialer := websocket.Dialer{
		HandshakeTimeout: c.config.ControlServer.Timeout,
	}

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
	c.connected = true
	c.mu.Unlock()

	c.logger.Info("Connected to control server",
		zap.String("agent_id", c.config.Agent.ID))

	conn.SetReadLimit(64 * 1024 * 1024)
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	return nil
}

func (c *Client) disconnect() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.connected = false

	if c.wsConn != nil {
		c.wsConn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
			time.Now().Add(5*time.Second))

		time.Sleep(100 * time.Millisecond)
		c.wsConn.Close()
		c.wsConn = nil
	}

	for _, tunnel := range c.tunnels {
		tunnel.mu.Lock()
		if tunnel.LocalConn != nil {
			tunnel.LocalConn.Close()
		}
		tunnel.mu.Unlock()
	}
	c.tunnels = make(map[string]*Tunnel)

	c.logger.Info("Disconnected from control server")
}

func (c *Client) reconnect() {
	c.logger.Warn("=== RECONNECTING ===")

	c.disconnect()
	time.Sleep(1 * time.Second)

	go func() {
		c.logger.Info("Attempting reconnect...")
		if err := c.Connect(); err != nil {
			c.logger.Error("Reconnect failed", zap.Error(err))
		} else {
			c.logger.Info("Reconnect successful")
		}
	}()
}

func (c *Client) register() error {
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

func (c *Client) handleMessages() {
	defer func() {
		if r := recover(); r != nil {
			c.logger.Error("handleMessages PANIC RECOVERED",
				zap.Any("panic", r),
				zap.String("stack", string(debug.Stack())))
			c.reconnect()
		}
	}()

	c.logger.Info("=== handleMessages STARTED ===")

	for {
		if !c.isConnected() {
			c.logger.Warn("handleMessages: connection lost during loop")
			return
		}

		c.wsConn.SetReadDeadline(time.Now().Add(60 * time.Second))

		messageType, data, err := c.wsConn.ReadMessage()
		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				c.logger.Info("WebSocket closed normally by server")
				c.disconnect()
				return
			}

			if isNetError(err) {
				c.logger.Error("Network error in WebSocket connection", zap.Error(err))
				c.reconnect()
				return
			}

			if isTimeout(err) {
				c.logger.Debug("WebSocket read timeout, continuing...")
				continue
			}

			c.logger.Error("handleMessages: ReadMessage failed", zap.Error(err))
			c.reconnect()
			return
		}

		c.logger.Debug("Message received",
			zap.Int("message_type", messageType),
			zap.Int("data_length", len(data)))

		if messageType == websocket.TextMessage {
			c.handleTextMessage(data)
		}
	}
}

func (c *Client) handleTextMessage(data []byte) {
	var msg models.Message
	if err := json.Unmarshal(data, &msg); err != nil {
		c.logger.Error("Failed to unmarshal message", zap.Error(err))
		return
	}

	c.logger.Info("Processing message",
		zap.String("type", msg.Type),
		zap.String("id", msg.ID))

	switch msg.Type {
	case models.MsgTypeTunnelReq:
		c.handleTunnelRequest(msg)
	case models.MsgTypeData:
		c.handleTunnelData(msg)
	case models.MsgTypePing:
		c.handlePing()
	case "registration_ack":
		c.logger.Info("Registration acknowledged")
	case "error":
		c.logger.Error("Received error from server",
			zap.Any("message", msg.Payload))
	default:
		c.logger.Warn("Unknown message type",
			zap.String("type", msg.Type))
	}
}

// 关键修复：处理隧道数据，保持隧道打开
func (c *Client) handleTunnelData(msg models.Message) {
	var dataPacket models.DataPacket
	payload, _ := json.Marshal(msg.Payload)
	if err := json.Unmarshal(payload, &dataPacket); err != nil {
		c.logger.Error("Failed to parse data packet", zap.Error(err))
		return
	}

	c.logger.Info("Received tunnel data",
		zap.String("tunnel_id", dataPacket.TunnelID),
		zap.String("msg_id", msg.ID),
		zap.Int("data_size", len(dataPacket.Data)),
		zap.Bool("is_closing", dataPacket.IsClosing))

	// 查找隧道
	c.mu.RLock()
	tunnel, exists := c.tunnels[dataPacket.TunnelID]
	c.mu.RUnlock()

	if !exists {
		c.logger.Warn("Received data for unknown tunnel",
			zap.String("tunnel_id", dataPacket.TunnelID),
			zap.String("msg_id", msg.ID),
			zap.Any("existing_tunnels", c.getTunnelIDs()))
		return
	}

	// 检查隧道是否已关闭
	tunnel.mu.RLock()
	closed := tunnel.Closed
	tunnel.mu.RUnlock()

	if closed {
		c.logger.Warn("Tunnel is already closed",
			zap.String("tunnel_id", dataPacket.TunnelID))
		return
	}

	if dataPacket.IsClosing {
		// 关键修复：只关闭当前连接，不删除隧道
		c.logger.Info("Closing tunnel connection",
			zap.String("tunnel_id", dataPacket.TunnelID))

		tunnel.mu.Lock()
		if tunnel.LocalConn != nil {
			tunnel.LocalConn.Close()
			tunnel.LocalConn = nil
			tunnel.forwarding = false
		}
		tunnel.mu.Unlock()

		// 不删除隧道，保持隧道打开以接收后续请求
		return
	}

	// 如果本地连接不存在或已关闭，创建新连接
	tunnel.mu.RLock()
	localConn := tunnel.LocalConn
	forwarding := tunnel.forwarding
	tunnel.mu.RUnlock()

	if localConn == nil || isConnectionClosed(localConn) {
		// 创建到本地服务的新连接
		conn, err := c.connectToLocalService(dataPacket.TunnelID, tunnel.TargetPort)
		if err != nil {
			c.logger.Error("Failed to connect to local service",
				zap.String("tunnel_id", dataPacket.TunnelID),
				zap.Error(err))
			return
		}

		tunnel.mu.Lock()
		tunnel.LocalConn = conn
		tunnel.mu.Unlock()

		// 如果没有正在转发，启动转发goroutine
		if !forwarding {
			go c.forwardLocalToWebSocket(tunnel)
		}

		localConn = conn
	}

	// 写入数据到本地连接
	if len(dataPacket.Data) > 0 {
		n, err := localConn.Write(dataPacket.Data)
		if err != nil {
			c.logger.Error("Failed to write to local connection",
				zap.String("tunnel_id", dataPacket.TunnelID),
				zap.Error(err))

			// 关闭连接，但保持隧道打开
			tunnel.mu.Lock()
			if tunnel.LocalConn != nil {
				tunnel.LocalConn.Close()
				tunnel.LocalConn = nil
				tunnel.forwarding = false
			}
			tunnel.mu.Unlock()
		} else {
			c.logger.Debug("Successfully wrote to local connection",
				zap.String("tunnel_id", dataPacket.TunnelID),
				zap.Int("bytes_written", n))
		}
	}
}

func (c *Client) handleTunnelRequest(msg models.Message) {
	c.logger.Info("=== handleTunnelRequest ENTER ===")

	payload, _ := json.Marshal(msg.Payload)
	var tunnelReq struct {
		TunnelID   string `json:"tunnel_id"`
		TargetPort int    `json:"target_port"`
		LocalAddr  string `json:"local_addr"`
	}

	if err := json.Unmarshal(payload, &tunnelReq); err != nil {
		c.logger.Error("Invalid tunnel request", zap.Error(err))
		return
	}

	c.logger.Info("Received tunnel request",
		zap.String("tunnel_id", tunnelReq.TunnelID),
		zap.Int("target_port", tunnelReq.TargetPort))

	// 检查端口是否允许
	allowed := false
	for _, port := range c.config.Services {
		if port.Port == tunnelReq.TargetPort {
			allowed = true
			break
		}
	}

	if !allowed {
		c.logger.Error("Port not allowed", zap.Int("port", tunnelReq.TargetPort))
		return
	}

	// 创建隧道（如果不存在）
	c.createTunnel(tunnelReq.TunnelID, tunnelReq.TargetPort)
}

func (c *Client) createTunnel(tunnelID string, targetPort int) {
	c.logger.Info("=== createTunnel START ===",
		zap.String("tunnel_id", tunnelID),
		zap.Int("target_port", targetPort))

	// 检查隧道是否已经存在
	c.mu.RLock()
	_, exists := c.tunnels[tunnelID]
	c.mu.RUnlock()

	if exists {
		c.logger.Info("Tunnel already exists", zap.String("tunnel_id", tunnelID))
		return
	}

	// 创建隧道对象
	tunnel := &Tunnel{
		ID:         tunnelID,
		TargetPort: targetPort,
		Closed:     false,
		forwarding: false,
	}

	// 存储隧道
	c.mu.Lock()
	c.tunnels[tunnelID] = tunnel
	c.mu.Unlock()

	c.logger.Info("Tunnel created successfully",
		zap.String("tunnel_id", tunnelID),
		zap.Int("total_tunnels", len(c.tunnels)))
}

func (c *Client) connectToLocalService(tunnelID string, targetPort int) (net.Conn, error) {
	localAddr := fmt.Sprintf("127.0.0.1:%d", targetPort)

	c.logger.Info("Connecting to local service",
		zap.String("tunnel_id", tunnelID),
		zap.String("local_addr", localAddr))

	conn, err := net.DialTimeout("tcp", localAddr, 10*time.Second)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to local service: %w", err)
	}

	return conn, nil
}

// 关键修复：修改forwardLocalToWebSocket以处理连接重用
func (c *Client) forwardLocalToWebSocket(tunnel *Tunnel) {
	tunnelID := tunnel.ID

	// 标记开始转发
	tunnel.mu.Lock()
	tunnel.forwarding = true
	tunnel.mu.Unlock()

	defer func() {
		// 标记转发结束
		tunnel.mu.Lock()
		tunnel.forwarding = false
		tunnel.mu.Unlock()

		c.logger.Info("Forwarding stopped",
			zap.String("tunnel_id", tunnelID))
	}()

	tunnel.mu.RLock()
	conn := tunnel.LocalConn
	tunnel.mu.RUnlock()

	if conn == nil {
		c.logger.Warn("No local connection for forwarding",
			zap.String("tunnel_id", tunnelID))
		return
	}

	c.logger.Info("=== forwardLocalToWebSocket START ===",
		zap.String("tunnel_id", tunnelID))

	buffer := make([]byte, 32*1024)
	totalBytes := 0

	for {
		// 定期检查隧道状态
		tunnel.mu.RLock()
		forwarding := tunnel.forwarding
		localConn := tunnel.LocalConn
		tunnel.mu.RUnlock()

		if !forwarding || localConn == nil || localConn != conn {
			c.logger.Info("Forwarding stopped by tunnel state change",
				zap.String("tunnel_id", tunnelID))
			break
		}

		n, err := conn.Read(buffer)
		if err != nil {
			if err != io.EOF && !strings.Contains(err.Error(), "use of closed network connection") {
				c.logger.Error("Local connection read error",
					zap.String("tunnel_id", tunnelID),
					zap.Error(err))
			}
			break
		}

		if n == 0 {
			continue
		}

		totalBytes += n
		c.logger.Debug("Read from local service",
			zap.String("tunnel_id", tunnelID),
			zap.Int("bytes", n),
			zap.Int("total_bytes", totalBytes))

		// 创建数据包
		dataPacket := models.DataPacket{
			TunnelID: tunnelID,
			Data:     buffer[:n],
		}

		msg := models.Message{
			Type:    models.MsgTypeData,
			Payload: dataPacket,
		}

		// 发送到服务器
		if err := c.sendMessage(msg); err != nil {
			c.logger.Error("Failed to send data to server",
				zap.String("tunnel_id", tunnelID),
				zap.Error(err))
			break
		}
	}

	c.logger.Info("=== forwardLocalToWebSocket END ===",
		zap.String("tunnel_id", tunnelID),
		zap.Int("total_bytes", totalBytes))
}

func (c *Client) handlePing() {
	msg := models.Message{
		Type: models.MsgTypePong,
	}
	c.sendMessage(msg)
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

			if err := c.sendMessage(msg); err != nil {
				c.logger.Error("Failed to send heartbeat", zap.Error(err))
				c.reconnect()
				return
			}
		}
	}
}

func (c *Client) sendMessage(msg models.Message) error {
	c.mu.RLock()
	conn := c.wsConn
	c.mu.RUnlock()

	if conn == nil {
		return fmt.Errorf("not connected")
	}

	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("failed to marshal message: %w", err)
	}

	conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return conn.WriteMessage(websocket.TextMessage, data)
}

func (c *Client) isConnected() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.connected && c.wsConn != nil
}

// 修改closeTunnel，只有在真正需要时才删除隧道
func (c *Client) closeTunnel(tunnelID string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if tunnel, exists := c.tunnels[tunnelID]; exists {
		tunnel.mu.Lock()
		if tunnel.LocalConn != nil {
			tunnel.LocalConn.Close()
			tunnel.LocalConn = nil
		}
		tunnel.Closed = true
		tunnel.forwarding = false
		tunnel.mu.Unlock()

		// 关键：不立即删除隧道，让它自然清理
		// 我们将在后续的清理过程中删除它
		c.logger.Info("Tunnel marked as closed",
			zap.String("tunnel_id", tunnelID))
	}
}

// 添加清理已关闭隧道的函数
func (c *Client) cleanupClosedTunnels() {
	c.mu.Lock()
	defer c.mu.Unlock()

	for tunnelID, tunnel := range c.tunnels {
		tunnel.mu.RLock()
		closed := tunnel.Closed
		tunnel.mu.RUnlock()

		if closed {
			// 检查隧道是否已经空闲一段时间
			delete(c.tunnels, tunnelID)
			c.logger.Debug("Removed closed tunnel",
				zap.String("tunnel_id", tunnelID))
		}
	}
}

func (c *Client) Disconnect() {
	c.logger.Info("=== Disconnecting client ===")
	c.cancel()
	c.disconnect()
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
	}

	for _, netErr := range netErrors {
		if strings.Contains(strings.ToLower(errStr), strings.ToLower(netErr)) {
			return true
		}
	}

	return false
}

func isTimeout(err error) bool {
	if err == nil {
		return false
	}

	if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
		return true
	}

	return false
}

// 检查连接是否已关闭
func isConnectionClosed(conn net.Conn) bool {
	if conn == nil {
		return true
	}

	// 尝试读取一个字节来检查连接状态
	conn.SetReadDeadline(time.Now().Add(1 * time.Millisecond))
	var buf [1]byte
	_, err := conn.Read(buf[:])
	if err != nil {
		return true
	}
	return false
}

// 辅助函数：获取所有隧道ID
func (c *Client) getTunnelIDs() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	ids := make([]string, 0, len(c.tunnels))
	for id := range c.tunnels {
		ids = append(ids, id)
	}
	return ids
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
	return 0.0
}

func getMemoryUsage() float64 {
	return 0.0
}
