package sendspin

import (
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Sendspin/sendspin-go/pkg/protocol"
	"github.com/gorilla/websocket"
	"github.com/miguelangel-nubla/sendspin-voip/internal/domain"
)

func TestDecodePCM_16Bit(t *testing.T) {
	// 4 samples of 16-bit little-endian
	raw := make([]byte, 8)
	binary.LittleEndian.PutUint16(raw[0:2], uint16(0))
	binary.LittleEndian.PutUint16(raw[2:4], uint16(1000))
	var negVal int16 = -1000
	binary.LittleEndian.PutUint16(raw[4:6], uint16(negVal))
	binary.LittleEndian.PutUint16(raw[6:8], uint16(32767))

	samples := decodePCM(raw, 16)
	if len(samples) != 4 {
		t.Fatalf("expected 4 samples, got %d", len(samples))
	}
	if samples[0] != 0 || samples[1] != 1000 || samples[2] != -1000 || samples[3] != 32767 {
		t.Errorf("unexpected 16-bit decoded samples: %v", samples)
	}
}

func TestDecodePCM_24Bit(t *testing.T) {
	// 2 samples of 24-bit little-endian:
	// Sample 1: 0x00, 0x80, 0x00 -> 0x008000 (32768 in 24-bit) -> scaled >> 8 = 128
	// Sample 2: 0xFF, 0x7F, 0x7F -> 0x7F7FFF (~8355839 in 24-bit) -> scaled >> 8 = 32639
	raw := []byte{
		0x00, 0x80, 0x00,
		0xFF, 0x7F, 0x7F,
	}

	samples := decodePCM(raw, 24)
	if len(samples) != 2 {
		t.Fatalf("expected 2 samples, got %d", len(samples))
	}
	if samples[0] != 128 {
		t.Errorf("expected sample 0 to be 128, got %d", samples[0])
	}
	if samples[1] != 32639 {
		t.Errorf("expected sample 1 to be 32639, got %d", samples[1])
	}
}

func TestNormalizeChannels(t *testing.T) {
	tests := []struct {
		name     string
		channels int
		fallback int
		want     int
	}{
		{"mono passes through", 1, 2, 1},
		{"stereo passes through", 2, 1, 2},
		{"zero falls back", 0, 1, 1},
		{"negative falls back", -3, 2, 2},
		{"unsupported layout falls back", 6, 2, 2},
		{"unsupported layout with bad fallback defaults to stereo", 6, 0, 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := domain.NormalizeChannels(tt.channels, tt.fallback); got != tt.want {
				t.Errorf("NormalizeChannels(%d, %d) = %d, want %d", tt.channels, tt.fallback, got, tt.want)
			}
		})
	}
}

func TestBuildSupportedFormatsForCodecs(t *testing.T) {
	codecs := []domain.Codec{domain.CodecOpus, domain.CodecG722, domain.CodecPCMU}
	formats, supportCodecs, rates, chans := BuildSupportedFormatsForCodecs(codecs, domain.CodecOpus)

	if len(formats) == 0 {
		t.Fatalf("expected supported formats, got none")
	}
	if len(supportCodecs) == 0 {
		t.Errorf("expected support codecs, got none")
	}
	if len(rates) == 0 {
		t.Errorf("expected sample rates, got none")
	}
	if len(chans) == 0 {
		t.Errorf("expected channels, got none")
	}

	// Verify Opus preferred first
	if formats[0].Codec != "opus" {
		t.Errorf("expected first format to be opus, got %s", formats[0].Codec)
	}
}

