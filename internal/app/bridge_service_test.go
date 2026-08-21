package app

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/miguelangel-nubla/sendspin-voip/internal/domain"
)

// MockSIPDialog implements SIPDialog
type mockSIPDialog struct {
	remoteRTP *net.UDPAddr
	codec     domain.Codec
	doneChan  chan struct{}
	mu        sync.Mutex
	byeCalled bool
}

func (m *mockSIPDialog) RemoteRTPAddr() *net.UDPAddr {
	return m.remoteRTP
}

func (m *mockSIPDialog) RemoteCodec() domain.Codec {
	return m.codec
}

func (m *mockSIPDialog) Bye(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.byeCalled = true
	select {
	case <-m.doneChan:
	default:
		close(m.doneChan)
	}
	return nil
}

func (m *mockSIPDialog) Done() <-chan struct{} {
	return m.doneChan
}

// MockSIPCaller implements SIPCallerPort
type mockSIPCaller struct {
	mu         sync.Mutex
	dialCount  int
	lastPlayer domain.PlayerConfig
	dialog     *mockSIPDialog
	dialogs    []*mockSIPDialog
}

func (m *mockSIPCaller) Start(ctx context.Context) error { return nil }
func (m *mockSIPCaller) Stop() error                    { return nil }
func (m *mockSIPCaller) LocalIP() string                 { return "127.0.0.1" }
func (m *mockSIPCaller) Dial(ctx context.Context, player domain.PlayerConfig, localRTPPort int) (SIPDialog, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dialCount++
	m.lastPlayer = player
	dlg := &mockSIPDialog{
		remoteRTP: &net.UDPAddr{IP: net.ParseIP("192.168.1.100"), Port: 16384},
		codec:     player.Codec,
		doneChan:  make(chan struct{}),
	}
	m.dialog = dlg
	m.dialogs = append(m.dialogs, dlg)
	return dlg, nil
}

// MockRTPSession implements RTPSession
type mockRTPSession struct {
	mu           sync.Mutex
	localPort    int
	startedAddr  *net.UDPAddr
	chunksPushed int
	drained      bool
}

func (m *mockRTPSession) LocalPort() int { return m.localPort }
func (m *mockRTPSession) StartTransmission(remoteAddr *net.UDPAddr) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.startedAddr = remoteAddr
	return nil
}
func (m *mockRTPSession) SetCodec(codec domain.Codec) {}
func (m *mockRTPSession) PushAudio(chunk domain.AudioChunk, volumePercent int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.chunksPushed++
	return nil
}
func (m *mockRTPSession) DrainAndClose(drainDelay time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.drained = true
	return nil
}

// MockRTPStreamer implements RTPStreamerPort
type mockRTPStreamer struct {
	mu       sync.Mutex
	sessions []*mockRTPSession
}

func (m *mockRTPStreamer) CreateSession(codec domain.Codec) (RTPSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sess := &mockRTPSession{localPort: 10002 + len(m.sessions)*2}
	m.sessions = append(m.sessions, sess)
	return sess, nil
}

// MockIngress implements PlayerIngressPort
type mockIngress struct {
	mu              sync.Mutex
	registeredCount int
	stopped         bool
}

func (m *mockIngress) RegisterPlayer(player domain.PlayerConfig, handler PlayerEventHandler) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.registeredCount++
	return nil
}

func (m *mockIngress) StopAll() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopped = true
	return nil
}

func (m *mockIngress) SendPauseToUpstream(playerID string) {}

func TestBridgeService_LifecycleAndPlayback(t *testing.T) {
	sipCaller := &mockSIPCaller{}
	rtpStreamer := &mockRTPStreamer{}
	ingress := &mockIngress{}
	arbiter := domain.NewTargetArbiter(domain.ConflictPolicyPreemptAnnouncements)

	bridge := NewBridgeService(
		nil,
		BridgeConfig{
			DefaultBufferMode: domain.BufferModeAnnouncement,
			PickupBufferMs:    500,
			DrainDelayMs:      50,
			ConflictPolicy:    domain.ConflictPolicyPreemptAnnouncements,
		},
		arbiter,
		sipCaller,
		rtpStreamer,
		ingress,
	)

	// 1. Register players
	playerConfigs := []domain.PlayerConfig{
		{
			ID:            "player-desk",
			Name:          "Desk Phone",
			SIPTarget:     "sip:101@192.168.1.50",
			Codec:         domain.CodecG722,
			BufferMode:    domain.BufferModeAnnouncement,
			Priority:      10,
			DefaultVolume: 100,
		},
	}

	if err := bridge.RegisterPlayers(playerConfigs); err != nil {
		t.Fatalf("RegisterPlayers failed: %v", err)
	}
	if ingress.registeredCount != 1 {
		t.Errorf("expected 1 registered player on ingress, got %d", ingress.registeredCount)
	}

	// 2. Start stream
	meta := domain.StreamMetadata{
		Title:     "Doorbell",
		MediaType: "announcement",
	}
	bridge.OnStreamStart("player-desk", meta)

	// Wait briefly for background dial goroutine
	time.Sleep(50 * time.Millisecond)

	sipCaller.mu.Lock()
	dials := sipCaller.dialCount
	sipCaller.mu.Unlock()

	if dials != 1 {
		t.Fatalf("expected 1 SIP dial, got %d", dials)
	}

	// 3. Push audio chunks
	chunk := domain.AudioChunk{
		Samples:    make([]int32, 960),
		SampleRate: 48000,
		Channels:   1,
		BitDepth:   16,
	}
	bridge.OnAudioChunk("player-desk", chunk)

	// 4. Volume change
	bridge.OnVolumeChange("player-desk", 75, false)

	// 5. Group update
	bridge.OnGroupUpdate("player-desk", true)

	// 6. End stream
	bridge.OnStreamEnd("player-desk")

	// Wait for drain and cleanup
	time.Sleep(100 * time.Millisecond)

	// 7. Shutdown
	bridge.Shutdown()
	if !ingress.stopped {
		t.Errorf("expected ingress.stopped to be true after Shutdown")
	}
}

