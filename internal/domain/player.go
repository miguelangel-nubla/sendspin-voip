package domain

import (
	"cmp"
	"fmt"
	"strings"
)

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
	AutoAnswer             AutoAnswerPreset `yaml:"auto_answer" json:"auto_answer"`
	CustomAutoAnswerHeader string           `yaml:"custom_auto_answer_header" json:"custom_auto_answer_header"`
	Priority               int              `yaml:"priority" json:"priority"` // Higher priority preempts lower
	DefaultVolume          int              `yaml:"default_volume" json:"default_volume"`
}

// Player represents the state of a registered virtual player.
type Player struct {
	Config    PlayerConfig
	IsGrouped bool
	Volume    int // 0-100
	IsMuted   bool
	IsPlaying bool
}

// ClampVolume restricts a volume percentage to the valid range [0, 100].
func ClampVolume(v int) int {
	return min(max(v, 0), 100)
}

// FormatVolumeGain formats a volume percentage (and mute status) into a descriptive string with dB equivalent.
func FormatVolumeGain(volumePercent int, muted bool) string {
	if muted || volumePercent <= 0 {
		return "volume mute"
	}
	if volumePercent >= 100 {
		return "volume 100% (0 dB)"
	}
	db := (float64(volumePercent)/100.0)*60.0 - 60.0
	return fmt.Sprintf("volume %d%% (%.1f dB)", volumePercent, db)
}

// EffectiveVolume returns 0 if muted, otherwise the current volume percentage.
func (p *Player) EffectiveVolume() int {
	if p.IsMuted {
		return 0
	}
	return p.Volume
}

// SetVolume updates the player's volume clamped to [0, 100].
func (p *Player) SetVolume(v int) {
	p.Volume = ClampVolume(v)
}

// NewPlayer creates a new player instance from configuration.
func NewPlayer(cfg PlayerConfig) (*Player, error) {
	if cfg.ID == "" {
		return nil, fmt.Errorf("player id is required")
	}
	cfg.Name = cmp.Or(cfg.Name, cfg.ID)
	if cfg.SIPTarget == "" {
		return nil, fmt.Errorf("player %q requires a sip_target", cfg.ID)
	}
	cfg.SIPTarget = NormalizeSIPTarget(cfg.SIPTarget)
	if cfg.SIPTarget == "" || cfg.SIPTarget == "sip:" {
		return nil, fmt.Errorf("player %q requires a valid sip_target", cfg.ID)
	}
	// Empty Codec means "auto": use downstream discovery order (no forced preference).
	cfg.AutoAnswer = cmp.Or(cfg.AutoAnswer, AutoAnswerDefault)
	if cfg.DefaultVolume <= 0 || cfg.DefaultVolume > 100 {
		cfg.DefaultVolume = 100
	}

	return &Player{
		Config:  cfg,
		Volume:  cfg.DefaultVolume,
		IsMuted: false,
	}, nil
}
