package tunnel

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"tunnel-control/internal/models"
	"tunnel-control/internal/registry"

	"github.com/gorilla/websocket"
	"go.uber.org/zap"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  64 * 1024,
	WriteBufferSize: 64 * 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

type TunnelManager struct {
	registry      registry.ServiceRegistry
	connections   map[string]*websocket.Conn // serviceID -> connection
	tunnels       map[string]*Tunnel         // tunnelID -> tunnel
	listeners     map[string]net.Listener    // tunnelID -> listener
	mu            sync.RWMutex
	logger        *zap.Logger
	config        *Config
	activeProxies map[string]*http.Server
}

type Tunnel struct {
	ID         string
	ServiceID  string
	LocalAddr  string
	RemoteAddr string
	CreatedAt  time.Time
	conn       net.Conn
	wsConn     *websocket.Conn
	closed     bool
	mu         sync.RWMutex
}

type Config struct {
	HeartbeatTimeout time.Duration
	BufferSize       int
	MaxMessageSize   int64
	PingInterval     time.Duration
	PongWait         time.Duration
}

func NewTunnelManager(reg registry.ServiceRegistry, logger *zap.Logger, config *Config) *TunnelManager {
	if config.BufferSize < 4096 {
		logger.Warn("Buffer size too small, increasing to 32KB", zap.Int("original", config.BufferSize))
		config.BufferSize = 32 * 1024
	}

	if config.MaxMessageSize < 1024*1024 {
		logger.Warn("Max message size too small, increasing to 10MB", zap.Int64("original", config.MaxMessageSize))
		config.MaxMessageSize = 10 * 1024 * 1024
	}

	return &TunnelManager{
		registry:      reg,
		connections:   make(map[string]*websocket.Conn),
		tunnels:       make(map[string]*Tunnel),
		listeners:     make(map[string]net.Listener),
		logger:        logger,
		config:        config,
		activeProxies: make(map[string]*http.Server),
	}
}

func (tm *TunnelManager) HandleWebSocket(w http.ResponseWriter, r *http.Request) {
	serviceID := r.URL.Query().Get("service_id")
	token := r.URL.Query().Get("token")

	if serviceID == "" || token == "" {
		http.Error(w, "service_id and token required", http.StatusBadRequest)
		return
	}

	if !tm.validateToken(serviceID, token) {
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		tm.logger.Error("WebSocket upgrade failed", zap.Error(err))
		return
	}

	defer conn.Close()

	tm.mu.Lock()
	tm.connections[serviceID] = conn
	tm.mu.Unlock()

	defer func() {
		tm.mu.Lock()
		delete(tm.connections, serviceID)
		// 清理该服务的所有隧道
		for tunnelID, tunnel := range tm.tunnels {
			if tunnel.ServiceID == serviceID {
				tm.closeTunnelInternal(tunnelID)
			}
		}
		tm.mu.Unlock()
	}()

	conn.SetReadLimit(tm.config.MaxMessageSize)
	conn.SetReadDeadline(time.Now().Add(tm.config.PongWait))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(tm.config.PongWait))
		return nil
	})

	go tm.keepAlive(conn, serviceID)

	for {
		messageType, message, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				tm.logger.Error("WebSocket error", zap.Error(err))
			}
			break
		}

		if messageType == websocket.TextMessage {
			tm.handleMessage(serviceID, message)
		}
	}
}

func (tm *TunnelManager) handleMessage(serviceID string, data []byte) {
	var msg models.Message
	if err := json.Unmarshal(data, &msg); err != nil {
		tm.logger.Error("Failed to unmarshal message", zap.Error(err))
		return
	}

	tm.logger.Debug("Message received",
		zap.String("type", msg.Type),
		zap.String("id", msg.ID),
		zap.String("service_id", serviceID))

	switch msg.Type {
	case models.MsgTypeRegister:
		tm.handleRegistration(serviceID, msg)
	case models.MsgTypeHeartbeat:
		tm.handleHeartbeat(serviceID, msg)
	case models.MsgTypeData:
		tm.handleData(serviceID, msg)
	case models.MsgTypeTunnelResp:
		tm.handleTunnelResponse(serviceID, msg)
	case models.MsgTypePong:
		// Already handled by SetPongHandler
	default:
		tm.logger.Warn("Unknown message type", zap.String("type", msg.Type))
	}
}

