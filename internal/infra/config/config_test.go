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