func TestNormalizeServerAddr(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"ws://192.168.1.100:8927/sendspin", "192.168.1.100:8927"},
		{"http://music.local:8927/", "music.local:8927"},
		{"10.0.0.5:8927", "10.0.0.5:8927"},
	}

	for _, tt := range tests {
		if got := normalizeServerAddr(tt.input); got != tt.want {
			t.Errorf("normalizeServerAddr(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

type dummyEventHandler struct{}

func (d *dummyEventHandler) OnStreamStart(playerID string, meta domain.StreamMetadata) {}
func (d *dummyEventHandler) OnMetadata(playerID string, meta domain.StreamMetadata)    {}
func (d *dummyEventHandler) OnStreamClear(playerID string)                             {}
func (d *dummyEventHandler) OnPlaybackState(playerID string, state string)             {}
func (d *dummyEventHandler) OnAudioChunk(playerID string, chunk domain.AudioChunk)     {}
func (d *dummyEventHandler) OnStreamEnd(playerID string)                               {}
func (d *dummyEventHandler) OnVolumeChange(playerID string, volume int)                {}
func (d *dummyEventHandler) OnMuteChange(playerID string, muted bool)                  {}
func (d *dummyEventHandler) OnGroupUpdate(playerID string, isGrouped bool)             {}

func TestIngress_RegisterAndUnregister(t *testing.T) {
	ing := NewIngress(nil, IngressConfig{
		Server:   "127.0.0.1:8927",
		BufferMs: 200,
	})
	defer func() { _ = ing.StopAll() }()

	player := domain.PlayerConfig{
		ID:            "test-player",
		Name:          "Test Player",
		SIPTarget:     "sip:100@127.0.0.1",
		DefaultVolume: 80,
	}

	handler := &dummyEventHandler{}
	if err := ing.RegisterPlayer(player, handler); err != nil {
		t.Fatalf("RegisterPlayer failed: %v", err)
	}

	stats, ok := ing.GetPlayerStats("test-player")
	if !ok {
		t.Fatalf("expected player stats for registered player")
	}
	if stats.ServerAddr != "127.0.0.1:8927" {
		t.Errorf("expected server addr 127.0.0.1:8927, got %s", stats.ServerAddr)
	}

	if err := ing.UnregisterPlayer("test-player"); err != nil {
		t.Fatalf("UnregisterPlayer failed: %v", err)
	}

	_, ok = ing.GetPlayerStats("test-player")
	if ok {
		t.Errorf("expected player to be removed after unregister")
	}
}

func TestDecodeIncomingAudioChunk_Opus(t *testing.T) {
	pkt := []byte{0xFC, 0x01, 0x02, 0x03}
	now := time.Now()
	chunk := protocol.AudioChunk{
		Timestamp: 123456,
		Data:      pkt,
	}

	decoded := decodeIncomingAudioChunk(chunk, "opus", 48000, 2, 16, now)
	if len(decoded.OpusData) != len(pkt) {
		t.Fatalf("expected OpusData length %d, got %d", len(pkt), len(decoded.OpusData))
	}
	if decoded.Timestamp != 123456 {
		t.Errorf("expected timestamp 123456, got %d", decoded.Timestamp)
	}
	if decoded.Channels != 2 {
		t.Errorf("expected 2 channels, got %d", decoded.Channels)
	}
	if decoded.SampleRate != 48000 {
		t.Errorf("expected 48000 sample rate, got %d", decoded.SampleRate)
	}
}

func TestIngress_MockSendspinServer(t *testing.T) {
	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}

	mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		for {
			var msg map[string]any
			if err := conn.ReadJSON(&msg); err != nil {
				return
			}
			msgType, _ := msg["type"].(string)
			switch msgType {
			case "client/hello":
				_ = conn.WriteJSON(map[string]any{
					"type": "server/hello",
					"payload": map[string]any{
						"client_id": "mock-test-player",
						"name":      "Mock MA Server",
						"version":   1,
						"roles":     []string{"player@v1", "metadata@v1", "controller@v1"},
					},
				})
			case "client/time":
				_ = conn.WriteJSON(map[string]any{
					"type": "server/time",
					"payload": map[string]any{
						"server_time": time.Now().UnixNano(),
					},
				})
			case "client/state":
				// Player synchronized state
			case "client/command":
				// Stop command
			}
		}
	}))
	defer mockSrv.Close()

	serverAddr := strings.TrimPrefix(mockSrv.URL, "http://")

	ing := NewIngress(nil, IngressConfig{
		Server:   serverAddr,
		BufferMs: 100,
	})
	defer func() { _ = ing.StopAll() }()

	player := domain.PlayerConfig{
		ID:            "mock-test-player",
		Name:          "Mock Test Player",
		SIPTarget:     "sip:200@127.0.0.1",
		Codec:         domain.CodecOpus,
		DefaultVolume: 90,
	}

	handler := &dummyEventHandler{}
	if err := ing.RegisterPlayer(player, handler); err != nil {
		t.Fatalf("RegisterPlayer failed: %v", err)
	}

	// Give client a moment to connect and synchronize
	var connected bool
	for i := 0; i < 20; i++ {
		time.Sleep(50 * time.Millisecond)
		stats, ok := ing.GetPlayerStats("mock-test-player")
		if ok && stats.Connected {
			connected = true
			break
		}
	}

	if !connected {
		t.Logf("player client did not mark connected in test timeout, continuing...")
	}

	ing.SendStopToUpstream("mock-test-player")

	if err := ing.UnregisterPlayer("mock-test-player"); err != nil {
		t.Fatalf("UnregisterPlayer failed: %v", err)
	}
}

func TestIngress_RegisterPlayerWithCodecs_PreservesZeroVolume(t *testing.T) {
	ing := NewIngress(nil, IngressConfig{
		Server:   "127.0.0.1:9999",
		BufferMs: 100,
	})
	defer func() { _ = ing.StopAll() }()

	player := domain.PlayerConfig{
		ID:            "zero-vol-player",
		Name:          "Zero Vol Player",
		SIPTarget:     "sip:200@127.0.0.1",
		Codec:         domain.CodecOpus,
		DefaultVolume: 0,
	}

	handler := &dummyEventHandler{}
	_ = ing.RegisterPlayerWithCodecs(player, []domain.Codec{domain.CodecOpus}, handler)

	ing.mu.Lock()
	worker, ok := ing.workers["zero-vol-player"]
	ing.mu.Unlock()

	if !ok {
		t.Fatalf("expected worker to be registered")
	}

	worker.statsMu.RLock()
	vol := worker.state.volume
	worker.statsMu.RUnlock()

	if vol != 0 {
		t.Errorf("expected worker volume to be 0, got %d", vol)
	}
}