func (tm *TunnelManager) handleRegistration(serviceID string, msg models.Message) {
	var regReq models.RegistrationRequest
	data, _ := json.Marshal(msg.Payload)
	if err := json.Unmarshal(data, &regReq); err != nil {
		tm.sendError(serviceID, "invalid registration format")
		return
	}

	service := &models.Service{
		ID:       serviceID,
		Name:     regReq.ServiceName,
		Hostname: regReq.Hostname,
		IP:       regReq.IP,
		Ports:    regReq.Ports,
		Tags:     regReq.Tags,
		Status:   "online",
		Version:  regReq.Version,
	}

	if err := tm.registry.Register(service); err != nil {
		tm.sendError(serviceID, fmt.Sprintf("registration failed: %v", err))
		return
	}

	tm.mu.RLock()
	conn, exists := tm.connections[serviceID]
	tm.mu.RUnlock()

	if exists {
		service.Conn = conn
	}

	response := models.Message{
		Type: "registration_ack",
		Payload: map[string]string{
			"status":  "registered",
			"message": "Service registered successfully",
		},
	}

	if err := tm.sendMessage(serviceID, response); err != nil {
		tm.logger.Error("Failed to send registration ack", zap.Error(err))
	}
}

func (tm *TunnelManager) handleHeartbeat(serviceID string, msg models.Message) {
	var heartbeat models.Heartbeat
	data, _ := json.Marshal(msg.Payload)
	if err := json.Unmarshal(data, &heartbeat); err != nil {
		return
	}

	if err := tm.registry.UpdateHeartbeat(serviceID); err != nil {
		tm.logger.Warn("Heartbeat for unknown service", zap.String("service_id", serviceID))
	}
}

// 关键修复：处理从Agent返回的数据
func (tm *TunnelManager) handleData(serviceID string, msg models.Message) {
	var dataPacket models.DataPacket
	payload, _ := json.Marshal(msg.Payload)
	if err := json.Unmarshal(payload, &dataPacket); err != nil {
		tm.logger.Error("Failed to unmarshal data packet",
			zap.Error(err),
			zap.Any("payload", msg.Payload))
		return
	}

	tm.logger.Debug("Processing data from agent",
		zap.String("service_id", serviceID),
		zap.String("tunnel_id", dataPacket.TunnelID),
		zap.String("msg_id", msg.ID),
		zap.Int("data_size", len(dataPacket.Data)),
		zap.Bool("is_closing", dataPacket.IsClosing))

	// 对于浏览器兼容性：忽略关闭消息
	if dataPacket.IsClosing {
		tm.logger.Debug("Ignoring close message for browser compatibility",
			zap.String("tunnel_id", dataPacket.TunnelID),
			zap.String("service_id", serviceID))
		return
	}

	// 查找对应的连接隧道
	tm.mu.RLock()
	var targetTunnel *Tunnel

	// 首先尝试使用msg.ID查找连接隧道
	if msg.ID != "" {
		if tunnel, exists := tm.tunnels[msg.ID]; exists && tunnel.ServiceID == serviceID {
			targetTunnel = tunnel
		}
	}

	// 如果没找到，查找所有以原始隧道ID开头的连接隧道
	if targetTunnel == nil {
		for tunnelID, tunnel := range tm.tunnels {
			if strings.HasPrefix(tunnelID, dataPacket.TunnelID+"-conn-") &&
				tunnel.ServiceID == serviceID {
				targetTunnel = tunnel
				break
			}
		}
	}
	tm.mu.RUnlock()

	if targetTunnel == nil {
		tm.logger.Warn("No active connection tunnel found for data",
			zap.String("original_tunnel_id", dataPacket.TunnelID),
			zap.String("msg_id", msg.ID),
			zap.String("service_id", serviceID))
		return
	}

	// 写入数据到客户端连接
	targetTunnel.mu.RLock()
	conn := targetTunnel.conn
	targetTunnel.mu.RUnlock()

	if conn == nil {
		tm.logger.Error("Tunnel connection is nil",
			zap.String("tunnel_id", targetTunnel.ID))
		return
	}

	if len(dataPacket.Data) > 0 {
		startTime := time.Now()
		n, err := conn.Write(dataPacket.Data)
		writeDuration := time.Since(startTime)

		if err != nil {
			tm.logger.Error("Failed to write data to client",
				zap.String("tunnel_id", targetTunnel.ID),
				zap.Error(err),
				zap.Duration("duration", writeDuration))

			// 关闭这个连接隧道
			tm.closeTunnelInternal(targetTunnel.ID)
		} else {
			tm.logger.Debug("Successfully wrote data to client",
				zap.String("tunnel_id", targetTunnel.ID),
				zap.Int("bytes", n),
				zap.Duration("duration", writeDuration))

			// 检查是否为完整响应，如果是，可以关闭连接
			if tm.isCompleteResponse(dataPacket.Data) {
				tm.logger.Debug("Complete response sent, closing connection tunnel",
					zap.String("tunnel_id", targetTunnel.ID))
				tm.closeTunnelInternal(targetTunnel.ID)
			}
		}
	}
}

