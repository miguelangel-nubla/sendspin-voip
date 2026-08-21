package config

import (
	"os"
	"testing"

	"github.com/miguelangel-nubla/sendspin-voip/internal/domain"
)

func TestConfig_LoadAndValidate(t *testing.T) {
	yamlContent := `
log_level: debug

sip:
  server: "192.168.1.50:5060"
  username: "sendspin"
  password: "password"
  domain: "192.168.1.50"
  transport: "udp"
  auto_answer_preset: "intercom"

sendspin:
  server: "ws://192.168.1.10:8095"
  buffer_ms: 600

bridge:
  default_buffer_mode: "announcement"
  pickup_buffer_ms: 2500
  drain_delay_ms: 400
  target_conflict_policy: "preempt_for_announcements"

players:
  - id: "office_phone_announcements"
    name: "Office Desk (Announcements)"
    sip_target: "sip:101@192.168.1.50"
    codec: "g722"
    buffer_mode: "announcement"
    priority: 10

  - id: "office_phone_music"
    name: "Office Desk (Music)"
    sip_target: "sip:101@192.168.1.50"
    codec: "g722"
    buffer_mode: "live"
    priority: 1
`
	tmpFile, err := os.CreateTemp("", "sendspin-config-*.yaml")
	if err != nil {
		t.Fatalf("failed to create temp config: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.Write([]byte(yamlContent)); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}
	_ = tmpFile.Close()

	cfg, err := Load(tmpFile.Name())
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if cfg.LogLevel != "debug" {
		t.Errorf("expected debug log level, got %s", cfg.LogLevel)
	}
	if len(cfg.Players) != 2 {
		t.Fatalf("expected 2 players, got %d", len(cfg.Players))
	}

	domainPlayers, err := cfg.ToDomainPlayerConfigs()
	if err != nil {
		t.Fatalf("ToDomainPlayerConfigs failed: %v", err)
	}

	if domainPlayers[0].Codec != domain.CodecG722 {
		t.Errorf("expected g722 codec, got %s", domainPlayers[0].Codec)
	}
	if domainPlayers[0].BufferMode != domain.BufferModeAnnouncement {
		t.Errorf("expected announcement buffer mode, got %s", domainPlayers[0].BufferMode)
	}
	if domainPlayers[1].BufferMode != domain.BufferModeLive {
		t.Errorf("expected live buffer mode, got %s", domainPlayers[1].BufferMode)
	}
}

// TestValidate_RejectsUnknownEnums covers the enum fields that are consumed by
// switch statements with no default branch. An unrecognised value used to be
// accepted at startup and then quietly change behaviour: an unknown conflict
// policy makes the arbiter reject every call to a busy target, an unknown
// buffer mode makes the bridge discard pre-answer audio (clipping the start of
// every announcement), and an unknown auto-answer preset drops the
// auto-answer header so the phone just rings.
func TestValidate_RejectsUnknownEnums(t *testing.T) {
	base := func() *AppConfig {
		cfg := DefaultConfig()
		cfg.Players = []PlayerConfig{{ID: "p1", SIPTarget: "sip:101@192.168.1.50"}}
		return cfg
	}

	tests := []struct {
		name   string
		mutate func(*AppConfig)
	}{
		{"unknown conflict policy", func(c *AppConfig) { c.Bridge.ConflictPolicy = "preempt-always" }},
		{"unknown buffer mode", func(c *AppConfig) { c.Bridge.DefaultBufferMode = "anouncement" }},
		{"unknown auto answer preset", func(c *AppConfig) { c.SIP.AutoAnswerPreset = "yealnk" }},
		{"unknown transport", func(c *AppConfig) { c.SIP.Transport = "sctp" }},
		{"rtp range inverted", func(c *AppConfig) { c.SIP.RTPPortMin, c.SIP.RTPPortMax = 20000, 10000 }},
		{"sip port out of range", func(c *AppConfig) { c.SIP.LocalSIPPort = 70000 }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := base()
			tt.mutate(cfg)
			if err := cfg.Validate(); err == nil {
				t.Error("expected Validate to reject the configuration, got nil error")
			}
		})
	}
}

func TestValidate_NormalizesAndDefaultsStateFile(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Players = []PlayerConfig{{ID: "p1", SIPTarget: "sip:101@192.168.1.50"}}
	cfg.SIP.Transport = "UDP"
	cfg.Bridge.ConflictPolicy = "BUSY"

	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate failed: %v", err)
	}

	if cfg.SIP.Transport != "udp" {
		t.Errorf("expected transport normalized to udp, got %q", cfg.SIP.Transport)
	}
	if cfg.Bridge.ConflictPolicy != domain.ConflictPolicyBusy {
		t.Errorf("expected conflict policy normalized to busy, got %q", cfg.Bridge.ConflictPolicy)
	}
	if cfg.StateFile == "" {
		t.Error("expected a default state file path to be filled in")
	}
}

func TestToDomainPlayerConfigs_RejectsUnknownCodec(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Players = []PlayerConfig{{ID: "p1", SIPTarget: "sip:101@192.168.1.50", Codec: "g729"}}

	if _, err := cfg.ToDomainPlayerConfigs(); err == nil {
		t.Error("expected an unsupported codec to be rejected")
	}
}
