package app

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/miguelangel-nubla/sendspin-voip/internal/domain"
)

// BridgeConfig contains global operational parameters for the bridge.
type BridgeConfig struct {
	DefaultBufferMode domain.BufferMode
	PickupBufferMs    int
	DrainDelayMs      int
	ConflictPolicy    domain.ConflictPolicy
}

type activeCallState struct {
	session    *domain.CallSession
	dialog     SIPDialog
	rtpSession RTPSession
	mu         sync.Mutex
	buffer     []domain.AudioChunk
	answered   bool
	done       chan struct{}
}

// BridgeService coordinates Sendspin player ingress with SIP call signaling and RTP media streaming.
type BridgeService struct {
	logger      *slog.Logger
	config      BridgeConfig
	arbiter     *domain.TargetArbiter
	sipCaller   SIPCallerPort
	rtpStreamer RTPStreamerPort
	ingress     PlayerIngressPort

	playersMu   sync.RWMutex
	players     map[string]*domain.Player
	activeCalls sync.Map // keyed by playerID -> *activeCallState
}

// NewBridgeService creates a new bridge service.
func NewBridgeService(
	logger *slog.Logger,
	config BridgeConfig,
	arbiter *domain.TargetArbiter,
	sipCaller SIPCallerPort,
	rtpStreamer RTPStreamerPort,
	ingress PlayerIngressPort,
) *BridgeService {
	if logger == nil {
		logger = slog.Default()
	}
	if config.PickupBufferMs <= 0 {
		config.PickupBufferMs = 2000
	}
	if config.DrainDelayMs <= 0 {
		config.DrainDelayMs = 500
	}
	if config.DefaultBufferMode == "" {
		config.DefaultBufferMode = domain.BufferModeAnnouncement
	}

	return &BridgeService{
		logger:      logger,
		config:      config,
		arbiter:     arbiter,
		sipCaller:   sipCaller,
		rtpStreamer: rtpStreamer,
		ingress:     ingress,
		players:     make(map[string]*domain.Player),
	}
}

// RegisterPlayers registers all configured players into the bridge and ingress adapter.
func (s *BridgeService) RegisterPlayers(configs []domain.PlayerConfig) error {
	s.playersMu.Lock()
	defer s.playersMu.Unlock()

	for _, cfg := range configs {
		p, err := domain.NewPlayer(cfg)
		if err != nil {
			return fmt.Errorf("invalid player config %s: %w", cfg.ID, err)
		}
		s.players[p.Config.ID] = p

		if err := s.ingress.RegisterPlayer(p.Config, s); err != nil {
			return fmt.Errorf("failed to register player %s on ingress: %w", p.Config.ID, err)
		}
		s.logger.Info("Registered virtual Sendspin player",
			"player_id", p.Config.ID,
			"name", p.Config.Name,
			"target", p.Config.SIPTarget,
			"codec", p.Config.Codec,
			"buffer_mode", p.Config.BufferMode,
		)
	}
	return nil
}

// OnStreamStart handles stream initiation from Music Assistant.
func (s *BridgeService) OnStreamStart(playerID string, meta domain.StreamMetadata) {
	s.playersMu.Lock()
	player, exists := s.players[playerID]
	if !exists {
		s.playersMu.Unlock()
		s.logger.Warn("Stream start for unknown player", "player_id", playerID)
		return
	}

	player.IsPlaying = true
	playerCfg := player.Config
	s.playersMu.Unlock()

	effectiveMode := playerCfg.BufferMode
	if effectiveMode == "" {
		effectiveMode = s.config.DefaultBufferMode
	}

	drainDelay := time.Duration(s.config.DrainDelayMs) * time.Millisecond
	sessionID := uuid.New().String()
	session := domain.NewCallSession(
		sessionID,
		playerID,
		playerCfg.SIPTarget,
		playerCfg.Priority,
		meta,
		effectiveMode,
		drainDelay,
	)

	s.logger.Info("Stream started on player",
		"player_id", playerID,
		"target", playerCfg.SIPTarget,
		"title", meta.Title,
		"artist", meta.Artist,
		"buffer_mode", effectiveMode,
		"priority", playerCfg.Priority,
	)

	// 2. Concurrency & Preemption check
	preempted, err := s.arbiter.RequestTarget(session)
	if err != nil {
		s.logger.Warn("Cannot start SIP call: target conflict",
			"player_id", playerID,
			"target", playerCfg.SIPTarget,
			"err", err,
		)
		return
	}

	if preempted != nil {
		s.logger.Info("Preempting active session on target",
			"preempted_player", preempted.PlayerID,
			"new_player", playerID,
			"target", session.SIPTarget,
		)
		// Synchronously terminate the preempted call to prevent INVITE collision / 486 Busy
		s.terminatePlayerCallSync(preempted.PlayerID, false, 500*time.Millisecond)
	}

	// 3. Allocate local RTP socket
	rtpSess, err := s.rtpStreamer.CreateSession(playerCfg.Codec)
	if err != nil {
		s.logger.Error("Failed to create RTP session", "err", err)
		s.arbiter.ReleaseTarget(session)
		return
	}

	callState := &activeCallState{
		session:    session,
		rtpSession: rtpSess,
		buffer:     make([]domain.AudioChunk, 0, 64),
		done:       make(chan struct{}),
	}
	s.activeCalls.Store(playerID, callState)

	// 4. Dial SIP target in background
	go s.dialAndRunCall(playerCfg, callState)
}