func TestBridgeService_Preemption(t *testing.T) {
	sipCaller := &mockSIPCaller{}
	rtpStreamer := &mockRTPStreamer{}
	ingress := &mockIngress{}
	arbiter := domain.NewTargetArbiter(domain.ConflictPolicyPreemptAnnouncements)

	bridge := NewBridgeService(
		nil,
		BridgeConfig{
			DefaultBufferMode: domain.BufferModeAnnouncement,
			PickupBufferMs:    500,
			DrainDelayMs:      50,
			ConflictPolicy:    domain.ConflictPolicyPreemptAnnouncements,
		},
		arbiter,
		sipCaller,
		rtpStreamer,
		ingress,
	)

	playerConfigs := []domain.PlayerConfig{
		{
			ID:         "player-music",
			Name:       "Desk Music",
			SIPTarget:  "sip:101@192.168.1.50",
			Codec:      domain.CodecG722,
			BufferMode: domain.BufferModeLive,
			Priority:   1,
		},
		{
			ID:         "player-alert",
			Name:       "Desk Alert",
			SIPTarget:  "sip:101@192.168.1.50",
			Codec:      domain.CodecG722,
			BufferMode: domain.BufferModeAnnouncement,
			Priority:   10,
		},
	}
	_ = bridge.RegisterPlayers(playerConfigs)

	// 1. Start music playback
	bridge.OnStreamStart("player-music", domain.StreamMetadata{Title: "Song A", MediaType: "track"})
	time.Sleep(30 * time.Millisecond)

	// 2. Start high-priority announcement on same target
	bridge.OnStreamStart("player-alert", domain.StreamMetadata{Title: "Doorbell", MediaType: "announcement"})
	time.Sleep(50 * time.Millisecond)

	// Music session should be preempted and alert should be active
	if _, ok := bridge.activeCalls.Load("player-music"); ok {
		t.Errorf("expected player-music to be preempted from activeCalls")
	}
	if _, ok := bridge.activeCalls.Load("player-alert"); !ok {
		t.Errorf("expected player-alert to be active")
	}

	bridge.Shutdown()
}

func TestBridgeService_RemoteHangup(t *testing.T) {
	sipCaller := &mockSIPCaller{}
	rtpStreamer := &mockRTPStreamer{}
	ingress := &mockIngress{}
	arbiter := domain.NewTargetArbiter(domain.ConflictPolicyPreemptAnnouncements)

	bridge := NewBridgeService(
		nil,
		BridgeConfig{
			DefaultBufferMode: domain.BufferModeAnnouncement,
			PickupBufferMs:    500,
			DrainDelayMs:      50,
		},
		arbiter,
		sipCaller,
		rtpStreamer,
		ingress,
	)

	playerConfigs := []domain.PlayerConfig{
		{
			ID:        "player-desk",
			SIPTarget: "sip:101@192.168.1.50",
			Codec:     domain.CodecG722,
		},
	}
	_ = bridge.RegisterPlayers(playerConfigs)

	bridge.OnStreamStart("player-desk", domain.StreamMetadata{Title: "Test"})
	time.Sleep(30 * time.Millisecond)

	// Simulate remote phone sending BYE (closing dialog.Done)
	sipCaller.mu.Lock()
	dialog := sipCaller.dialog
	sipCaller.mu.Unlock()

	if dialog != nil {
		close(dialog.doneChan)
	}

	time.Sleep(50 * time.Millisecond)

	// Call should be terminated from activeCalls
	if _, ok := bridge.activeCalls.Load("player-desk"); ok {
		t.Errorf("expected active call to terminate after remote hangup")
	}

	bridge.Shutdown()
}