// 检查是否为完整的HTTP响应
func (tm *TunnelManager) isCompleteResponse(data []byte) bool {
	if len(data) < 14 {
		return false
	}

	dataStr := string(data)

	// 检查是否有头部结束标记
	headerEnd1 := strings.Index(dataStr, "\r\n\r\n")
	headerEnd2 := strings.Index(dataStr, "\n\n")

	if headerEnd1 == -1 && headerEnd2 == -1 {
		return false
	}

	// 如果是HTTP响应，检查状态码
	if strings.HasPrefix(dataStr, "HTTP/") {
		return true
	}

	return false
}

func (tm *TunnelManager) handleTunnelResponse(serviceID string, msg models.Message) {
	tm.logger.Info("Tunnel response received",
		zap.String("service_id", serviceID),
		zap.Any("message", msg))
}

func (tm *TunnelManager) CreateTunnel(serviceID string, targetPort int) (string, error) {
	tm.logger.Info("Creating tunnel",
		zap.String("service_id", serviceID),
		zap.Int("target_port", targetPort))

	tunnelID := fmt.Sprintf("tun-%s-%d-%d", serviceID, targetPort, time.Now().UnixNano())

	tm.mu.RLock()
	conn, exists := tm.connections[serviceID]
	tm.mu.RUnlock()

	if !exists {
		return "", fmt.Errorf("service %s not connected", serviceID)
	}

	// 验证连接是否存活
	if err := conn.WriteControl(websocket.PingMessage, []byte{}, time.Now().Add(5*time.Second)); err != nil {
		tm.logger.Warn("WebSocket connection dead, cleaning up",
			zap.String("service_id", serviceID),
			zap.Error(err))
		tm.mu.Lock()
		delete(tm.connections, serviceID)
		tm.mu.Unlock()
		return "", fmt.Errorf("websocket connection dead: %w", err)
	}

	// 创建监听器
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		tm.logger.Error("Failed to create listener",
			zap.String("tunnel_id", tunnelID),
			zap.Error(err))
		return "", fmt.Errorf("failed to create listener: %w", err)
	}

	localAddr := listener.Addr().String()
	tm.logger.Info("Listener created",
		zap.String("tunnel_id", tunnelID),
		zap.String("local_addr", localAddr))

	// 创建原始隧道对象
	tunnel := &Tunnel{
		ID:        tunnelID,
		ServiceID: serviceID,
		LocalAddr: localAddr,
		CreatedAt: time.Now(),
	}

	// 存储原始隧道
	tm.mu.Lock()
	tm.tunnels[tunnelID] = tunnel
	tm.listeners[tunnelID] = listener
	tm.mu.Unlock()

	// 发送隧道请求到agent
	tunnelReq := models.Message{
		Type: models.MsgTypeTunnelReq,
		ID:   tunnelID,
		Payload: map[string]interface{}{
			"tunnel_id":   tunnelID,
			"target_port": targetPort,
			"local_addr":  localAddr,
		},
	}

	if err := tm.sendMessage(serviceID, tunnelReq); err != nil {
		tm.logger.Error("Failed to send tunnel request",
			zap.String("tunnel_id", tunnelID),
			zap.String("service_id", serviceID),
			zap.Error(err))

		tm.mu.Lock()
		delete(tm.tunnels, tunnelID)
		delete(tm.listeners, tunnelID)
		tm.mu.Unlock()
		listener.Close()

		return "", err
	}

	// 开始接受连接
	go tm.acceptTunnelConnections(tunnelID, listener, serviceID, targetPort)

	tm.logger.Info("Tunnel creation completed",
		zap.String("tunnel_id", tunnelID),
		zap.String("local_addr", localAddr))

	return localAddr, nil
}

