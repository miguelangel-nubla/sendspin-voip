package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/miguelangel-nubla/sendspin-voip/internal/app"
	"github.com/miguelangel-nubla/sendspin-voip/internal/domain"
	"gopkg.in/yaml.v3"
)

// AppConfig represents the root application configuration.
type AppConfig struct {
	LogLevel string         `yaml:"log_level" json:"log_level"`
	SIP      SIPConfig      `yaml:"sip" json:"sip"`
	Sendspin SendspinConfig `yaml:"sendspin" json:"sendspin"`
	Bridge   BridgeConfig   `yaml:"bridge" json:"bridge"`
	Players  []PlayerConfig `yaml:"players" json:"players"`
}

// SIPConfig defines SIP connectivity options.
type SIPConfig struct {
	Mode                   string                  `yaml:"mode" json:"mode"` // "pbx" or "direct"
	Server                 string                  `yaml:"server" json:"server"`
	Username               string                  `yaml:"username" json:"username"`
	Password               string                  `yaml:"password" json:"password"`
	Domain                 string                  `yaml:"domain" json:"domain"`
	Transport              string                  `yaml:"transport" json:"transport"` // "udp" or "tcp"
	LocalIP                string                  `yaml:"local_ip" json:"local_ip"`   // Advertised IP in SDP
	LocalSIPPort           int                     `yaml:"local_sip_port" json:"local_sip_port"`
	RTPPortMin             int                     `yaml:"rtp_port_min" json:"rtp_port_min"`
	RTPPortMax             int                     `yaml:"rtp_port_max" json:"rtp_port_max"`
	AutoAnswerPreset       domain.AutoAnswerPreset `yaml:"auto_answer_preset" json:"auto_answer_preset"`
	CustomAutoAnswerHeader string                  `yaml:"custom_auto_answer_header" json:"custom_auto_answer_header"`
}

// SendspinConfig defines Sendspin discovery and connection settings.
type SendspinConfig struct {
	Server   string `yaml:"server" json:"server"` // "auto" for mDNS, or ws://...
	BufferMs int    `yaml:"buffer_ms" json:"buffer_ms"`
}

// BridgeConfig defines bridge runtime parameters.
type BridgeConfig struct {
	DefaultBufferMode domain.BufferMode     `yaml:"default_buffer_mode" json:"default_buffer_mode"`
	PickupBufferMs    int                   `yaml:"pickup_buffer_ms" json:"pickup_buffer_ms"`
	DrainDelayMs      int                   `yaml:"drain_delay_ms" json:"drain_delay_ms"`
	IdleHangupDelayMs int                   `yaml:"idle_hangup_delay_ms" json:"idle_hangup_delay_ms"`
	ConflictPolicy    domain.ConflictPolicy `yaml:"target_conflict_policy" json:"target_conflict_policy"`
}

// PlayerConfig represents player definition in YAML/JSON.
type PlayerConfig struct {
	ID                     string                  `yaml:"id" json:"id"`
	Name                   string                  `yaml:"name" json:"name"`
	SIPTarget              string                  `yaml:"sip_target" json:"sip_target"`
	Codec                  string                  `yaml:"codec" json:"codec"`
	BufferMode             string                  `yaml:"buffer_mode" json:"buffer_mode"`
	AutoAnswer             domain.AutoAnswerPreset `yaml:"auto_answer" json:"auto_answer"`
	CustomAutoAnswerHeader string                  `yaml:"custom_auto_answer_header" json:"custom_auto_answer_header"`
	Priority               int                     `yaml:"priority" json:"priority"`
	DefaultVolume          int                     `yaml:"default_volume" json:"default_volume"`
}

