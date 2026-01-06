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
	ReadBufferSize:  64 * 1024, // 增加到64KB
	WriteBufferSize: 64 * 1024, // 增加到64KB
	CheckOrigin: func(r *http.Request) bool {
		// In production, validate origin properly
		return true
	},
}

type TunnelManager struct {
	registry      registry.ServiceRegistry
	connections   map[string]*websocket.Conn // serviceID -> connection
	tunnels       map[string]*Tunnel         // tunnelID -> tunnel
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
	mu         sync.RWMutex // 为每个隧道添加锁
}

type Config struct {
	HeartbeatTimeout time.Duration
	BufferSize       int
	MaxMessageSize   int64
	PingInterval     time.Duration
	PongWait         time.Duration
}

func NewTunnelManager(reg registry.ServiceRegistry, logger *zap.Logger, config *Config) *TunnelManager {
	// 验证配置
	if config.BufferSize < 4096 {
		logger.Warn("Buffer size too small, increasing to 32KB", zap.Int("original", config.BufferSize))
		config.BufferSize = 32 * 1024
	}

	if config.MaxMessageSize < 1024*1024 { // 小于1MB
		logger.Warn("Max message size too small, increasing to 10MB", zap.Int64("original", config.MaxMessageSize))
		config.MaxMessageSize = 10 * 1024 * 1024
	}

	return &TunnelManager{
		registry:      reg,
		connections:   make(map[string]*websocket.Conn),
		tunnels:       make(map[string]*Tunnel),
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

	// Validate token (simplified - in production use proper JWT validation)
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

	// Register connection
	tm.mu.Lock()
	tm.connections[serviceID] = conn
	tm.mu.Unlock()

	defer func() {
		tm.mu.Lock()
		delete(tm.connections, serviceID)
		tm.mu.Unlock()
	}()

	// Set connection parameters
	conn.SetReadLimit(tm.config.MaxMessageSize)
	conn.SetReadDeadline(time.Now().Add(tm.config.PongWait))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(tm.config.PongWait))
		return nil
	})

	// Start ping pong
	go tm.keepAlive(conn, serviceID)

	// Handle messages
	for {
		messageType, message, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				tm.logger.Error("WebSocket error", zap.Error(err))
			}
			break
		}

		tm.logger.Debug("WebSocket message received",
			zap.Int("message_type", messageType),
			zap.Int("message_length", len(message)),
			zap.String("service_id", serviceID))

		if messageType == websocket.TextMessage {
			tm.handleMessage(serviceID, message)
		} else if messageType == websocket.BinaryMessage {
			tm.handleBinaryData(serviceID, message)
		}
	}
}