func (tm *TunnelManager) acceptTunnelConnections(tunnelID string, listener net.Listener, serviceID string, targetPort int) {
	tm.logger.Info("Starting tunnel listener",
		zap.String("tunnel_id", tunnelID),
		zap.String("listen_addr", listener.Addr().String()))

	defer func() {
		listener.Close()
		tm.mu.Lock()
		delete(tm.listeners, tunnelID)
		tm.mu.Unlock()
		tm.logger.Info("Tunnel listener stopped",
			zap.String("tunnel_id", tunnelID))
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			if strings.Contains(err.Error(), "use of closed network connection") {
				tm.logger.Info("Tunnel listener closed",
					zap.String("tunnel_id", tunnelID))
				break
			}
			tm.logger.Error("Failed to accept connection",
				zap.String("tunnel_id", tunnelID),
				zap.Error(err))
			continue
		}

		// 为每个连接创建唯一的隧道实例ID
		connectionTunnelID := fmt.Sprintf("%s-conn-%d", tunnelID, time.Now().UnixNano())

		tm.logger.Info("Accepted client connection",
			zap.String("connection_tunnel_id", connectionTunnelID),
			zap.String("client_addr", conn.RemoteAddr().String()),
			zap.String("original_tunnel_id", tunnelID))

		// 创建连接隧道实例
		tunnel := &Tunnel{
			ID:        connectionTunnelID,
			ServiceID: serviceID,
			LocalAddr: listener.Addr().String(),
			CreatedAt: time.Now(),
			wsConn:    nil,
			conn:      conn,
		}

		// 存储连接隧道
		tm.mu.Lock()
		tm.tunnels[connectionTunnelID] = tunnel
		tm.mu.Unlock()

		// 启动协程处理这个连接
		go tm.handleTunnelConnection(connectionTunnelID, conn, tunnelID)
	}
}

// 完整修复的handleTunnelConnection（方案B）
func (tm *TunnelManager) handleTunnelConnection(connectionTunnelID string, conn net.Conn, originalTunnelID string) {
	tm.logger.Info("Starting tunnel connection handler",
		zap.String("tunnel_id", connectionTunnelID),
		zap.String("client_addr", conn.RemoteAddr().String()))

	// 获取隧道信息
	tm.mu.RLock()
	tunnel, exists := tm.tunnels[connectionTunnelID]
	wsConn, serviceExists := tm.connections[tunnel.ServiceID]
	tm.mu.RUnlock()

	if !exists || !serviceExists {
		tm.logger.Error("Tunnel or service not found",
			zap.String("tunnel_id", connectionTunnelID))
		conn.Close()
		return
	}

	// 设置连接
	tunnel.mu.Lock()
	tunnel.wsConn = wsConn
	tunnel.conn = conn
	tunnel.mu.Unlock()

	// 设置连接超时
	conn.SetDeadline(time.Now().Add(5 * time.Minute))

	// 启动客户端到Agent的数据转发
	clientToAgentDone := make(chan bool, 1)

	go func() {
		defer func() {
			clientToAgentDone <- true
		}()

		buffer := make([]byte, 32*1024)
		totalSent := 0

		for {
			// 设置读取超时
			conn.SetReadDeadline(time.Now().Add(30 * time.Second))

			n, err := conn.Read(buffer)
			if err != nil {
				if err == io.EOF {
					tm.logger.Info("Client connection closed (EOF)",
						zap.String("tunnel_id", connectionTunnelID))
				} else if isTimeout(err) {
					tm.logger.Debug("Client read timeout, continuing",
						zap.String("tunnel_id", connectionTunnelID))
					continue
				} else {
					tm.logger.Error("Client read error",
						zap.String("tunnel_id", connectionTunnelID),
						zap.Error(err))
				}
				break
			}

			if n == 0 {
				continue
			}

			totalSent += n
			tm.logger.Debug("Read data from client",
				zap.String("tunnel_id", connectionTunnelID),
				zap.Int("bytes", n),
				zap.Int("total_sent", totalSent))

			// 发送到Agent
			dataPacket := models.DataPacket{
				TunnelID: originalTunnelID,
				Data:     buffer[:n],
			}

			msg := models.Message{
				Type:    models.MsgTypeData,
				ID:      connectionTunnelID,
				Payload: dataPacket,
			}

			if err := tm.sendMessage(tunnel.ServiceID, msg); err != nil {
				tm.logger.Error("Failed to send data to agent",
					zap.String("tunnel_id", connectionTunnelID),
					zap.Error(err))
				break
			}
		}
	}()

	// Agent到客户端的数据转发在handleData中处理

	// 等待客户端连接结束
	<-clientToAgentDone

	// 清理
	conn.Close()
	tm.closeTunnelInternal(connectionTunnelID)

	tm.logger.Info("Tunnel connection handler completed",
		zap.String("tunnel_id", connectionTunnelID),
		zap.String("client_addr", conn.RemoteAddr().String()))
}