// Load loads configuration from YAML file path, Home Assistant /data/options.json, or environment variables.
func Load(explicitPath string) (*AppConfig, error) {
	cfg := DefaultConfig()

	// 1. Check for explicit YAML file or standard paths
	var foundPath string
	searchPaths := []string{
		explicitPath,
		os.Getenv("CONFIG_PATH"),
		"/data/options.json", // Home Assistant Add-on options path
		"config.yaml",
		"config.yml",
		"/etc/sendspin-voip/config.yaml",
	}

	for _, p := range searchPaths {
		if p != "" {
			if _, err := os.Stat(p); err == nil {
				foundPath = p
				break
			}
		}
	}

	if foundPath != "" {
		data, err := os.ReadFile(foundPath)
		if err != nil {
			return nil, fmt.Errorf("failed to read config file %s: %w", foundPath, err)
		}

		if strings.HasSuffix(foundPath, ".json") {
			if err := json.Unmarshal(data, cfg); err != nil {
				return nil, fmt.Errorf("failed to parse JSON config %s: %w", foundPath, err)
			}
		} else {
			if err := yaml.Unmarshal(data, cfg); err != nil {
				return nil, fmt.Errorf("failed to parse YAML config %s: %w", foundPath, err)
			}
		}
	}

	// 2. Override with Environment Variables
	applyEnvOverrides(cfg)

	// 3. Validation & defaults normalization
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

// DefaultConfig returns reasonable default configuration values.
func DefaultConfig() *AppConfig {
	return &AppConfig{
		LogLevel: "info",
		SIP: SIPConfig{
			Mode:             "pbx",
			Transport:        "udp",
			LocalSIPPort:     5060,
			RTPPortMin:       10000,
			RTPPortMax:       20000,
			AutoAnswerPreset: domain.AutoAnswerDefault,
		},
		Sendspin: SendspinConfig{
			Server:   "auto",
			BufferMs: 500,
		},
		Bridge: BridgeConfig{
			DefaultBufferMode: domain.BufferModeAnnouncement,
			PickupBufferMs:    2000,
			DrainDelayMs:      500,
			IdleHangupDelayMs: 5000,
			ConflictPolicy:    domain.ConflictPolicyPreemptAnnouncements,
		},
		Players: []PlayerConfig{},
	}
}

func applyEnvOverrides(cfg *AppConfig) {
	if v := os.Getenv("LOG_LEVEL"); v != "" {
		cfg.LogLevel = v
	}
	if v := os.Getenv("SIP_SERVER"); v != "" {
		cfg.SIP.Server = v
	}
	if v := os.Getenv("SIP_USERNAME"); v != "" {
		cfg.SIP.Username = v
	}
	if v := os.Getenv("SIP_PASSWORD"); v != "" {
		cfg.SIP.Password = v
	}
	if v := os.Getenv("SIP_DOMAIN"); v != "" {
		cfg.SIP.Domain = v
	}
	if v := os.Getenv("SIP_LOCAL_IP"); v != "" {
		cfg.SIP.LocalIP = v
	}
	if v := os.Getenv("SENDSPIN_SERVER"); v != "" {
		cfg.Sendspin.Server = v
	}
	if v := os.Getenv("DEFAULT_BUFFER_MODE"); v != "" {
		cfg.Bridge.DefaultBufferMode = domain.BufferMode(v)
	}
	if v := os.Getenv("PICKUP_BUFFER_MS"); v != "" {
		if val, err := strconv.Atoi(v); err == nil {
			cfg.Bridge.PickupBufferMs = val
		}
	}
	if v := os.Getenv("IDLE_HANGUP_DELAY_MS"); v != "" {
		if val, err := strconv.Atoi(v); err == nil {
			cfg.Bridge.IdleHangupDelayMs = val
		}
	}
}

// Validate checks configuration sanity.
func (c *AppConfig) Validate() error {
	if len(c.Players) == 0 {
		return fmt.Errorf("at least one player must be configured in 'players'")
	}

	seenIDs := make(map[string]bool)
	for i, p := range c.Players {
		if strings.TrimSpace(p.ID) == "" {
			return fmt.Errorf("players[%d].id cannot be empty", i)
		}
		if seenIDs[p.ID] {
			return fmt.Errorf("duplicate player id %q configured", p.ID)
		}
		seenIDs[p.ID] = true

		if strings.TrimSpace(p.SIPTarget) == "" {
			return fmt.Errorf("players[%d] (%s) has no sip_target", i, p.ID)
		}
	}

	return nil
}

// ToDomainPlayerConfigs transforms the raw config into domain player configs.
func (c *AppConfig) ToDomainPlayerConfigs() ([]domain.PlayerConfig, error) {
	result := make([]domain.PlayerConfig, len(c.Players))
	for i, p := range c.Players {
		codec, err := domain.ParseCodec(p.Codec)
		if err != nil && p.Codec != "" {
			return nil, fmt.Errorf("player %s: %w", p.ID, err)
		}
		if codec == "" {
			codec = domain.CodecG722
		}

		bMode, err := domain.ParseBufferMode(p.BufferMode)
		if err != nil {
			return nil, fmt.Errorf("player %s: %w", p.ID, err)
		}

		autoAnswer := p.AutoAnswer
		if autoAnswer == "" {
			autoAnswer = c.SIP.AutoAnswerPreset
			if autoAnswer == "" {
				autoAnswer = domain.AutoAnswerDefault
			}
		}

		vol := p.DefaultVolume
		if vol <= 0 || vol > 100 {
			vol = 100
		}

		name := p.Name
		if name == "" {
			name = p.ID
		}

		customHeader := p.CustomAutoAnswerHeader
		if customHeader == "" {
			customHeader = c.SIP.CustomAutoAnswerHeader
		}

		result[i] = domain.PlayerConfig{
			ID:                     p.ID,
			Name:                   name,
			SIPTarget:              p.SIPTarget,
			Codec:                  codec,
			BufferMode:             bMode,
			AutoAnswer:             autoAnswer,
			CustomAutoAnswerHeader: customHeader,
			Priority:               p.Priority,
			DefaultVolume:          vol,
		}
	}
	return result, nil
}

// ToBridgeConfig converts to app.BridgeConfig.
func (c *AppConfig) ToBridgeConfig() app.BridgeConfig {
	return app.BridgeConfig{
		DefaultBufferMode: c.Bridge.DefaultBufferMode,
		PickupBufferMs:    c.Bridge.PickupBufferMs,
		DrainDelayMs:      c.Bridge.DrainDelayMs,
		IdleHangupDelayMs: c.Bridge.IdleHangupDelayMs,
		ConflictPolicy:    c.Bridge.ConflictPolicy,
	}
}

// EnsureDir creates parent directory if not existing.
func EnsureDir(path string) error {
	dir := filepath.Dir(path)
	return os.MkdirAll(dir, 0755)
}