func (tm *TunnelManager) handleMessage(serviceID string, data []byte) {
	tm.logger.Info("=== handleMessage CALLED ===",
		zap.String("service_id", serviceID),
		zap.Int("data_length", len(data)),
		zap.String("data_preview", string(data[:min(200, len(data))])))

	var msg models.Message
	if err := json.Unmarshal(data, &msg); err != nil {
		tm.logger.Error("Failed to unmarshal message", zap.Error(err))
		return
	}

	tm.logger.Info("Message parsed",
		zap.String("type", msg.Type),
		zap.String("id", msg.ID),
		zap.Any("payload_keys", getMapKeys(msg.Payload)))

	switch msg.Type {
	case models.MsgTypeRegister:
		tm.logger.Info("Processing register message")
		tm.handleRegistration(serviceID, msg)
	case models.MsgTypeHeartbeat:
		tm.logger.Info("Processing heartbeat message")
		tm.handleHeartbeat(serviceID, msg)
	case models.MsgTypeData:
		tm.logger.Info("Processing DATA message")
		tm.handleData(serviceID, msg)
	case models.MsgTypeTunnelResp:
		tm.logger.Info("Processing tunnel response")
		tm.handleTunnelResponse(serviceID, msg)
	case models.MsgTypePong:
		tm.logger.Debug("Processing pong")
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

	// Store connection in service
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

func (tm *TunnelManager) handleData(serviceID string, msg models.Message) {
	tm.logger.Info("=== handleData ENTER ===",
		zap.String("service_id", serviceID),
		zap.String("msg_id", msg.ID),
		zap.String("msg_type", msg.Type))

	var dataPacket models.DataPacket
	payload, _ := json.Marshal(msg.Payload)
	if err := json.Unmarshal(payload, &dataPacket); err != nil {
		tm.logger.Error("Failed to unmarshal data packet",
			zap.Error(err),
			zap.Any("payload", msg.Payload))
		return
	}

	tm.logger.Info("Data packet parsed",
		zap.String("tunnel_id", dataPacket.TunnelID),
		zap.Int("data_size", len(dataPacket.Data)),
		zap.Bool("is_closing", dataPacket.IsClosing),
		zap.Int("sequence", int(dataPacket.Sequence)),
		zap.ByteString("data_preview", dataPacket.Data[:min(100, len(dataPacket.Data))]))

	tm.mu.RLock()
	tunnel, exists := tm.tunnels[dataPacket.TunnelID]
	tm.mu.RUnlock()

	// 获取客户端地址
	clientAddr := "none"
	if exists && tunnel.conn != nil {
		clientAddr = tunnel.conn.RemoteAddr().String()
	}

	tm.logger.Info("Tunnel lookup result",
		zap.String("tunnel_id", dataPacket.TunnelID),
		zap.Bool("exists", exists),
		zap.Bool("conn_nil", exists && tunnel.conn == nil),
		zap.String("client_addr", clientAddr))

	if !exists {
		tm.logger.Warn("Received data for unknown tunnel",
			zap.String("tunnel_id", dataPacket.TunnelID),
			zap.Any("existing_tunnels", tm.getTunnelIDs()))
		return
	}

	if dataPacket.IsClosing {
		tm.logger.Info("Closing tunnel via data packet",
			zap.String("tunnel_id", dataPacket.TunnelID))
		tm.closeTunnel(dataPacket.TunnelID)
		return
	}

	// Write data to the local connection
	if tunnel.conn == nil {
		tm.logger.Error("Tunnel connection is nil, cannot write data",
			zap.String("tunnel_id", dataPacket.TunnelID))
		return
	}

	// 如果是HTTP响应，检查并可能添加CORS头
	modifiedData := dataPacket.Data
	if len(dataPacket.Data) > 0 {
		// 检查是否是HTTP响应
		dataStr := string(dataPacket.Data)
		isHttpResponse := strings.Contains(dataStr, "HTTP/1.") ||
			strings.Contains(dataStr, "HTTP/2") ||
			strings.Contains(dataStr, "HTTP/3")

		if isHttpResponse && strings.Contains(dataStr, "\r\n\r\n") {
			// 尝试添加CORS头
			modifiedData = addCorsHeaders(dataPacket.Data)

			if len(modifiedData) != len(dataPacket.Data) {
				tm.logger.Info("Added CORS headers to HTTP response",
					zap.String("tunnel_id", dataPacket.TunnelID),
					zap.Int("original_size", len(dataPacket.Data)),
					zap.Int("modified_size", len(modifiedData)))
			}
		}
	}

	tm.logger.Info("Writing data to client connection",
		zap.String("tunnel_id", dataPacket.TunnelID),
		zap.Int("bytes_to_write", len(modifiedData)),
		zap.String("client_addr", clientAddr))

	start := time.Now()
	n, err := tunnel.conn.Write(modifiedData)
	duration := time.Since(start)

	if err != nil {
		tm.logger.Error("Failed to write tunnel data to client",
			zap.String("tunnel_id", dataPacket.TunnelID),
			zap.Error(err),
			zap.Int("bytes_tried", len(modifiedData)),
			zap.Int("bytes_written", n),
			zap.Duration("write_duration", duration))
		tm.closeTunnel(dataPacket.TunnelID)
	} else {
		tm.logger.Info("Successfully wrote data to client",
			zap.String("tunnel_id", dataPacket.TunnelID),
			zap.Int("bytes_written", n),
			zap.Duration("write_duration", duration),
			zap.Int("expected_bytes", len(modifiedData)))
	}
}

// 添加CORS头到HTTP响应
func addCorsHeaders(data []byte) []byte {
	dataStr := string(data)

	// 查找头部和主体的分界
	headerBodySplit := "\r\n\r\n"
	splitIndex := strings.Index(dataStr, headerBodySplit)
	if splitIndex == -1 {
		// 如果没有找到标准的分界，尝试其他格式
		headerBodySplit = "\n\n"
		splitIndex = strings.Index(dataStr, headerBodySplit)
		if splitIndex == -1 {
			return data // 无法解析，返回原数据
		}
	}

	headers := dataStr[:splitIndex]
	body := dataStr[splitIndex+len(headerBodySplit):]

	// 检查是否已有CORS头
	if strings.Contains(strings.ToLower(headers), "access-control-allow-origin") {
		return data // 已经有CORS头，返回原数据
	}

	// 添加CORS头
	corsHeaders := []string{
		"Access-Control-Allow-Origin: *",
		"Access-Control-Allow-Methods: GET, POST, PUT, DELETE, OPTIONS",
		"Access-Control-Allow-Headers: Content-Type, Authorization, X-Requested-With",
		"Access-Control-Allow-Credentials: true",
		"Access-Control-Expose-Headers: Content-Length, Content-Range",
		"Access-Control-Max-Age: 86400", // 24小时
	}

	// 在现有头部后添加CORS头
	// 查找最后一个头部行
	lines := strings.Split(headers, "\r\n")
	if len(lines) == 1 {
		lines = strings.Split(headers, "\n") // 尝试Unix换行符
	}

	// 构建新的头部
	var newHeaders strings.Builder
	for _, line := range lines {
		newHeaders.WriteString(line)
		newHeaders.WriteString("\r\n")
	}

	// 添加CORS头
	for _, corsHeader := range corsHeaders {
		newHeaders.WriteString(corsHeader)
		newHeaders.WriteString("\r\n")
	}

	// 重建响应
	result := newHeaders.String() + "\r\n" + body
	return []byte(result)
}

// 获取所有隧道ID
func (tm *TunnelManager) getTunnelIDs() []string {
	tm.mu.RLock()
	defer tm.mu.RUnlock()

	ids := make([]string, 0, len(tm.tunnels))
	for id := range tm.tunnels {
		ids = append(ids, id)
	}
	return ids
}

func (tm *TunnelManager) handleTunnelResponse(serviceID string, msg models.Message) {
	// Handle tunnel establishment response from agent
	tm.logger.Info("Tunnel response received",
		zap.String("service_id", serviceID),
		zap.Any("message", msg))
}

func (tm *TunnelManager) handleBinaryData(serviceID string, data []byte) {
	tm.logger.Info("=== handleBinaryData CALLED ===",
		zap.String("service_id", serviceID),
		zap.Int("data_length", len(data)))

	if len(data) < 36 {
		tm.logger.Error("Binary data too short",
			zap.Int("length", len(data)),
			zap.String("service_id", serviceID),
			zap.ByteString("first_10_bytes", data[:min(10, len(data))]))
		return
	}

	tunnelID := string(data[:36])
	tunnelData := data[36:]

	tm.logger.Info("Parsed binary data",
		zap.String("tunnel_id", tunnelID),
		zap.Int("tunnel_data_length", len(tunnelData)))

	tm.mu.RLock()
	tunnel, exists := tm.tunnels[tunnelID]
	tm.mu.RUnlock()

	if !exists {
		tm.logger.Error("Tunnel not found for binary data",
			zap.String("tunnel_id", tunnelID),
			zap.String("service_id", serviceID),
			zap.Any("existing_tunnels", tm.getTunnelIDs()))
		return
	}

	if tunnel.conn == nil {
		tm.logger.Error("Tunnel connection is nil",
			zap.String("tunnel_id", tunnelID),
			zap.String("service_id", serviceID))
		return
	}

	tm.logger.Info("Writing binary data to client",
		zap.String("tunnel_id", tunnelID),
		zap.Int("data_size", len(tunnelData)))

	start := time.Now()
	n, err := tunnel.conn.Write(tunnelData)
	duration := time.Since(start)

	if err != nil {
		tm.logger.Error("Failed to write binary data to client",
			zap.String("tunnel_id", tunnelID),
			zap.Error(err),
			zap.Int("bytes_tried", len(tunnelData)),
			zap.Int("bytes_written", n),
			zap.Duration("write_duration", duration))
		tm.closeTunnel(tunnelID)
	} else {
		tm.logger.Info("Successfully wrote binary data to client",
			zap.String("tunnel_id", tunnelID),
			zap.Int("bytes_written", n),
			zap.Duration("write_duration", duration))
	}
}

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

func (tm *TunnelManager) CreateTunnel(serviceID string, targetPort int) (string, error) {
	tm.logger.Info("=== CreateTunnel CALLED ===",
		zap.String("service_id", serviceID),
		zap.Int("target_port", targetPort))

	// Generate tunnel ID
	tunnelID := fmt.Sprintf("tun-%s-%d-%d", serviceID, targetPort, time.Now().UnixNano())
	tm.logger.Info("Generated tunnel ID",
		zap.String("tunnel_id", tunnelID))

	tm.mu.RLock()
	conn, exists := tm.connections[serviceID]
	tm.mu.RUnlock()

	tm.logger.Info("WebSocket connection check",
		zap.String("service_id", serviceID),
		zap.Bool("exists", exists),
		zap.Bool("conn_nil", conn == nil))

	if !exists {
		return "", fmt.Errorf("service %s not connected", serviceID)
	}

	// Verify connection is alive before sending tunnel request
	if err := conn.WriteControl(websocket.PingMessage, []byte{}, time.Now().Add(5*time.Second)); err != nil {
		tm.logger.Warn("WebSocket connection dead, cleaning up",
			zap.String("service_id", serviceID),
			zap.Error(err))
		tm.mu.Lock()
		delete(tm.connections, serviceID)
		tm.mu.Unlock()
		return "", fmt.Errorf("websocket connection dead: %w", err)
	}

	// Create listener for this tunnel
	listener, err := net.Listen("tcp", "0.0.0.0:0") // 监听所有接口
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

	tunnel := &Tunnel{
		ID:        tunnelID,
		ServiceID: serviceID,
		LocalAddr: localAddr,
		CreatedAt: time.Now(),
		wsConn:    conn,
	}

	tm.mu.Lock()
	tm.tunnels[tunnelID] = tunnel
	tm.mu.Unlock()

	tm.logger.Info("Tunnel stored in manager",
		zap.String("tunnel_id", tunnelID),
		zap.Int("total_tunnels", len(tm.tunnels)))

	// Send tunnel request to agent
	tunnelReq := models.Message{
		Type: models.MsgTypeTunnelReq,
		ID:   tunnelID,
		Payload: map[string]interface{}{
			"tunnel_id":   tunnelID,
			"target_port": targetPort,
			"local_addr":  localAddr,
		},
	}

	tm.logger.Info("Sending tunnel request to agent",
		zap.String("tunnel_id", tunnelID),
		zap.String("service_id", serviceID),
		zap.Any("request", tunnelReq))

	if err := tm.sendMessage(serviceID, tunnelReq); err != nil {
		tm.logger.Error("Failed to send tunnel request",
			zap.String("tunnel_id", tunnelID),
			zap.String("service_id", serviceID),
			zap.Error(err))

		tm.mu.Lock()
		delete(tm.tunnels, tunnelID)
		tm.mu.Unlock()
		listener.Close()

		return "", err
	}

	tm.logger.Info("Tunnel request sent successfully",
		zap.String("tunnel_id", tunnelID))

	// Start accepting connections on local listener
	go tm.acceptTunnelConnections(tunnelID, listener, targetPort)

	tm.logger.Info("Tunnel creation completed",
		zap.String("tunnel_id", tunnelID),
		zap.String("local_addr", localAddr))

	return localAddr, nil
}

// func (tm *TunnelManager) acceptTunnelConnections(tunnelID string, listener net.Listener, targetPort int) {
// 	// Keep listening for multiple connections
// 	for {
// 		conn, err := listener.Accept()
// 		if err != nil {
// 			tm.logger.Error("Listener accept error", zap.Error(err))
// 			continue // Don't break on error
// 		}

// 		// Handle each connection in its own goroutine
// 		go tm.handleTunnelConnection(tunnelID, conn)
// 	}
// }

func (tm *TunnelManager) acceptTunnelConnections(tunnelID string, listener net.Listener, targetPort int) {
	defer listener.Close()

	tm.logger.Info("Waiting for client connection",
		zap.String("tunnel_id", tunnelID),
		zap.String("listen_addr", listener.Addr().String()))

	// Accept only one connection per tunnel
	conn, err := listener.Accept()
	if err != nil {
		tm.logger.Error("Failed to accept tunnel connection",
			zap.Error(err), zap.String("tunnel_id", tunnelID))
		return
	}

	tm.logger.Info("Accepted client connection",
		zap.String("tunnel_id", tunnelID),
		zap.String("client_addr", conn.RemoteAddr().String()))

	tm.mu.RLock()
	tunnel, exists := tm.tunnels[tunnelID]
	tm.mu.RUnlock()

	if !exists {
		conn.Close()
		return
	}

	// Store connection in tunnel with lock
	tunnel.mu.Lock()
	tunnel.conn = conn
	tunnel.mu.Unlock()

	// Handle connection data
	tm.handleTunnelConnection(tunnelID, conn)

	tm.logger.Info("Tunnel connection completed",
		zap.String("tunnel_id", tunnelID))
}

func (tm *TunnelManager) handleTunnelConnection(tunnelID string, conn net.Conn) {
	startTime := time.Now()
	clientAddr := conn.RemoteAddr().String()

	tm.logger.Info("=== Tunnel connection STARTED ===",
		zap.String("tunnel_id", tunnelID),
		zap.String("client_addr", clientAddr),
		zap.Time("start_time", startTime))

	defer func() {
		duration := time.Since(startTime)
		tm.logger.Info("=== Tunnel connection ENDED ===",
			zap.String("tunnel_id", tunnelID),
			zap.String("client_addr", clientAddr),
			zap.Duration("duration", duration),
			zap.Float64("duration_seconds", duration.Seconds()),
			zap.Time("end_time", time.Now()))
	}()

	tm.mu.RLock()
	tunnel, exists := tm.tunnels[tunnelID]
	tm.mu.RUnlock()

	if !exists {
		tm.logger.Error("Tunnel does not exist at connection start",
			zap.String("tunnel_id", tunnelID))
		return
	}

	tm.logger.Info("Tunnel found for connection",
		zap.String("tunnel_id", tunnelID),
		zap.String("service_id", tunnel.ServiceID),
		zap.Bool("ws_conn_exists", tunnel.wsConn != nil))

	// 设置更长的超时
	conn.SetDeadline(time.Now().Add(10 * time.Minute))

	// Set up bidirectional copy
	done := make(chan bool, 2)

	// Client -> WebSocket
	go func() {
		buffer := make([]byte, tm.config.BufferSize)
		totalRead := 0

		tm.logger.Info("Starting client->websocket forwarding",
			zap.String("tunnel_id", tunnelID))

		for {
			tm.logger.Debug("Waiting to read from client connection",
				zap.String("tunnel_id", tunnelID))

			n, err := conn.Read(buffer)
			if err != nil {
				if err != io.EOF {
					tm.logger.Error("Client read error",
						zap.String("tunnel_id", tunnelID),
						zap.Error(err),
						zap.String("error_type", fmt.Sprintf("%T", err)))
				} else {
					tm.logger.Info("Client connection EOF",
						zap.String("tunnel_id", tunnelID))
				}
				break
			}

			if n > 0 {
				totalRead += n
				tm.logger.Info("Read from client",
					zap.String("tunnel_id", tunnelID),
					zap.Int("bytes", n),
					zap.Int("total_read", totalRead),
					zap.ByteString("first_few_bytes", buffer[:min(10, n)]))

				dataPacket := models.DataPacket{
					TunnelID: tunnelID,
					Data:     buffer[:n],
				}

				msg := models.Message{
					Type:    models.MsgTypeData,
					Payload: dataPacket,
				}

				tm.logger.Debug("Sending data to agent",
					zap.String("tunnel_id", tunnelID),
					zap.Int("data_size", n))

				if err := tm.sendMessage(tunnel.ServiceID, msg); err != nil {
					tm.logger.Error("Failed to forward data to agent",
						zap.String("tunnel_id", tunnelID),
						zap.Error(err))
					break
				}

				tm.logger.Debug("Data sent to agent successfully",
					zap.String("tunnel_id", tunnelID))
			}
		}

		tm.logger.Info("Client->websocket forwarding ended",
			zap.String("tunnel_id", tunnelID),
			zap.Int("total_bytes_read", totalRead))

		done <- true
	}()

	// WebSocket -> Client (handled by handleData and handleBinaryData)
	// We just need to keep this goroutine alive

	tm.logger.Info("Waiting for client connection to close",
		zap.String("tunnel_id", tunnelID))

	// Wait for client to close or error
	<-done

	tm.logger.Info("Client connection finished",
		zap.String("tunnel_id", tunnelID))
}

func (tm *TunnelManager) closeTunnel(tunnelID string) {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	if tunnel, exists := tm.tunnels[tunnelID]; exists {
		// 使用隧道的锁来安全地关闭连接
		tunnel.mu.Lock()
		if tunnel.conn != nil {
			tunnel.conn.Close()
			tunnel.conn = nil
		}
		tunnel.mu.Unlock()

		delete(tm.tunnels, tunnelID)

		tm.logger.Info("Tunnel closed",
			zap.String("tunnel_id", tunnelID),
			zap.String("service_id", tunnel.ServiceID),
			zap.Float64("duration_seconds", time.Since(tunnel.CreatedAt).Seconds()))
	}
}

func (tm *TunnelManager) sendMessage(serviceID string, msg models.Message) error {
	tm.logger.Info("=== sendMessage ENTER ===",
		zap.String("service_id", serviceID),
		zap.String("msg_type", msg.Type),
		zap.String("msg_id", msg.ID))

	tm.mu.RLock()
	conn, exists := tm.connections[serviceID]
	tm.mu.RUnlock()

	if !exists {
		tm.logger.Error("sendMessage: service not connected",
			zap.String("service_id", serviceID))
		return fmt.Errorf("service %s not connected", serviceID)
	}

	data, err := json.Marshal(msg)
	if err != nil {
		tm.logger.Error("sendMessage: marshal failed",
			zap.String("service_id", serviceID),
			zap.Error(err))
		return fmt.Errorf("failed to marshal message: %w", err)
	}

	tm.logger.Debug("Message prepared",
		zap.String("service_id", serviceID),
		zap.Int("data_len", len(data)))

	conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	start := time.Now()
	err = conn.WriteMessage(websocket.TextMessage, data)
	duration := time.Since(start)

	if err != nil {
		tm.logger.Error("sendMessage: write failed",
			zap.String("service_id", serviceID),
			zap.Error(err),
			zap.Duration("duration", duration))
		return err
	}

	tm.logger.Info("sendMessage: success",
		zap.String("service_id", serviceID),
		zap.String("msg_type", msg.Type),
		zap.Int("bytes_sent", len(data)),
		zap.Duration("duration", duration))

	return nil
}

func (tm *TunnelManager) sendError(serviceID, errorMsg string) {
	msg := models.Message{
		Type:  models.MsgTypeError,
		Error: errorMsg,
	}

	tm.sendMessage(serviceID, msg)
}

func (tm *TunnelManager) keepAlive(conn *websocket.Conn, serviceID string) {
	tm.logger.Info("Starting keepAlive for service",
		zap.String("service_id", serviceID))

	ticker := time.NewTicker(tm.config.PingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				tm.logger.Warn("Ping failed",
					zap.String("service_id", serviceID),
					zap.Error(err))
				return
			}
			tm.logger.Debug("Ping sent",
				zap.String("service_id", serviceID))
		}
	}
}

func (tm *TunnelManager) validateToken(serviceID, token string) bool {
	// In production, implement proper JWT validation
	// For now, accept any non-empty token
	return token != ""
}
