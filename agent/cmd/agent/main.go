package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"tunnel-agent/internal/config"
	"tunnel-agent/internal/tunnel"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func main() {
	// Load configuration
	cfgPath := "configs/agent.yaml"
	if len(os.Args) > 1 {
		cfgPath = os.Args[1]
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Printf("Failed to load config: %v\n", err)
		os.Exit(1)
	}

	// Initialize logger
	logger := initLogger(cfg.Logging.Level, cfg.Logging.Format)
	defer logger.Sync()

	logger.Info("Starting Tunnel Agent",
		zap.String("agent_id", cfg.Agent.ID),
		zap.String("agent_name", cfg.Agent.Name),
		zap.String("control_server", cfg.ControlServer.URL),
		zap.Int("service_count", len(cfg.Services)))

	// Create tunnel client
	client := tunnel.NewClient(cfg, logger)

	// Connect to control server
	if err := client.Connect(); err != nil {
		logger.Error("Failed to connect to control server", zap.Error(err))
		os.Exit(1)
	}

	// Wait for termination signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Graceful shutdown
	<-sigChan
	logger.Info("Shutting down agent...")

	client.Disconnect()

	logger.Info("Agent stopped gracefully")
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
