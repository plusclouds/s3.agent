// Package config handles loading and validation of the agent configuration.
// Configuration is layered: defaults → config file → environment variables.
// Environment variables use the prefix PLUSCLOUDS_AGENT_ and dot-separated
// keys map to underscores (e.g. nats.url → PLUSCLOUDS_AGENT_NATS_URL).
package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/viper"
)

// AgentVersion is set at build time via -ldflags.
const AgentVersion = "1.0.0"

// Config is the top-level configuration structure for the agent.
type Config struct {
	NATS     NATSConfig     `mapstructure:"nats"`
	Agent    AgentConfig    `mapstructure:"agent"`
	S3       S3Config       `mapstructure:"s3"`
	ISO      ISOConfig      `mapstructure:"iso"`
	Log      LogConfig      `mapstructure:"log"`
	Autoheal AutohealConfig `mapstructure:"autoheal"`
}

// Connection type constants for NATSConfig.ConnectionType.
const (
	ConnectionTypeNATS      = "nats"
	ConnectionTypeWebSocket = "websocket"
)

// NATSConfig holds NATS connection settings.
type NATSConfig struct {
	// ConnectionType selects the transport: "nats" (TCP, default) or "websocket".
	// Use "websocket" to connect through firewalls/proxies without TLS certificates.
	ConnectionType string `mapstructure:"connection_type"`
	// URL is the NATS TCP server address, used when connection_type is "nats".
	// Example: nats://nats.plusclouds.com:4222
	URL string `mapstructure:"url"`
	// WebSocketURL is the NATS WebSocket address, used when connection_type is "websocket".
	// Use ws:// for plain WebSocket (no TLS). Example: ws://nats.plusclouds.com:8080
	WebSocketURL string `mapstructure:"websocket_url"`
	// AgentUUID is the fallback VM UUID used as the NATS username when the ISO
	// config drive is not present. The ISO virtual_machine_id always takes precedence.
	// Can also be set via PLUSCLOUDS_AGENT_NATS_AGENT_UUID environment variable.
	AgentUUID string `mapstructure:"agent_uuid"`
	// APIKey is the fallback NATS authentication token when the ISO config drive
	// does not carry an agent_api_key. The ISO value always takes precedence.
	// Can also be set via PLUSCLOUDS_AGENT_NATS_API_KEY environment variable.
	APIKey string `mapstructure:"api_key"`
	// MaxReconnects controls how many times the client retries on disconnect.
	// -1 means unlimited.
	MaxReconnects int `mapstructure:"max_reconnects"`
	// ReconnectWait is the delay between reconnect attempts.
	ReconnectWait time.Duration `mapstructure:"reconnect_wait"`
}

// ActiveURL returns the server URL for the configured connection type.
func (c NATSConfig) ActiveURL() string {
	if c.ConnectionType == ConnectionTypeWebSocket {
		return c.WebSocketURL
	}
	return c.URL
}

// AgentConfig holds agent behaviour settings.
type AgentConfig struct {
	// Type is the agent type reported to the platform (e.g. "storage", "vm").
	Type string `mapstructure:"type"`
	// HeartbeatInterval is how often the agent publishes a heartbeat event.
	HeartbeatInterval time.Duration `mapstructure:"heartbeat_interval"`
	// TelemetryInterval is how often the agent publishes a telemetry event.
	TelemetryInterval time.Duration `mapstructure:"telemetry_interval"`
	// AllowedOperations is the set of operation names the dispatcher will execute.
	// Any operation not listed is rejected without execution.
	AllowedOperations []string `mapstructure:"allowed_operations"`
	// AllowedCommands is the allowlist of binary paths the exec operation may run.
	// Only consulted when "exec" appears in AllowedOperations.
	AllowedCommands []string `mapstructure:"allowed_commands"`
}

// S3Config holds SeaweedFS and related service configuration for the s3d agent.
type S3Config struct {
	// SeaweedFS component API endpoints (all internal, localhost-only).
	MasterURL string `mapstructure:"master_url"` // http://localhost:9333
	VolumeURL string `mapstructure:"volume_url"` // http://localhost:8080
	FilerURL  string `mapstructure:"filer_url"`  // http://localhost:8888
	S3URL     string `mapstructure:"s3_url"`     // http://localhost:8333

	// IAMFile is the path to the SeaweedFS S3 IAM config file.
	IAMFile string `mapstructure:"iam_file"` // /etc/seaweedfs/s3.json
	// WeedS3Service is the systemd unit name reloaded after IAM changes.
	WeedS3Service string `mapstructure:"weed_s3_service"` // weed-s3

	// NginxBlockedKeysFile is the Nginx include file managed for customer blocking.
	NginxBlockedKeysFile string `mapstructure:"nginx_blocked_keys_file"`

	// CapacityWarnPct triggers an immediate s3_health event when crossed.
	CapacityWarnPct float64 `mapstructure:"capacity_warn_pct"` // 80.0
	// CapacityCriticalPct triggers an immediate s3_health event when crossed.
	CapacityCriticalPct float64 `mapstructure:"capacity_critical_pct"` // 90.0
}

