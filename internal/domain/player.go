package domain

import (
	"fmt"
	"strings"
)

// BufferMode defines how audio chunks are buffered and scheduled during SIP call setup.
type BufferMode string

const (
	// BufferModeAnnouncement buffers all audio during SIP handshake and plays from sample 0 (zero speech loss).
	BufferModeAnnouncement BufferMode = "announcement"
	// BufferModeLive discards pre-connect audio to immediately lock into live multi-room clock synchronization.
	BufferModeLive BufferMode = "live"
)

// ParseBufferMode normalizes and validates a buffer mode.
func ParseBufferMode(s string) (BufferMode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "announcement", "fifo", "buffered", "":
		return BufferModeAnnouncement, nil
	case "live", "sync", "realtime":
		return BufferModeLive, nil
	default:
		return "", fmt.Errorf("invalid buffer_mode %q (allowed: announcement, live)", s)
	}
}

// AutoAnswerPreset defines standard auto-answer headers for various VoIP hardware.
type AutoAnswerPreset string

const (
	AutoAnswerDefault     AutoAnswerPreset = "default"     // Alert-Info: <http://example.com>;info=alert-autoanswer
	AutoAnswerIntercom    AutoAnswerPreset = "intercom"    // Alert-Info: Intercom / Ring Answer
	AutoAnswerYealink     AutoAnswerPreset = "yealink"     // Alert-Info: info=alert-autoanswer;delay=0
	AutoAnswerGrandstream AutoAnswerPreset = "grandstream" // Alert-Info: Ring Answer
	AutoAnswerSnom        AutoAnswerPreset = "snom"        // Alert-Info: <sip:domain>;info=alert-autoanswer;delay=0
	AutoAnswerCallInfo    AutoAnswerPreset = "call_info"   // Call-Info: <sip:...>;answer-after=0
	AutoAnswerPAutoAnswer AutoAnswerPreset = "p_auto"      // P-Auto-Answer: true
	AutoAnswerNone        AutoAnswerPreset = "none"        // No auto-answer header (phone rings normally)
	AutoAnswerCustom      AutoAnswerPreset = "custom"      // Custom header string
)

// ParseAutoAnswerPreset normalizes and validates an auto-answer preset.
// An empty value selects the default preset. An unrecognised preset matches no
// case in buildAutoAnswerHeaders, so the INVITE goes out with no auto-answer
// header at all and the phone simply rings — validate it at startup instead of
// silently degrading.
func ParseAutoAnswerPreset(s string) (AutoAnswerPreset, error) {
	switch p := AutoAnswerPreset(strings.ToLower(strings.TrimSpace(s))); p {
	case "":
		return AutoAnswerDefault, nil
	case AutoAnswerDefault, AutoAnswerIntercom, AutoAnswerYealink, AutoAnswerGrandstream,
		AutoAnswerSnom, AutoAnswerCallInfo, AutoAnswerPAutoAnswer, AutoAnswerNone, AutoAnswerCustom:
		return p, nil
	default:
		return "", fmt.Errorf("invalid auto_answer preset %q (allowed: default, intercom, yealink, grandstream, snom, call_info, p_auto, none, custom)", s)
	}
}

// PlayerConfig defines the configuration for a single virtual Sendspin player.
type PlayerConfig struct {
	ID                     string           `yaml:"id" json:"id"`
	Name                   string           `yaml:"name" json:"name"`
	SIPTarget              string           `yaml:"sip_target" json:"sip_target"`
	Codec                  Codec            `yaml:"codec" json:"codec"`
	BufferMode             BufferMode       `yaml:"buffer_mode" json:"buffer_mode"`
	AutoAnswer             AutoAnswerPreset `yaml:"auto_answer" json:"auto_answer"`
	CustomAutoAnswerHeader string           `yaml:"custom_auto_answer_header" json:"custom_auto_answer_header"`
	Priority               int              `yaml:"priority" json:"priority"` // Higher priority preempts lower
	DefaultVolume          int              `yaml:"default_volume" json:"default_volume"`
}

// Player represents the state of a registered virtual player.
type Player struct {
	Config     PlayerConfig
	IsGrouped  bool
	Volume     int // 0-100
	IsMuted    bool
	IsPlaying  bool
	CurrentURI string
}

// NewPlayer creates a new player instance from configuration.
func NewPlayer(cfg PlayerConfig) (*Player, error) {
	if cfg.ID == "" {
		return nil, fmt.Errorf("player id is required")
	}
	if cfg.Name == "" {
		cfg.Name = cfg.ID
	}
	if cfg.SIPTarget == "" {
		return nil, fmt.Errorf("player %q requires a sip_target", cfg.ID)
	}
	cfg.SIPTarget = NormalizeSIPTarget(cfg.SIPTarget)
	if cfg.SIPTarget == "" || cfg.SIPTarget == "sip:" {
		return nil, fmt.Errorf("player %q requires a valid sip_target", cfg.ID)
	}
	// Empty Codec means "auto": use downstream discovery order (no forced preference).
	if cfg.BufferMode == "" {
		cfg.BufferMode = BufferModeAnnouncement
	}
	if cfg.AutoAnswer == "" {
		cfg.AutoAnswer = AutoAnswerDefault
	}
	if cfg.DefaultVolume <= 0 || cfg.DefaultVolume > 100 {
		cfg.DefaultVolume = 100
	}

	return &Player{
		Config:  cfg,
		Volume:  cfg.DefaultVolume,
		IsMuted: false,
	}, nil
}
