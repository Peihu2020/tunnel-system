package tunnel

import (
	"context"
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
	connections   map[string]*ConnectionWrapper // 改为包装器
	tunnels       map[string]*Tunnel
	listeners     map[string]net.Listener
	mu            sync.RWMutex
	logger        *zap.Logger
	config        *Config
	activeProxies map[string]*http.Server
}

// 包装WebSocket连接，添加写锁
type ConnectionWrapper struct {
	conn *websocket.Conn
	mu   sync.Mutex // 为每个连接添加互斥锁
}

type Tunnel struct {
	ID         string
	ServiceID  string
	LocalAddr  string
	RemoteAddr string
	CreatedAt  time.Time
	conn       net.Conn
	wsConn     *ConnectionWrapper
	closed     bool
	mu         sync.RWMutex
}

type Config struct {
	HeartbeatTimeout time.Duration
	BufferSize       int
	MaxMessageSize   int64
	PingInterval     time.Duration
	PongWait         time.Duration
	WriteWait        time.Duration
}

func NewTunnelManager(reg registry.ServiceRegistry, logger *zap.Logger, config *Config) *TunnelManager {
	if config == nil {
		config = &Config{}
	}

	if config.BufferSize < 4096 {
		logger.Warn("Buffer size too small, increasing to 32KB", zap.Int("original", config.BufferSize))
		config.BufferSize = 32 * 1024
	}

	if config.MaxMessageSize < 1024*1024 {
		logger.Warn("Max message size too small, increasing to 10MB", zap.Int64("original", config.MaxMessageSize))
		config.MaxMessageSize = 10 * 1024 * 1024
	}

	if config.PingInterval == 0 {
		config.PingInterval = 25 * time.Second
	}
	if config.PongWait == 0 {
		config.PongWait = 30 * time.Second
	}
	if config.WriteWait == 0 {
		config.WriteWait = 10 * time.Second
	}

	return &TunnelManager{
		registry:      reg,
		connections:   make(map[string]*ConnectionWrapper),
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

	// 创建包装器
	wrapper := &ConnectionWrapper{
		conn: conn,
	}

	tm.mu.Lock()
	tm.connections[serviceID] = wrapper
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
		tm.logger.Info("WebSocket connection closed", zap.String("service_id", serviceID))

		// 关闭WebSocket连接
		wrapper.mu.Lock()
		conn.Close()
		wrapper.mu.Unlock()
	}()

	conn.SetReadLimit(tm.config.MaxMessageSize)
	conn.SetReadDeadline(time.Now().Add(tm.config.PongWait))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(tm.config.PongWait))
		return nil
	})

	go tm.keepAlive(wrapper, serviceID)

	tm.logger.Info("WebSocket connection established",
		zap.String("service_id", serviceID),
		zap.String("remote_addr", r.RemoteAddr))

	for {
		messageType, message, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				tm.logger.Error("WebSocket error", zap.Error(err))
			} else {
				tm.logger.Debug("WebSocket closed", zap.String("service_id", serviceID))
			}
			break
		}

		if messageType == websocket.TextMessage {
			go tm.handleMessage(serviceID, message, wrapper)
		} else if messageType == websocket.PingMessage {
			// 响应Ping
			wrapper.mu.Lock()
			conn.SetWriteDeadline(time.Now().Add(tm.config.WriteWait))
			conn.WriteMessage(websocket.PongMessage, nil)
			wrapper.mu.Unlock()
		}
	}
}

func (tm *TunnelManager) handleMessage(serviceID string, data []byte, wrapper *ConnectionWrapper) {
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
		tm.handleRegistration(serviceID, msg, wrapper)
	case models.MsgTypeHeartbeat:
		tm.handleHeartbeat(serviceID, msg)
	case models.MsgTypeData:
		tm.handleData(serviceID, msg, wrapper)
	case models.MsgTypeTunnelResp:
		tm.handleTunnelResponse(serviceID, msg)
	case models.MsgTypePong:
		tm.logger.Debug("Pong received", zap.String("service_id", serviceID))
	default:
		tm.logger.Warn("Unknown message type", zap.String("type", msg.Type))
	}
}

