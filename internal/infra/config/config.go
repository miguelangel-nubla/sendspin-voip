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
	LogLevel string `yaml:"log_level" json:"log_level"`
	// StateFile is where per-player volume/mute state is persisted. Empty means
	// "pick a sensible default" — see DefaultStateFilePath.
	StateFile string         `yaml:"state_file" json:"state_file"`
	HTTP      HTTPConfig     `yaml:"http" json:"http"`
	SIP       SIPConfig      `yaml:"sip" json:"sip"`
	Sendspin  SendspinConfig   `yaml:"sendspin" json:"sendspin"`
	Bridge    app.BridgeConfig `yaml:"bridge" json:"bridge"`
	Players   []PlayerConfig   `yaml:"players" json:"players"`
}

// haDataDir is the persistent volume Home Assistant mounts into every add-on.
const haDataDir = "/data"

// DefaultStateFilePath returns where player state should live when the config
// does not say. Inside a Home Assistant add-on the working directory is not
// persisted across restarts or upgrades, but /data is — so prefer it when
// present. Otherwise fall back to the working directory.
func DefaultStateFilePath() string {
	if info, err := os.Stat(haDataDir); err == nil && info.IsDir() {
		return filepath.Join(haDataDir, "sendspin-voip-state.json")
	}
	return "sendspin-voip-state.json"
}

// HTTPConfig defines the HTTP server options for web UI and streams debug info.
type HTTPConfig struct {
	Listen      string `yaml:"listen" json:"listen"`             // e.g. ":8080"
	APIToken    string `yaml:"api_token" json:"api_token"`       // If set, required via Authorization: Bearer or ?token=
	EnablePprof bool   `yaml:"enable_pprof" json:"enable_pprof"` // Expose /debug/pprof (default false)
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

// BridgeConfig is an alias to app.BridgeConfig for convenience.
type BridgeConfig = app.BridgeConfig

// PlayerConfig represents player definition in YAML/JSON.
type PlayerConfig struct {
	ID                     string                  `yaml:"id" json:"id"`
	Name                   string                  `yaml:"name" json:"name"`
	SIPTarget              string                  `yaml:"sip_target" json:"sip_target"`
	Codec                  string                  `yaml:"codec" json:"codec"`
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
		HTTP: HTTPConfig{
			Listen:      ":8080",
			EnablePprof: false,
		},
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
			DrainDelayMs:      500,
			IdleHangupDelayMs: 5000,
			ConflictPolicy:    domain.ConflictPolicyPreemptHigher,
		},
		Players: []PlayerConfig{},
	}
}

