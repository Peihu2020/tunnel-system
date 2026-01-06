package config

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/viper"
)

type ServicePort struct {
	Port        int    `mapstructure:"port"`
	Protocol    string `mapstructure:"protocol"`
	Name        string `mapstructure:"name"`
	Description string `mapstructure:"description"`
	Internal    bool   `mapstructure:"internal"`
}

type Config struct {
	Agent struct {
		ID                string        `mapstructure:"id"`
		Name              string        `mapstructure:"name"`
		Group             string        `mapstructure:"group"`
		Tags              []string      `mapstructure:"tags"`
		HeartbeatInterval time.Duration `mapstructure:"heartbeat_interval"`
		ReconnectDelay    time.Duration `mapstructure:"reconnect_delay"`
		MaxRetries        int           `mapstructure:"max_retries"`
	} `mapstructure:"agent"`

	ControlServer struct {
		URL         string        `mapstructure:"url"`
		Token       string        `mapstructure:"token"`
		InsecureTLS bool          `mapstructure:"insecure_tls"`
		Timeout     time.Duration `mapstructure:"timeout"`
	} `mapstructure:"control_server"`

	Services []ServicePort `mapstructure:"services"`

	Monitoring struct {
		Enabled         bool          `mapstructure:"enabled"`
		MetricsPort     int           `mapstructure:"metrics_port"`
		CollectInterval time.Duration `mapstructure:"collect_interval"`
	} `mapstructure:"monitoring"`

	Logging struct {
		Level  string `mapstructure:"level"`
		Format string `mapstructure:"format"`
		Output string `mapstructure:"output"`
	} `mapstructure:"logging"`
}

func Load(configPath string) (*Config, error) {
	viper.SetConfigFile(configPath)
	viper.SetConfigType("yaml")

	// Set defaults
	viper.SetDefault("agent.heartbeat_interval", "30s")
	viper.SetDefault("agent.reconnect_delay", "5s")
	viper.SetDefault("agent.max_retries", 10)
	viper.SetDefault("control_server.timeout", "30s")
	viper.SetDefault("monitoring.enabled", false)
	viper.SetDefault("monitoring.metrics_port", 9090)
	viper.SetDefault("logging.level", "info")

	if err := viper.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("failed to read config: %w", err)
	}

	var config Config
	if err := viper.Unmarshal(&config); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %w", err)
	}

	// Auto-generate agent ID if not provided
	if config.Agent.ID == "" {
		hostname, _ := os.Hostname()
		config.Agent.ID = fmt.Sprintf("%s-%d", hostname, time.Now().Unix())
	}

	return &config, nil
}