func (s *BridgeService) dialAndRunCall(cfg domain.PlayerConfig, call *activeCallState) {
	session := call.session
	session.SetState(domain.StateDialing)

	dialCtx, cancel := context.WithTimeout(session.Ctx, 15*time.Second)
	defer cancel()

	s.logger.Debug("Dialing SIP target", "player_id", cfg.ID, "target", cfg.SIPTarget, "rtp_port", call.rtpSession.LocalPort())

	dialog, err := s.sipCaller.Dial(dialCtx, cfg, call.rtpSession.LocalPort())
	if err != nil {
		s.logger.Error("SIP call failed", "player_id", cfg.ID, "target", cfg.SIPTarget, "err", err)
		s.terminatePlayerCall(cfg.ID, true)
		return
	}

	call.dialog = dialog
	session.SetState(domain.StateActive)

	remoteRTP := dialog.RemoteRTPAddr()
	if remoteRTP == nil {
		s.logger.Error("No remote RTP address returned from SDP", "player_id", cfg.ID)
		s.terminatePlayerCall(cfg.ID, true)
		return
	}

	activeCodec := cfg.Codec
	if negotiated := dialog.RemoteCodec(); negotiated != "" && negotiated != cfg.Codec {
		s.logger.Info("Switching RTP codec to SDP answer negotiated codec",
			"player_id", cfg.ID,
			"offered", cfg.Codec,
			"negotiated", negotiated,
		)
		call.rtpSession.SetCodec(negotiated)
		activeCodec = negotiated
	}

	if err := call.rtpSession.StartTransmission(remoteRTP); err != nil {
		s.logger.Error("Failed to start RTP transmission", "player_id", cfg.ID, "err", err)
		s.terminatePlayerCall(cfg.ID, true)
		return
	}

	s.logger.Info("SIP call connected & streaming RTP",
		"player_id", cfg.ID,
		"remote_rtp", remoteRTP.String(),
		"codec", activeCodec,
	)

	// Flush buffered audio if in Announcement mode
	call.mu.Lock()
	call.answered = true
	var preBuffer []domain.AudioChunk
	if session.EffectiveMod == domain.BufferModeAnnouncement {
		preBuffer = make([]domain.AudioChunk, len(call.buffer))
		copy(preBuffer, call.buffer)
	}
	call.buffer = nil
	call.mu.Unlock()

	s.playersMu.RLock()
	volume := 100
	if p, ok := s.players[cfg.ID]; ok {
		volume = p.Volume
		if p.IsMuted {
			volume = 0
		}
	}
	s.playersMu.RUnlock()

	for _, chunk := range preBuffer {
		if err := call.rtpSession.PushAudio(chunk, volume); err != nil {
			s.logger.Debug("Error pushing buffered audio chunk", "err", err)
		}
	}

	// Listen for remote hangup (phone physically hung up)
	select {
	case <-dialog.Done():
		s.logger.Info("Remote SIP phone hung up", "player_id", cfg.ID, "target", cfg.SIPTarget)
		s.terminatePlayerCall(cfg.ID, true)
	case <-session.Ctx.Done():
		// Local termination
	}
}

