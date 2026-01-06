package config

import (
	"fmt"
	"time"

	"github.com/spf13/viper"
)

type Config struct {
	Server struct {
		Host         string        `mapstructure:"host"`
		Port         int           `mapstructure:"port"`
		TLS          bool          `mapstructure:"tls"`
		CertFile     string        `mapstructure:"cert_file"`
		KeyFile      string        `mapstructure:"key_file"`
		ReadTimeout  time.Duration `mapstructure:"read_timeout"`
		WriteTimeout time.Duration `mapstructure:"write_timeout"`
		IdleTimeout  time.Duration `mapstructure:"idle_timeout"`
	} `mapstructure:"server"`

	Auth struct {
		JWTSecret       string        `mapstructure:"jwt_secret"`
		TokenExpiry     time.Duration `mapstructure:"token_expiry"`
		ServiceTokenTTL time.Duration `mapstructure:"service_token_ttl"`
	} `mapstructure:"auth"`

	Tunnel struct {
		HeartbeatInterval time.Duration `mapstructure:"heartbeat_interval"`
		HeartbeatTimeout  time.Duration `mapstructure:"heartbeat_timeout"`
		MaxConnections    int           `mapstructure:"max_connections"`
		BufferSize        int           `mapstructure:"buffer_size"`
	} `mapstructure:"tunnel"`

	Database struct {
		Type     string `mapstructure:"type"` // "memory", "redis", "postgres"
		DSN      string `mapstructure:"dsn"`
		Services string `mapstructure:"services_file"`
	} `mapstructure:"database"`

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
	viper.SetDefault("server.host", "0.0.0.0")
	viper.SetDefault("server.port", 8443)
	viper.SetDefault("server.tls", true)
	viper.SetDefault("auth.token_expiry", "24h")
	viper.SetDefault("tunnel.heartbeat_interval", "30s")
	viper.SetDefault("tunnel.buffer_size", 4096)
	viper.SetDefault("logging.level", "info")

	if err := viper.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("failed to read config: %w", err)
	}

	var config Config
	if err := viper.Unmarshal(&config); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %w", err)
	}

	return &config, nil
}
