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
	Closed     bool
	mu         sync.RWMutex
	// 不再维护持久连接，为每个请求创建新连接
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
		// 延长Pong响应时间
		conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		return nil
	})

	return nil
}

func (c *Client) disconnect() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.connected = false

	if c.wsConn != nil {
		// 优雅关闭WebSocket
		err := c.wsConn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, "client shutdown"),
			time.Now().Add(5*time.Second))
		if err != nil {
			c.logger.Debug("Error sending close message", zap.Error(err))
		}

		time.Sleep(100 * time.Millisecond)
		c.wsConn.Close()
		c.wsConn = nil
	}

	// 清理所有隧道
	for tunnelID := range c.tunnels {
		delete(c.tunnels, tunnelID)
	}

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
			// 指数退避重试
			time.Sleep(5 * time.Second)
			c.reconnect()
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

		// 设置较长的读取超时
		c.wsConn.SetReadDeadline(time.Now().Add(90 * time.Second))

		messageType, data, err := c.wsConn.ReadMessage()
		if err != nil {
			// 检查是否为读取超时
			if err.Error() == "i/o timeout" {
				c.logger.Debug("WebSocket read timeout, sending ping...")
				// 发送Ping检查连接
				if err := c.wsConn.WriteControl(websocket.PingMessage,
					[]byte{}, time.Now().Add(5*time.Second)); err != nil {
					c.logger.Error("Ping failed, reconnecting", zap.Error(err))
					c.reconnect()
					return
				}
				continue
			}

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

			c.logger.Error("handleMessages: ReadMessage failed", zap.Error(err))
			c.reconnect()
			return
		}

		c.logger.Debug("Message received",
			zap.Int("message_type", messageType),
			zap.Int("data_length", len(data)))

		if messageType == websocket.TextMessage {
			go c.handleTextMessage(data)
		} else if messageType == websocket.PongMessage {
			// 重置读取超时
			c.wsConn.SetReadDeadline(time.Now().Add(90 * time.Second))
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

// 关键修复：处理隧道数据，为每个请求创建独立的连接
func (c *Client) handleTunnelData(msg models.Message) {
	var dataPacket models.DataPacket
	payload, _ := json.Marshal(msg.Payload)
	if err := json.Unmarshal(payload, &dataPacket); err != nil {
		c.logger.Error("Failed to parse data packet", zap.Error(err))
		return
	}

	c.logger.Info("Processing tunnel data",
		zap.String("tunnel_id", dataPacket.TunnelID),
		zap.String("msg_id", msg.ID),
		zap.Int("data_size", len(dataPacket.Data)),
		zap.Bool("is_closing", dataPacket.IsClosing))

	// 对于浏览器兼容性：忽略关闭消息
	if dataPacket.IsClosing {
		c.logger.Debug("Ignoring close message for browser compatibility",
			zap.String("tunnel_id", dataPacket.TunnelID),
			zap.String("msg_id", msg.ID))
		return
	}

	// 查找隧道
	c.mu.RLock()
	tunnel, exists := c.tunnels[dataPacket.TunnelID]
	c.mu.RUnlock()

	if !exists {
		c.logger.Warn("Received data for unknown tunnel",
			zap.String("tunnel_id", dataPacket.TunnelID),
			zap.String("msg_id", msg.ID))
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

	// 为每个请求创建独立的goroutine处理
	go c.processRequest(tunnel, dataPacket, msg.ID)
}

// 处理单个HTTP请求
func (c *Client) processRequest(tunnel *Tunnel, dataPacket models.DataPacket, msgID string) {
	// 生成唯一的请求ID用于日志跟踪
	requestID := fmt.Sprintf("%s-%d", msgID, time.Now().UnixNano())

	c.logger.Info("Processing request",
		zap.String("tunnel_id", tunnel.ID),
		zap.String("request_id", requestID),
		zap.Int("request_size", len(dataPacket.Data)))

	// 连接到本地服务
	localAddr := fmt.Sprintf("127.0.0.1:%d", tunnel.TargetPort)
	conn, err := net.DialTimeout("tcp", localAddr, 10*time.Second)
	if err != nil {
		c.logger.Error("Failed to connect to local service",
			zap.String("tunnel_id", tunnel.ID),
			zap.String("request_id", requestID),
			zap.Error(err))
		return
	}

	defer func() {
		conn.Close()
		c.logger.Debug("Local connection closed",
			zap.String("tunnel_id", tunnel.ID),
			zap.String("request_id", requestID))
	}()

	c.logger.Debug("Connected to local service",
		zap.String("tunnel_id", tunnel.ID),
		zap.String("request_id", requestID),
		zap.String("local_addr", localAddr))

	// 设置连接超时
	conn.SetDeadline(time.Now().Add(30 * time.Second))

	// 发送请求数据
	if len(dataPacket.Data) > 0 {
		startTime := time.Now()
		n, err := conn.Write(dataPacket.Data)
		writeDuration := time.Since(startTime)

		if err != nil {
			c.logger.Error("Failed to write request to local service",
				zap.String("tunnel_id", tunnel.ID),
				zap.String("request_id", requestID),
				zap.Error(err))
			return
		}

		c.logger.Debug("Request sent to local service",
			zap.String("tunnel_id", tunnel.ID),
			zap.String("request_id", requestID),
			zap.Int("bytes", n),
			zap.Duration("duration", writeDuration))
	}

	// 读取响应
	response, err := c.readResponse(conn, tunnel.ID, requestID)
	if err != nil {
		c.logger.Error("Failed to read response from local service",
			zap.String("tunnel_id", tunnel.ID),
			zap.String("request_id", requestID),
			zap.Error(err))
		return
	}

	// 发送响应到控制服务器
	if len(response) > 0 {
		responsePacket := models.DataPacket{
			TunnelID: tunnel.ID,
			Data:     response,
		}

		responseMsg := models.Message{
			Type:    models.MsgTypeData,
			ID:      msgID,
			Payload: responsePacket,
		}

		startTime := time.Now()
		if err := c.sendMessage(responseMsg); err != nil {
			c.logger.Error("Failed to send response to server",
				zap.String("tunnel_id", tunnel.ID),
				zap.String("request_id", requestID),
				zap.Error(err))
			return
		}

		sendDuration := time.Since(startTime)
		c.logger.Info("Response sent successfully",
			zap.String("tunnel_id", tunnel.ID),
			zap.String("request_id", requestID),
			zap.Int("response_bytes", len(response)),
			zap.Duration("duration", sendDuration))
	}
}

// 读取HTTP响应
func (c *Client) readResponse(conn net.Conn, tunnelID, requestID string) ([]byte, error) {
	buffer := make([]byte, 64*1024) // 64KB buffer
	var response []byte
	totalRead := 0
	readTimeout := 30 * time.Second

	for {
		// 设置读取超时
		conn.SetReadDeadline(time.Now().Add(readTimeout))

		n, err := conn.Read(buffer)
		if err != nil {
			if err == io.EOF {
				// 响应结束
				if totalRead > 0 {
					response = append(response, buffer[:n]...)
					totalRead += n
				}
				break
			}

			if isTimeout(err) {
				c.logger.Warn("Read timeout, ending response",
					zap.String("tunnel_id", tunnelID),
					zap.String("request_id", requestID))
				if n > 0 {
					response = append(response, buffer[:n]...)
					totalRead += n
				}
				break
			}

			// 其他错误
			return nil, fmt.Errorf("read error: %w", err)
		}

		if n > 0 {
			response = append(response, buffer[:n]...)
			totalRead += n

			// 检查是否已读取完整响应
			if c.isCompleteResponse(response) {
				c.logger.Debug("Complete HTTP response detected",
					zap.String("tunnel_id", tunnelID),
					zap.String("request_id", requestID),
					zap.Int("bytes", totalRead))
				break
			}

			// 检查buffer是否快满了
			if totalRead > len(buffer)-4096 {
				c.logger.Warn("Response buffer nearly full, stopping read",
					zap.String("tunnel_id", tunnelID),
					zap.String("request_id", requestID),
					zap.Int("total_bytes", totalRead))
				break
			}
		}
	}

	c.logger.Debug("Response reading completed",
		zap.String("tunnel_id", tunnelID),
		zap.String("request_id", requestID),
		zap.Int("total_bytes", totalRead))

	return response, nil
}

// 检查是否为完整的HTTP响应
func (c *Client) isCompleteResponse(data []byte) bool {
	if len(data) < 14 { // 最小响应: "HTTP/1.1 200\r\n\r\n"
		return false
	}

	// 查找头部结束标记
	dataStr := string(data)

	// 检查是否有 \r\n\r\n 或 \n\n
	headerEnd1 := strings.Index(dataStr, "\r\n\r\n")
	headerEnd2 := strings.Index(dataStr, "\n\n")

	if headerEnd1 == -1 && headerEnd2 == -1 {
		return false
	}

	// 检查是否有Content-Length头部
	if strings.Contains(dataStr, "Content-Length:") {
		// 需要解析Content-Length并检查body是否完整
		lines := strings.Split(dataStr, "\r\n")
		if len(lines) == 1 {
			lines = strings.Split(dataStr, "\n")
		}

		var contentLength int
		for _, line := range lines {
			if strings.HasPrefix(strings.ToLower(line), "content-length:") {
				parts := strings.SplitN(line, ":", 2)
				if len(parts) == 2 {
					fmt.Sscanf(strings.TrimSpace(parts[1]), "%d", &contentLength)
					break
				}
			}
		}

		if contentLength > 0 {
			// 找到头部结束位置
			headerEnd := headerEnd1
			if headerEnd == -1 {
				headerEnd = headerEnd2
			} else if headerEnd2 != -1 && headerEnd2 < headerEnd {
				headerEnd = headerEnd2
			}

			if headerEnd == -1 {
				return false
			}

			// 计算body开始位置
			bodyStart := headerEnd
			if strings.HasPrefix(dataStr[headerEnd:], "\r\n\r\n") {
				bodyStart += 4
			} else if strings.HasPrefix(dataStr[headerEnd:], "\n\n") {
				bodyStart += 2
			}

			// 检查body长度
			bodyLength := len(data) - bodyStart
			return bodyLength >= contentLength
		}
	}

	// 对于chunked编码或其他情况，认为头部结束就是响应结束
	return true
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
	}

	// 存储隧道
	c.mu.Lock()
	c.tunnels[tunnelID] = tunnel
	c.mu.Unlock()

	c.logger.Info("Tunnel created successfully",
		zap.String("tunnel_id", tunnelID),
		zap.Int("total_tunnels", len(c.tunnels)))
}

func (c *Client) handlePing() {
	msg := models.Message{
		Type: models.MsgTypePong,
	}
	c.sendMessage(msg)
}

func (c *Client) heartbeat() {
	c.logger.Info("=== heartbeat STARTED ===")

	pingTicker := time.NewTicker(25 * time.Second)
	heartbeatTicker := time.NewTicker(30 * time.Second)
	defer pingTicker.Stop()
	defer heartbeatTicker.Stop()

	for {
		select {
		case <-c.ctx.Done():
			c.logger.Info("=== heartbeat STOPPED (context cancelled) ===")
			return
		case <-pingTicker.C:
			if !c.isConnected() {
				c.logger.Warn("heartbeat: not connected, stopping")
				return
			}

			// 发送WebSocket Ping
			c.wsConn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if err := c.wsConn.WriteMessage(websocket.PingMessage, []byte{}); err != nil {
				c.logger.Error("Failed to send ping", zap.Error(err))
				c.reconnect()
				return
			}

		case <-heartbeatTicker.C:
			if !c.isConnected() {
				c.logger.Warn("heartbeat: not connected, stopping")
				return
			}

			// 发送应用层心跳
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
				// 不立即重连，让Ping/Pong机制处理
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
		"EOF",
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
	// 实现CPU使用率获取
	return 0.0
}

func getMemoryUsage() float64 {
	// 实现内存使用率获取
	return 0.0
}