// ISOConfig holds ISO/config-drive settings.
type ISOConfig struct {
	// MountPath is the filesystem path where the config drive ISO is mounted.
	MountPath string `mapstructure:"mount_path"`
	// FallbackEnv enables falling back to environment variables when ISO files
	// are absent. Useful for development and testing without a real ISO.
	FallbackEnv bool `mapstructure:"fallback_env"`
}

// LogConfig holds logging settings.
type LogConfig struct {
	// Level is the minimum log level: debug, info, warn, error.
	Level string `mapstructure:"level"`
	// Format controls log output to stdout: json or console.
	Format string `mapstructure:"format"`
	// File is an optional path to write logs to in addition to stdout.
	// Always written in JSON format regardless of the Format setting.
	// Leave empty to disable file logging.
	File string `mapstructure:"file"`
}

// AutohealConfig holds automatic service recovery settings.
type AutohealConfig struct {
	// Enabled activates automatic restart of failed services.
	Enabled bool `mapstructure:"enabled"`
	// RestartDelay is how long to wait before restarting a failed service.
	RestartDelay time.Duration `mapstructure:"restart_delay"`
}

// Load reads the configuration from the given file path, then overlays
// environment variables (prefix PLUSCLOUDS_AGENT_), and finally applies
// built-in defaults for any missing values.
func Load(cfgFile string) (*Config, error) {
	v := viper.New()

	setDefaults(v)

	if cfgFile != "" {
		v.SetConfigFile(cfgFile)
	} else {
		v.SetConfigName("agent")
		v.SetConfigType("yaml")
		v.AddConfigPath("/etc/plusclouds")
		v.AddConfigPath("$HOME/.plusclouds")
		v.AddConfigPath("./configs")
		v.AddConfigPath(".")
	}

	if err := v.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			return nil, fmt.Errorf("reading config file: %w", err)
		}
	}

	v.SetEnvPrefix("PLUSCLOUDS_AGENT")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("unmarshalling config: %w", err)
	}

	return &cfg, nil
}

func setDefaults(v *viper.Viper) {
	v.SetDefault("agent.type", "storage")

	v.SetDefault("s3.master_url", "http://localhost:9333")
	v.SetDefault("s3.volume_url", "http://localhost:8080")
	v.SetDefault("s3.filer_url", "http://localhost:8888")
	v.SetDefault("s3.s3_url", "http://localhost:8333")
	v.SetDefault("s3.iam_file", "/etc/seaweedfs/s3.json")
	v.SetDefault("s3.weed_s3_service", "weed-s3")
	v.SetDefault("s3.nginx_blocked_keys_file", "/etc/nginx/conf.d/s3_blocked_keys.conf")
	v.SetDefault("s3.capacity_warn_pct", 80.0)
	v.SetDefault("s3.capacity_critical_pct", 90.0)

	v.SetDefault("nats.connection_type", ConnectionTypeNATS)
	v.SetDefault("nats.url", "nats://localhost:4222")
	v.SetDefault("nats.websocket_url", "ws://localhost:8080")
	v.SetDefault("nats.agent_uuid", "")
	v.SetDefault("nats.api_key", "")
	v.SetDefault("nats.max_reconnects", -1)
	v.SetDefault("nats.reconnect_wait", 5*time.Second)

	v.SetDefault("agent.heartbeat_interval", 30*time.Second)
	v.SetDefault("agent.telemetry_interval", 30*time.Second)
	v.SetDefault("agent.allowed_operations", []string{
		"agent.allowed_operations",
		"services.list",
		"services.get",
		"services.start",
		"services.stop",
		"services.restart",
		"services.reload",
		"services.enable",
		"services.disable",
		"system.info",
		"system.metrics",
		"system.cpu",
		"system.memory",
		"system.disk",
		"system.network",
		"system.update",
		"telemetry.set_interval",
		"vm.reboot",
		"vm.shutdown",
		"full_sync",
		"bucket_create",
		"bucket_delete",
		"iam_create",
		"iam_delete",
		"reconcile",
		"s3.cluster.status",
		"s3.bucket.stats",
		"s3.iam.list",
		"s3.customer.block",
		"s3.customer.unblock",
		"s3.blocked.list",
	})
	v.SetDefault("agent.allowed_commands", []string{})

	v.SetDefault("iso.mount_path", "/media/plusclouds-config")
	v.SetDefault("iso.fallback_env", true)

	v.SetDefault("log.level", "info")
	v.SetDefault("log.format", "json")
	v.SetDefault("log.file", "")

	v.SetDefault("autoheal.enabled", true)
	v.SetDefault("autoheal.restart_delay", 10*time.Second)
}