// OnAudioChunk receives decoded audio chunks from the Sendspin player client.
func (s *BridgeService) OnAudioChunk(playerID string, chunk domain.AudioChunk) {
	val, ok := s.activeCalls.Load(playerID)
	if !ok {
		return
	}
	call := val.(*activeCallState)

	s.playersMu.RLock()
	volume := 100
	if p, ok := s.players[playerID]; ok {
		volume = p.Volume
		if p.IsMuted {
			volume = 0
		}
	}
	s.playersMu.RUnlock()

	call.mu.Lock()
	answered := call.answered
	if !answered {
		if call.session.EffectiveMod == domain.BufferModeAnnouncement {
			// Limit ring buffer to max configured capacity
			maxCapacity := (s.config.PickupBufferMs / 20) + 10 // ~50 chunks per sec
			if len(call.buffer) < maxCapacity {
				call.buffer = append(call.buffer, chunk)
			}
		}
		call.mu.Unlock()
		return
	}
	call.mu.Unlock()

	if err := call.rtpSession.PushAudio(chunk, volume); err != nil {
		s.logger.Debug("Failed to push audio to RTP session", "player_id", playerID, "err", err)
	}
}

// OnStreamEnd handles stream completion from Music Assistant.
func (s *BridgeService) OnStreamEnd(playerID string) {
	s.playersMu.Lock()
	if p, ok := s.players[playerID]; ok {
		p.IsPlaying = false
	}
	s.playersMu.Unlock()

	s.logger.Info("Stream ended from Music Assistant", "player_id", playerID)
	s.terminatePlayerCall(playerID, true)
}

// OnVolumeChange updates player volume gain.
func (s *BridgeService) OnVolumeChange(playerID string, volume int, muted bool) {
	s.playersMu.Lock()
	if p, ok := s.players[playerID]; ok {
		p.Volume = volume
		p.IsMuted = muted
		s.logger.Debug("Player volume changed", "player_id", playerID, "volume", volume, "muted", muted)
	}
	s.playersMu.Unlock()
}

// OnGroupUpdate tracks multi-room sync group membership.
func (s *BridgeService) OnGroupUpdate(playerID string, isGrouped bool) {
	s.playersMu.Lock()
	if p, ok := s.players[playerID]; ok {
		p.IsGrouped = isGrouped
		s.logger.Info("Player group status updated", "player_id", playerID, "is_grouped", isGrouped)
	}
	s.playersMu.Unlock()
}

func (s *BridgeService) terminatePlayerCallSync(playerID string, releaseArbiter bool, timeout time.Duration) {
	val, ok := s.activeCalls.LoadAndDelete(playerID)
	if !ok {
		return
	}
	call := val.(*activeCallState)

	call.session.SetState(domain.StateTerminating)
	call.session.Close()

	if call.dialog != nil {
		byeCtx, cancel := context.WithTimeout(context.Background(), timeout)
		_ = call.dialog.Bye(byeCtx)
		cancel()
	}

	if call.rtpSession != nil {
		_ = call.rtpSession.DrainAndClose(0)
	}

	if releaseArbiter {
		s.arbiter.ReleaseTarget(call.session)
	}
	call.session.SetState(domain.StateTerminated)
}

func (s *BridgeService) terminatePlayerCall(playerID string, releaseArbiter bool) {
	val, ok := s.activeCalls.LoadAndDelete(playerID)
	if !ok {
		return
	}
	call := val.(*activeCallState)

	call.session.SetState(domain.StateTerminating)
	call.session.Close()

	go func() {
		// 1. Drain RTP jitter buffer
		if call.rtpSession != nil {
			_ = call.rtpSession.DrainAndClose(call.session.DrainDelay)
		}

		// 2. Send SIP BYE
		if call.dialog != nil {
			byeCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = call.dialog.Bye(byeCtx)
		}

		// 3. Release arbiter target
		if releaseArbiter {
			s.arbiter.ReleaseTarget(call.session)
		}
		call.session.SetState(domain.StateTerminated)
	}()
}

// Shutdown cleanly stops all active calls and players.
func (s *BridgeService) Shutdown() {
	s.logger.Info("Shutting down sendspin-voip bridge...")
	s.activeCalls.Range(func(key, value any) bool {
		playerID := key.(string)
		s.terminatePlayerCall(playerID, true)
		return true
	})
	_ = s.ingress.StopAll()
	_ = s.sipCaller.Stop()
}