func applyEnvOverrides(cfg *AppConfig) {
	if v := os.Getenv("LOG_LEVEL"); v != "" {
		cfg.LogLevel = v
	}
	if v := os.Getenv("HTTP_LISTEN"); v != "" {
		cfg.HTTP.Listen = v
	} else if v := os.Getenv("PORT"); v != "" {
		if !strings.HasPrefix(v, ":") {
			v = ":" + v
		}
		cfg.HTTP.Listen = v
	}
	if v := os.Getenv("HTTP_API_TOKEN"); v != "" {
		cfg.HTTP.APIToken = v
	}
	if v := os.Getenv("STATE_FILE"); v != "" {
		cfg.StateFile = v
	}
	if v := os.Getenv("HTTP_ENABLE_PPROF"); v != "" {
		if val, err := strconv.ParseBool(v); err == nil {
			cfg.HTTP.EnablePprof = val
		}
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
	if v := os.Getenv("TARGET_CONFLICT_POLICY"); v != "" {
		cfg.Bridge.ConflictPolicy = domain.ConflictPolicy(strings.ToLower(strings.TrimSpace(v)))
	}
	if v := os.Getenv("DRAIN_DELAY_MS"); v != "" {
		if val, err := strconv.Atoi(v); err == nil {
			cfg.Bridge.DrainDelayMs = val
		}
	}
	if v := os.Getenv("SIP_LOCAL_SIP_PORT"); v != "" {
		if val, err := strconv.Atoi(v); err == nil {
			cfg.SIP.LocalSIPPort = val
		}
	}
	if v := os.Getenv("SIP_TRANSPORT"); v != "" {
		cfg.SIP.Transport = v
	}
	if v := os.Getenv("IDLE_HANGUP_DELAY_MS"); v != "" {
		if val, err := strconv.Atoi(v); err == nil {
			cfg.Bridge.IdleHangupDelayMs = val
		}
	}
}

// Validate checks configuration sanity and normalizes enum-valued fields.
func (c *AppConfig) Validate() error {
	if c.StateFile == "" {
		c.StateFile = DefaultStateFilePath()
	}

	if c.SIP.Transport == "" {
		c.SIP.Transport = "udp"
	}
	switch strings.ToLower(strings.TrimSpace(c.SIP.Transport)) {
	case "udp", "tcp":
		c.SIP.Transport = strings.ToLower(strings.TrimSpace(c.SIP.Transport))
	default:
		return fmt.Errorf("invalid sip.transport %q (allowed: udp, tcp)", c.SIP.Transport)
	}

	if c.SIP.LocalSIPPort <= 0 || c.SIP.LocalSIPPort > 65535 {
		return fmt.Errorf("sip.local_sip_port must be between 1 and 65535, got %d", c.SIP.LocalSIPPort)
	}
	if c.SIP.RTPPortMin <= 0 || c.SIP.RTPPortMin > 65535 {
		return fmt.Errorf("sip.rtp_port_min must be between 1 and 65535, got %d", c.SIP.RTPPortMin)
	}
	if c.SIP.RTPPortMax <= c.SIP.RTPPortMin || c.SIP.RTPPortMax > 65535 {
		return fmt.Errorf("sip.rtp_port_max (%d) must be greater than sip.rtp_port_min (%d) and at most 65535",
			c.SIP.RTPPortMax, c.SIP.RTPPortMin)
	}

	preset, err := domain.ParseAutoAnswerPreset(string(c.SIP.AutoAnswerPreset))
	if err != nil {
		return fmt.Errorf("sip.auto_answer_preset: %w", err)
	}
	c.SIP.AutoAnswerPreset = preset

	policy, err := domain.ParseConflictPolicy(string(c.Bridge.ConflictPolicy))
	if err != nil {
		return fmt.Errorf("bridge.%w", err)
	}
	c.Bridge.ConflictPolicy = policy

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
		// Empty codec = auto (discovery order); do not force a default.
		codec, err := domain.ParseCodec(p.Codec)
		if err != nil {
			return nil, fmt.Errorf("player %s: %w", p.ID, err)
		}

		autoAnswer := c.SIP.AutoAnswerPreset
		if p.AutoAnswer != "" {
			autoAnswer, err = domain.ParseAutoAnswerPreset(string(p.AutoAnswer))
			if err != nil {
				return nil, fmt.Errorf("player %s: %w", p.ID, err)
			}
		}
		if autoAnswer == "" {
			autoAnswer = domain.AutoAnswerDefault
		}

		vol := p.DefaultVolume
		if vol <= 0 {
			vol = 100
		} else {
			vol = domain.ClampVolume(vol)
		}

		name := p.Name
		if name == "" {
			name = p.ID
		}

		result[i] = domain.PlayerConfig{
			ID:                     p.ID,
			Name:                   name,
			SIPTarget:              p.SIPTarget,
			Codec:                  codec,
			AutoAnswer:             autoAnswer,
			CustomAutoAnswerHeader: p.CustomAutoAnswerHeader,
			Priority:               p.Priority,
			DefaultVolume:          vol,
		}
	}
	return result, nil
}

// ToBridgeConfig returns the app.BridgeConfig.
func (c *AppConfig) ToBridgeConfig() app.BridgeConfig {
	return c.Bridge
}
