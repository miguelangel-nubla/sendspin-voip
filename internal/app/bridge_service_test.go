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
func (m *mockSIPCaller) RegistrationStatus() SIPStatus {
	return SIPStatus{
		Mode:       "pbx",
		Server:     "127.0.0.1:5060",
		Registered: true,
	}
}
func (m *mockSIPCaller) ProbeTarget(ctx context.Context, targetURI string) ([]domain.Codec, error) {
	return []domain.Codec{domain.CodecOpus, domain.CodecG722, domain.CodecPCMU, domain.CodecPCMA}, nil
}
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
func (m *mockRTPSession) Stats() RTPStats {
	m.mu.Lock()
	defer m.mu.Unlock()
	rStr := ""
	if m.startedAddr != nil {
		rStr = m.startedAddr.String()
	}
	return RTPStats{
		LocalPort:   m.localPort,
		RemoteAddr:  rStr,
		PacketsSent: uint64(m.chunksPushed),
		BytesSent:   uint64(m.chunksPushed * 160),
	}
}
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
func (m *mockRTPSession) ClearBuffer() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.chunksPushed = 0
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
	return m.RegisterPlayerWithCodecs(player, nil, handler)
}

func (m *mockIngress) RegisterPlayerWithCodecs(player domain.PlayerConfig, codecs []domain.Codec, handler PlayerEventHandler) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.registeredCount++
	return nil
}

func (m *mockIngress) UnregisterPlayer(playerID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return nil
}

func (m *mockIngress) StopAll() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopped = true
	return nil
}

func (m *mockIngress) SendPauseToUpstream(playerID string) {}

func (m *mockIngress) GetPlayerStats(playerID string) (IngressPlayerStats, bool) {
	return IngressPlayerStats{
		ServerAddr: "127.0.0.1:8927",
		Connected:  true,
		Codec:      "pcm",
		SampleRate: 16000,
		Channels:   1,
		BitDepth:   16,
	}, true
}

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

func TestBridgeService_SeekAndTrackChange_ReusesCall(t *testing.T) {
	sipCaller := &mockSIPCaller{}
	rtpStreamer := &mockRTPStreamer{}
	ingress := &mockIngress{}
	arbiter := domain.NewTargetArbiter(domain.ConflictPolicyPreemptAnnouncements)

	bridge := NewBridgeService(
		nil,
		BridgeConfig{
			DefaultBufferMode: domain.BufferModeLive,
			PickupBufferMs:    500,
			DrainDelayMs:      50,
			IdleHangupDelayMs: 2000,
		},
		arbiter,
		sipCaller,
		rtpStreamer,
		ingress,
	)

	playerConfigs := []domain.PlayerConfig{
		{
			ID:         "player-music",
			SIPTarget:  "sip:101@192.168.1.50",
			Codec:      domain.CodecG722,
			BufferMode: domain.BufferModeLive,
		},
	}
	_ = bridge.RegisterPlayers(playerConfigs)

	// 1. Initial track playback
	bridge.OnStreamStart("player-music", domain.StreamMetadata{Title: "Song 1"})
	time.Sleep(30 * time.Millisecond)

	sipCaller.mu.Lock()
	if sipCaller.dialCount != 1 {
		t.Fatalf("expected 1 dial initially, got %d", sipCaller.dialCount)
	}
	sipCaller.mu.Unlock()

	// Push some audio
	chunk := domain.AudioChunk{
		Samples:    make([]int32, 960),
		SampleRate: 48000,
		Channels:   1,
		BitDepth:   16,
	}
	bridge.OnAudioChunk("player-music", chunk)

	// 2. User performs seek in Music Assistant
	// MA sends StreamClear, StreamEnd, then StreamStart for the new position
	bridge.OnStreamClear("player-music")
	bridge.OnStreamEnd("player-music")
	time.Sleep(20 * time.Millisecond)

	// StreamStart for seeked position
	bridge.OnStreamStart("player-music", domain.StreamMetadata{Title: "Song 1 (Seeked)"})
	time.Sleep(30 * time.Millisecond)

	// Verify SIP call was NOT hung up and NO second INVITE was sent
	sipCaller.mu.Lock()
	if sipCaller.dialCount != 1 {
		t.Errorf("expected still 1 dial after seek, got %d", sipCaller.dialCount)
	}
	sipCaller.mu.Unlock()

	// Verify RTP streamer was not asked to create a second session
	rtpStreamer.mu.Lock()
	if len(rtpStreamer.sessions) != 1 {
		t.Errorf("expected 1 RTP session, got %d", len(rtpStreamer.sessions))
	}
	rtpStreamer.mu.Unlock()

	// Push audio after seek
	bridge.OnAudioChunk("player-music", chunk)

	// Verify active call is still intact
	if _, ok := bridge.activeCalls.Load("player-music"); !ok {
		t.Errorf("expected player-music to remain in activeCalls")
	}

	bridge.Shutdown()
}