func (tm *TunnelManager) handleRegistration(serviceID string, msg models.Message, wrapper *ConnectionWrapper) {
	var regReq models.RegistrationRequest
	data, _ := json.Marshal(msg.Payload)
	if err := json.Unmarshal(data, &regReq); err != nil {
		tm.sendError(serviceID, "invalid registration format", wrapper)
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
		tm.sendError(serviceID, fmt.Sprintf("registration failed: %v", err), wrapper)
		return
	}

	response := models.Message{
		Type: "registration_ack",
		Payload: map[string]string{
			"status":  "registered",
			"message": "Service registered successfully",
		},
	}

	if err := tm.sendMessage(serviceID, response, wrapper); err != nil {
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

func (tm *TunnelManager) handleData(serviceID string, msg models.Message, wrapper *ConnectionWrapper) {
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
				tm.logger.Debug("Complete response sent",
					zap.String("tunnel_id", targetTunnel.ID))
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
	wrapper, exists := tm.connections[serviceID]
	tm.mu.RUnlock()

	if !exists {
		return "", fmt.Errorf("service %s not connected", serviceID)
	}

	// 验证连接是否存活
	wrapper.mu.Lock()
	err := wrapper.conn.WriteControl(websocket.PingMessage, []byte{}, time.Now().Add(5*time.Second))
	wrapper.mu.Unlock()
	if err != nil {
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
		wsConn:    wrapper,
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

	if err := tm.sendMessage(serviceID, tunnelReq, wrapper); err != nil {
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
	go tm.acceptTunnelConnections(tunnelID, listener, serviceID, targetPort, wrapper)

	tm.logger.Info("Tunnel creation completed",
		zap.String("tunnel_id", tunnelID),
		zap.String("local_addr", localAddr))

	return localAddr, nil
}

func (tm *TunnelManager) acceptTunnelConnections(tunnelID string, listener net.Listener, serviceID string, targetPort int, wrapper *ConnectionWrapper) {
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
			wsConn:    wrapper,
			conn:      conn,
		}

		// 存储连接隧道
		tm.mu.Lock()
		tm.tunnels[connectionTunnelID] = tunnel
		tm.mu.Unlock()

		// 启动协程处理这个连接
		go tm.handleTunnelConnection(connectionTunnelID, conn, tunnelID, wrapper)
	}
}

// 修复后的handleTunnelConnection，使用带锁的连接包装器
func (tm *TunnelManager) handleTunnelConnection(connectionTunnelID string, conn net.Conn, originalTunnelID string, wrapper *ConnectionWrapper) {
	tm.logger.Info("Starting tunnel connection handler",
		zap.String("tunnel_id", connectionTunnelID),
		zap.String("client_addr", conn.RemoteAddr().String()))

	// 获取隧道信息
	tm.mu.RLock()
	tunnel, exists := tm.tunnels[connectionTunnelID]
	tm.mu.RUnlock()

	if !exists {
		tm.logger.Error("Tunnel not found",
			zap.String("tunnel_id", connectionTunnelID))
		conn.Close()
		return
	}

	// 设置连接
	tunnel.mu.Lock()
	tunnel.conn = conn
	tunnel.mu.Unlock()

	// 使用context控制goroutine生命周期
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 设置合理的超时
	conn.SetDeadline(time.Now().Add(60 * time.Second))

	// 启动客户端到Agent的数据转发
	clientToAgentDone := make(chan struct{})
	errorChan := make(chan error, 1)

	go func() {
		defer func() {
			close(clientToAgentDone)
			cancel()
		}()

		buffer := make([]byte, 32*1024)
		totalSent := 0

		for {
			select {
			case <-ctx.Done():
				return
			default:
				// 设置读取超时
				conn.SetReadDeadline(time.Now().Add(5 * time.Second))

				n, err := conn.Read(buffer)
				if err != nil {
					if err == io.EOF {
						tm.logger.Debug("Client connection closed (EOF)",
							zap.String("tunnel_id", connectionTunnelID))
						return
					} else if isTimeout(err) {
						// 超时，检查连接是否仍然存活
						if tm.isConnectionClosed(conn) {
							tm.logger.Debug("Client connection appears closed",
								zap.String("tunnel_id", connectionTunnelID))
							return
						}
						// 连接仍然存活，继续等待
						continue
					} else if strings.Contains(err.Error(), "use of closed network connection") ||
						strings.Contains(err.Error(), "connection reset by peer") {
						// 客户端主动关闭连接，这是正常行为
						tm.logger.Debug("Client closed connection",
							zap.String("tunnel_id", connectionTunnelID))
						return
					} else {
						tm.logger.Warn("Client read error",
							zap.String("tunnel_id", connectionTunnelID),
							zap.Error(err))
						errorChan <- err
						return
					}
				}

				if n == 0 {
					continue
				}

				totalSent += n
				tm.logger.Debug("Read data from client",
					zap.String("tunnel_id", connectionTunnelID),
					zap.Int("bytes", n),
					zap.Int("total_sent", totalSent))

				// 发送到Agent（使用带锁的连接）
				dataPacket := models.DataPacket{
					TunnelID: originalTunnelID,
					Data:     buffer[:n],
				}

				msg := models.Message{
					Type:    models.MsgTypeData,
					ID:      connectionTunnelID,
					Payload: dataPacket,
				}

				if err := tm.sendMessage(tunnel.ServiceID, msg, wrapper); err != nil {
					tm.logger.Error("Failed to send data to agent",
						zap.String("tunnel_id", connectionTunnelID),
						zap.Error(err))
					errorChan <- err
					return
				}

				// 每次成功读取后重置超时
				conn.SetDeadline(time.Now().Add(60 * time.Second))
			}
		}
	}()

	// 等待处理完成或超时
	select {
	case <-clientToAgentDone:
		tm.logger.Debug("Client to agent transfer completed normally",
			zap.String("tunnel_id", connectionTunnelID))
	case err := <-errorChan:
		tm.logger.Warn("Error in tunnel connection",
			zap.String("tunnel_id", connectionTunnelID),
			zap.Error(err))
	case <-time.After(60 * time.Second):
		tm.logger.Warn("Tunnel connection timeout",
			zap.String("tunnel_id", connectionTunnelID))
	}

	// 清理
	conn.Close()
	tm.closeTunnelInternal(connectionTunnelID)

	tm.logger.Info("Tunnel connection handler completed",
		zap.String("tunnel_id", connectionTunnelID),
		zap.String("client_addr", conn.RemoteAddr().String()))
}

// 添加连接状态检查函数
func (tm *TunnelManager) isConnectionClosed(conn net.Conn) bool {
	// 尝试写一个字节来检查连接状态
	conn.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
	_, err := conn.Write([]byte{})
	return err != nil
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

// 修改sendMessage，使用带锁的连接
func (tm *TunnelManager) sendMessage(serviceID string, msg models.Message, wrapper *ConnectionWrapper) error {
	if wrapper == nil {
		tm.mu.RLock()
		_, exists := tm.connections[serviceID]
		tm.mu.RUnlock()

		if !exists {
			return fmt.Errorf("service %s not connected", serviceID)
		}
	}

	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("failed to marshal message: %w", err)
	}

	// 使用连接锁确保线程安全
	wrapper.mu.Lock()
	defer wrapper.mu.Unlock()

	wrapper.conn.SetWriteDeadline(time.Now().Add(tm.config.WriteWait))
	return wrapper.conn.WriteMessage(websocket.TextMessage, data)
}

func (tm *TunnelManager) sendError(serviceID, errorMsg string, wrapper *ConnectionWrapper) {
	msg := models.Message{
		Type:  models.MsgTypeError,
		Error: errorMsg,
	}
	tm.sendMessage(serviceID, msg, wrapper)
}

func (tm *TunnelManager) keepAlive(wrapper *ConnectionWrapper, serviceID string) {
	ticker := time.NewTicker(tm.config.PingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			wrapper.mu.Lock()
			wrapper.conn.SetWriteDeadline(time.Now().Add(tm.config.WriteWait))
			err := wrapper.conn.WriteMessage(websocket.PingMessage, nil)
			wrapper.mu.Unlock()

			if err != nil {
				tm.logger.Debug("Ping failed, connection may be closed",
					zap.String("service_id", serviceID),
					zap.Error(err))
				return
			}
		}
	}
}

func (tm *TunnelManager) validateToken(serviceID, token string) bool {
	// 实际实现应该验证token有效性
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
