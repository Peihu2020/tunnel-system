package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"tunnel-control/internal/config"
	"tunnel-control/internal/registry"
	"tunnel-control/internal/tunnel"

	"github.com/gorilla/mux"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func main() {
	// Load configuration
	cfg, err := config.Load("configs/tunnel-control.yaml")
	if err != nil {
		fmt.Printf("Failed to load config: %v\n", err)
		os.Exit(1)
	}

	// Initialize logger
	logger := initLogger(cfg.Logging.Level, cfg.Logging.Format)
	defer logger.Sync()

	logger.Info("Starting Tunnel Control Server",
		zap.String("version", "1.0.0"),
		zap.String("host", cfg.Server.Host),
		zap.Int("port", cfg.Server.Port),
		zap.Bool("tls", cfg.Server.TLS))

	// Initialize service registry
	serviceRegistry := registry.NewInMemoryRegistry()

	// Initialize tunnel manager
	tunnelConfig := &tunnel.Config{
		HeartbeatTimeout: cfg.Tunnel.HeartbeatTimeout,
		BufferSize:       cfg.Tunnel.BufferSize,
		MaxMessageSize:   1024 * 1024, // 1MB
		PingInterval:     30 * time.Second,
		PongWait:         60 * time.Second,
	}

	tunnelManager := tunnel.NewTunnelManager(serviceRegistry, logger, tunnelConfig)

	// Setup HTTP router
	router := mux.NewRouter()

	// API routes
	api := router.PathPrefix("/api/v1").Subrouter()
	api.Use(loggingMiddleware(logger))
	api.Use(authenticationMiddleware(cfg.Auth.JWTSecret))

	// WebSocket endpoint
	router.HandleFunc("/ws", tunnelManager.HandleWebSocket)

	// API endpoints
	api.HandleFunc("/services", handleListServices(serviceRegistry)).Methods("GET")
	api.HandleFunc("/services/{id}", handleGetService(serviceRegistry)).Methods("GET")
	api.HandleFunc("/tunnel", handleCreateTunnel(tunnelManager)).Methods("POST")
	api.HandleFunc("/health", handleHealthCheck).Methods("GET")

	// Static files for dashboard
	router.PathPrefix("/").Handler(http.FileServer(http.Dir("./static/")))

	// Start cleanup goroutine
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go cleanupStaleServices(ctx, serviceRegistry, 5*time.Minute, logger)

	// Create HTTP server
	server := &http.Server{
		Addr:         fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port),
		Handler:      router,
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
		IdleTimeout:  cfg.Server.IdleTimeout,
	}

	// Start server with TLS if configured
	var serverErr error
	if cfg.Server.TLS {
		// Load certificates
		cert, err := tls.LoadX509KeyPair(cfg.Server.CertFile, cfg.Server.KeyFile)
		if err != nil {
			logger.Fatal("Failed to load certificates", zap.Error(err))
		}

		tlsConfig := &tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{cert},
			CurvePreferences: []tls.CurveID{
				tls.X25519, tls.CurveP256, tls.CurveP384,
			},
			CipherSuites: []uint16{
				// HTTP/2 REQUIRED ciphers (add these)
				tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
				tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,

				// Strong ciphers
				tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
				tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
				tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
				tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,

				// Fallback for compatibility
				tls.TLS_RSA_WITH_AES_128_GCM_SHA256,
				tls.TLS_RSA_WITH_AES_256_GCM_SHA384,
			},
			PreferServerCipherSuites: true,
			NextProtos:               []string{"h2", "http/1.1"},
		}

		server.TLSConfig = tlsConfig
		logger.Info("Starting server with TLS")

		// Create listener first
		listener, err := net.Listen("tcp", server.Addr)
		if err != nil {
			logger.Fatal("Failed to create listener", zap.Error(err))
		}

		// Wrap with TLS listener
		tlsListener := tls.NewListener(listener, tlsConfig)
		serverErr = server.Serve(tlsListener)
	} else {
		logger.Warn("Starting server without TLS (NOT RECOMMENDED FOR PRODUCTION)")
		serverErr = server.ListenAndServe()
	}

	// Graceful shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-quit
		logger.Info("Shutting down server...")

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		if err := server.Shutdown(ctx); err != nil {
			logger.Error("Server shutdown failed", zap.Error(err))
		}
	}()

	if serverErr != nil && serverErr != http.ErrServerClosed {
		logger.Error("Server failed", zap.Error(serverErr))
		os.Exit(1)
	}

	logger.Info("Server stopped gracefully")
}