func TestBridgeService_IdleHangupDelay_Expires(t *testing.T) {
	sipCaller := &mockSIPCaller{}
	rtpStreamer := &mockRTPStreamer{}
	ingress := &mockIngress{}
	arbiter := domain.NewTargetArbiter(domain.ConflictPolicyPreemptAnnouncements)

	bridge := NewBridgeService(
		nil,
		BridgeConfig{
			DefaultBufferMode: domain.BufferModeLive,
			PickupBufferMs:    500,
			DrainDelayMs:      20,
			IdleHangupDelayMs: 60, // 60ms linger
		},
		arbiter,
		sipCaller,
		rtpStreamer,
		ingress,
	)

	playerConfigs := []domain.PlayerConfig{
		{
			ID:         "player-music",
			SIPTarget:  "sip:101@192.168.1.50",
			Codec:      domain.CodecG722,
			BufferMode: domain.BufferModeLive,
		},
	}
	_ = bridge.RegisterPlayers(playerConfigs)

	bridge.OnStreamStart("player-music", domain.StreamMetadata{Title: "Song 1"})
	time.Sleep(30 * time.Millisecond)

	// Stream ends
	bridge.OnStreamEnd("player-music")

	// Verify call is still active during linger
	time.Sleep(20 * time.Millisecond)
	if _, ok := bridge.activeCalls.Load("player-music"); !ok {
		t.Errorf("expected call to linger during IdleHangupDelay")
	}

	// Wait for linger timer (60ms) to expire + drain
	time.Sleep(100 * time.Millisecond)
	if _, ok := bridge.activeCalls.Load("player-music"); ok {
		t.Errorf("expected call to terminate after IdleHangupDelay expired")
	}

	bridge.Shutdown()
}

func TestBridgeService_TrackChange_StateStopped_ReusesCall(t *testing.T) {
	sipCaller := &mockSIPCaller{}
	rtpStreamer := &mockRTPStreamer{}
	ingress := &mockIngress{}
	arbiter := domain.NewTargetArbiter(domain.ConflictPolicyPreemptAnnouncements)

	bridge := NewBridgeService(
		nil,
		BridgeConfig{
			DefaultBufferMode: domain.BufferModeLive,
			PickupBufferMs:    500,
			DrainDelayMs:      50,
			IdleHangupDelayMs: 2000,
		},
		arbiter,
		sipCaller,
		rtpStreamer,
		ingress,
	)

	playerConfigs := []domain.PlayerConfig{
		{
			ID:         "player-music",
			SIPTarget:  "sip:101@192.168.1.50",
			Codec:      domain.CodecG722,
			BufferMode: domain.BufferModeLive,
		},
	}
	_ = bridge.RegisterPlayers(playerConfigs)

	// 1. Initial track playback
	bridge.OnStreamStart("player-music", domain.StreamMetadata{Title: "Track 1"})
	bridge.OnPlaybackState("player-music", "playing")
	time.Sleep(30 * time.Millisecond)

	// 2. Track 1 finishes -> MA sends stream end and stopped state before starting Track 2
	bridge.OnStreamEnd("player-music")
	bridge.OnPlaybackState("player-music", "stopped")
	time.Sleep(30 * time.Millisecond)

	// Call should still be alive in linger state
	if _, ok := bridge.activeCalls.Load("player-music"); !ok {
		t.Fatalf("expected call to stay alive during track transition linger")
	}

	// 3. Track 2 begins
	bridge.OnStreamStart("player-music", domain.StreamMetadata{Title: "Track 2"})
	bridge.OnPlaybackState("player-music", "playing")
	time.Sleep(30 * time.Millisecond)

	sipCaller.mu.Lock()
	if sipCaller.dialCount != 1 {
		t.Errorf("expected only 1 SIP dial across track change, got %d", sipCaller.dialCount)
	}
	sipCaller.mu.Unlock()

	bridge.Shutdown()
}

func TestBridgeService_GetStreamsDebugInfo(t *testing.T) {
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
			IdleHangupDelayMs: 2000,
		},
		arbiter,
		sipCaller,
		rtpStreamer,
		ingress,
	)

	playerConfigs := []domain.PlayerConfig{
		{
			ID:            "player-test",
			Name:          "Test Desk Phone",
			SIPTarget:     "sip:8003@asterisk.local",
			Codec:         domain.CodecG722,
			BufferMode:    domain.BufferModeLive,
			Priority:      10,
			DefaultVolume: 100,
		},
	}
	_ = bridge.RegisterPlayers(playerConfigs)

	// Check idle state debug info
	streams := bridge.GetStreamsDebugInfo()
	if len(streams) != 1 {
		t.Fatalf("expected 1 stream, got %d", len(streams))
	}
	st, ok := streams["player-test"]
	if !ok {
		t.Fatalf("expected stream for player-test")
	}
	if st.ID != "player-test" || st.Name != "Test Desk Phone" {
		t.Errorf("unexpected stream details: %+v", st)
	}
	if len(st.Producers) == 0 || len(st.Consumers) == 0 {
		t.Errorf("expected producers and consumers in debug info")
	}

	// Start playback
	bridge.OnStreamStart("player-test", domain.StreamMetadata{Title: "Song 1", Artist: "Artist 1"})
	bridge.OnPlaybackState("player-test", "playing")
	time.Sleep(30 * time.Millisecond)

	stActive, ok := bridge.GetStreamDebugInfo("player-test")
	if !ok {
		t.Fatalf("expected player-test in active streams")
	}
	if stActive.State != "active" && stActive.State != "playing" && stActive.State != "dialing" {
		t.Errorf("expected active/playing state, got %s", stActive.State)
	}

	bridge.Shutdown()
}