// 检查是否为完整的HTTP请求
func (tm *TunnelManager) isCompleteRequest(data []byte) bool {
	if len(data) < 16 { // 最小请求: "GET / HTTP/1.1\r\n\r\n"
		return false
	}

	dataStr := string(data)

	// 检查是否有头部结束标记
	headerEnd1 := strings.Index(dataStr, "\r\n\r\n")
	headerEnd2 := strings.Index(dataStr, "\n\n")

	return headerEnd1 != -1 || headerEnd2 != -1
}

// 检查是否为HTTP请求
func (tm *TunnelManager) isHTTPRequest(data []byte) bool {
	str := string(data)
	return strings.HasPrefix(str, "GET ") ||
		strings.HasPrefix(str, "POST ") ||
		strings.HasPrefix(str, "PUT ") ||
		strings.HasPrefix(str, "DELETE ") ||
		strings.HasPrefix(str, "HEAD ") ||
		strings.HasPrefix(str, "OPTIONS ") ||
		strings.HasPrefix(str, "PATCH ") ||
		strings.HasPrefix(str, "CONNECT ")
}

// 处理HTTP请求，添加必要的头部
func (tm *TunnelManager) processHTTPRequest(data []byte, tunnelID string) []byte {
	str := string(data)

	// 确保有Host头部
	if !strings.Contains(str, "\r\nHost: ") && !strings.Contains(str, "\nHost: ") {
		// 在请求行后添加Host头部
		lines := strings.SplitN(str, "\r\n", 2)
		if len(lines) < 2 {
			lines = strings.SplitN(str, "\n", 2)
		}

		if len(lines) == 2 {
			requestLine := lines[0]
			remaining := lines[1]

			// 添加Host头部
			modified := requestLine + "\r\nHost: localhost\r\n" + remaining
			return []byte(modified)
		}
	}

	return data
}

func (tm *TunnelManager) closeTunnel(tunnelID string) {
	tm.closeTunnelInternal(tunnelID)
}

func (tm *TunnelManager) closeTunnelInternal(tunnelID string) {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	if tunnel, exists := tm.tunnels[tunnelID]; exists {
		tunnel.mu.Lock()
		if tunnel.conn != nil {
			tunnel.conn.Close()
			tunnel.conn = nil
		}
		tunnel.mu.Unlock()

		delete(tm.tunnels, tunnelID)

		tm.logger.Info("Tunnel closed",
			zap.String("tunnel_id", tunnelID),
			zap.String("service_id", tunnel.ServiceID))
	}

	// 检查是否是原始隧道ID，如果是则关闭监听器
	if strings.HasPrefix(tunnelID, "tun-") && !strings.Contains(tunnelID, "-conn-") {
		if listener, exists := tm.listeners[tunnelID]; exists {
			listener.Close()
			delete(tm.listeners, tunnelID)
			tm.logger.Info("Tunnel listener closed",
				zap.String("tunnel_id", tunnelID))
		}
	}
}

func (tm *TunnelManager) sendMessage(serviceID string, msg models.Message) error {
	tm.mu.RLock()
	conn, exists := tm.connections[serviceID]
	tm.mu.RUnlock()

	if !exists {
		return fmt.Errorf("service %s not connected", serviceID)
	}

	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("failed to marshal message: %w", err)
	}

	conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return conn.WriteMessage(websocket.TextMessage, data)
}

func (tm *TunnelManager) sendError(serviceID, errorMsg string) {
	msg := models.Message{
		Type:  models.MsgTypeError,
		Error: errorMsg,
	}
	tm.sendMessage(serviceID, msg)
}

func (tm *TunnelManager) keepAlive(conn *websocket.Conn, serviceID string) {
	ticker := time.NewTicker(tm.config.PingInterval)
	defer ticker.Stop()

	for {
		<-ticker.C
		conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
			tm.logger.Warn("Ping failed",
				zap.String("service_id", serviceID),
				zap.Error(err))
			return
		}
	}
}

func (tm *TunnelManager) validateToken(serviceID, token string) bool {
	return token != ""
}

// 辅助函数
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
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