func initLogger(level, format string) *zap.Logger {
	var zapConfig zap.Config

	if format == "json" {
		zapConfig = zap.NewProductionConfig()
	} else {
		zapConfig = zap.NewDevelopmentConfig()
	}

	// Set log level
	switch level {
	case "debug":
		zapConfig.Level = zap.NewAtomicLevelAt(zapcore.DebugLevel)
	case "info":
		zapConfig.Level = zap.NewAtomicLevelAt(zapcore.InfoLevel)
	case "warn":
		zapConfig.Level = zap.NewAtomicLevelAt(zapcore.WarnLevel)
	case "error":
		zapConfig.Level = zap.NewAtomicLevelAt(zapcore.ErrorLevel)
	default:
		zapConfig.Level = zap.NewAtomicLevelAt(zapcore.InfoLevel)
	}

	logger, err := zapConfig.Build()
	if err != nil {
		fmt.Printf("Failed to create logger: %v\n", err)
		os.Exit(1)
	}

	return logger
}

func loggingMiddleware(logger *zap.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()

			// Create response writer wrapper to capture status code
			rw := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}

			next.ServeHTTP(rw, r)

			duration := time.Since(start)

			logger.Info("HTTP request",
				zap.String("method", r.Method),
				zap.String("path", r.URL.Path),
				zap.String("remote_addr", r.RemoteAddr),
				zap.Int("status", rw.statusCode),
				zap.Duration("duration", duration),
				zap.String("user_agent", r.UserAgent()))
		})
	}
}

type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

func authenticationMiddleware(jwtSecret string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Skip auth for public endpoints
			if r.URL.Path == "/api/v1/health" ||
				r.URL.Path == "/ws" ||
				strings.HasPrefix(r.URL.Path, "/static/") {
				next.ServeHTTP(w, r)
				return
			}

			token := r.Header.Get("Authorization")
			if token == "" {
				// Try query parameter
				token = r.URL.Query().Get("token")
			}

			// Remove "Bearer " prefix if present
			if strings.HasPrefix(token, "Bearer ") {
				token = strings.TrimPrefix(token, "Bearer ")
			}

			// For now, accept any non-empty token (simplified)
			// In production, validate against JWT secret
			if token == "" {
				http.Error(w, "Authentication required", http.StatusUnauthorized)
				return
			}

			// Store token in context for later use
			ctx := context.WithValue(r.Context(), "auth_token", token)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func handleListServices(registry registry.ServiceRegistry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		services, err := registry.GetAll()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(services)
	}
}

func handleGetService(registry registry.ServiceRegistry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		serviceID := vars["id"]

		service, err := registry.Get(serviceID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(service)
	}
}

func handleCreateTunnel(tunnelManager *tunnel.TunnelManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ServiceID  string `json:"service_id"`
			TargetPort int    `json:"target_port"`
		}

		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}

		if req.ServiceID == "" || req.TargetPort <= 0 {
			http.Error(w, "service_id and target_port are required", http.StatusBadRequest)
			return
		}

		localAddr, err := tunnelManager.CreateTunnel(req.ServiceID, req.TargetPort)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		response := map[string]string{
			"tunnel_id":  fmt.Sprintf("tun-%s-%d", req.ServiceID, req.TargetPort),
			"local_addr": localAddr,
			"status":     "created",
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}
}

func handleHealthCheck(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status": "healthy",
		"time":   time.Now().UTC().Format(time.RFC3339),
	})
}

func cleanupStaleServices(ctx context.Context, registry registry.ServiceRegistry,
	interval time.Duration, logger *zap.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			removed := registry.CleanupStaleServices(2 * interval)
			if len(removed) > 0 {
				logger.Info("Cleaned up stale services",
					zap.Int("count", len(removed)),
					zap.Strings("services", removed))
			}
		}
	}
}
