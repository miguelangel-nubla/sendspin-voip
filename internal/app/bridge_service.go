package app

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
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
	IdleHangupDelayMs int
	ConflictPolicy    domain.ConflictPolicy
}

type activeCallState struct {
	session     *domain.CallSession
	dialog      SIPDialog
	rtpSession  RTPSession
	mu          sync.Mutex
	buffer      []domain.AudioChunk
	answered    bool
	done        chan struct{}
	lingerTimer *time.Timer
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
	if config.IdleHangupDelayMs < 0 {
		config.IdleHangupDelayMs = 0
	} else if config.IdleHangupDelayMs == 0 {
		config.IdleHangupDelayMs = 5000
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

// RegisterPlayers registers configured players and starts dynamic downstream discovery.
func (s *BridgeService) RegisterPlayers(configs []domain.PlayerConfig) error {
	s.playersMu.Lock()
	for _, cfg := range configs {
		p, err := domain.NewPlayer(cfg)
		if err != nil {
			s.playersMu.Unlock()
			return fmt.Errorf("invalid player config %s: %w", cfg.ID, err)
		}
		s.players[p.Config.ID] = p
	}
	s.playersMu.Unlock()

	// Perform initial probe & publish synchronously so players are available immediately
	for _, cfg := range configs {
		s.probeAndSyncPlayer(cfg)
		go s.runPlayerDiscoveryLoop(cfg)
	}

	return nil
}

func (s *BridgeService) runPlayerDiscoveryLoop(cfg domain.PlayerConfig) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		s.probeAndSyncPlayer(cfg)
	}
}

func (s *BridgeService) probeAndSyncPlayer(cfg domain.PlayerConfig) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	codecs, err := s.sipCaller.ProbeTarget(ctx, cfg.SIPTarget)
	if err != nil {
		s.logger.Debug("Downstream SIP target probe note", "player_id", cfg.ID, "target", cfg.SIPTarget, "err", err)
		// If probe fails (e.g. PBX starting up or strict firewall), still register with fallback codecs so player is not blocked
		codecs = domain.PrioritizeCodecs(cfg.Codec, nil)
	} else {
		s.logger.Info("Discovered downstream SIP target capabilities",
			"player_id", cfg.ID,
			"target", cfg.SIPTarget,
			"codecs", codecs,
		)
	}

	if err := s.ingress.RegisterPlayerWithCodecs(cfg, codecs, s); err != nil {
		s.logger.Warn("Failed to register player with discovered codecs", "player_id", cfg.ID, "err", err)
	} else {
		s.logger.Info("Registered virtual Sendspin player with dynamic capabilities",
			"player_id", cfg.ID,
			"name", cfg.Name,
			"target", cfg.SIPTarget,
			"codecs", codecs,
		)
	}
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

	// 1. Check if there is already an active or lingering call for this player (e.g. seek, next track)
	if val, ok := s.activeCalls.Load(playerID); ok {
		call := val.(*activeCallState)
		call.mu.Lock()
		state := call.session.GetState()
		if state == domain.StateActive || state == domain.StateDialing {
			if call.lingerTimer != nil {
				call.lingerTimer.Stop()
				call.lingerTimer = nil
			}
			call.session.Metadata = meta
			call.buffer = nil
			call.mu.Unlock()

			if call.rtpSession != nil {
				call.rtpSession.ClearBuffer()
			}

			s.logger.Info("Reusing active SIP call session for stream (seek/next track)",
				"player_id", playerID,
				"target", playerCfg.SIPTarget,
				"title", meta.Title,
				"artist", meta.Artist,
			)
			return
		}
		call.mu.Unlock()
	}

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

	// Flush buffered audio if in Announcement mode under lock before setting answered=true
	call.mu.Lock()
	var preBuffer []domain.AudioChunk
	if session.EffectiveMod == domain.BufferModeAnnouncement {
		preBuffer = call.buffer
		call.buffer = nil
	} else {
		call.buffer = nil
	}

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
	call.answered = true
	call.mu.Unlock()

	// Listen for remote hangup (phone physically hung up)
	select {
	case <-dialog.Done():
		s.logger.Info("Remote SIP phone hung up", "player_id", cfg.ID, "target", cfg.SIPTarget)
		s.ingress.SendPauseToUpstream(cfg.ID)
		s.terminatePlayerCall(cfg.ID, true)
	case <-session.Ctx.Done():
		// Local termination
	}
}

// OnStreamClear handles buffer flushing requested by Music Assistant (e.g. on seek).
func (s *BridgeService) OnStreamClear(playerID string) {
	if val, ok := s.activeCalls.Load(playerID); ok {
		call := val.(*activeCallState)
		call.mu.Lock()
		call.buffer = nil
		call.mu.Unlock()

		if call.rtpSession != nil {
			call.rtpSession.ClearBuffer()
		}
		s.logger.Debug("Flushed audio buffer on stream clear", "player_id", playerID)
	}
}

// OnPlaybackState handles playback state changes from Music Assistant (playing, paused, stopped).
func (s *BridgeService) OnPlaybackState(playerID string, state string) {
	s.logger.Debug("Playback state changed", "player_id", playerID, "state", state)
	switch state {
	case "paused", "stopped":
		s.handleStreamPauseOrStop(playerID)
	case "playing":
		s.playersMu.Lock()
		if p, ok := s.players[playerID]; ok {
			p.IsPlaying = true
		}
		s.playersMu.Unlock()

		if val, ok := s.activeCalls.Load(playerID); ok {
			call := val.(*activeCallState)
			call.mu.Lock()
			if call.lingerTimer != nil {
				call.lingerTimer.Stop()
				call.lingerTimer = nil
			}
			call.mu.Unlock()
		}
	}
}

// OnAudioChunk receives decoded audio chunks from the Sendspin player client.
func (s *BridgeService) OnAudioChunk(playerID string, chunk domain.AudioChunk) {
	val, ok := s.activeCalls.Load(playerID)
	if !ok {
		return
	}
	call := val.(*activeCallState)

	call.mu.Lock()
	if call.lingerTimer != nil {
		call.lingerTimer.Stop()
		call.lingerTimer = nil
	}
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

	s.playersMu.RLock()
	volume := 100
	if p, ok := s.players[playerID]; ok {
		volume = p.Volume
		if p.IsMuted {
			volume = 0
		}
	}
	s.playersMu.RUnlock()

	if err := call.rtpSession.PushAudio(chunk, volume); err != nil {
		s.logger.Debug("Failed to push audio to RTP session", "player_id", playerID, "err", err)
	}
}

// OnStreamEnd handles stream completion from Music Assistant.
func (s *BridgeService) OnStreamEnd(playerID string) {
	s.logger.Info("Stream ended from Music Assistant", "player_id", playerID)
	s.handleStreamPauseOrStop(playerID)
}

func (s *BridgeService) handleStreamPauseOrStop(playerID string) {
	s.playersMu.Lock()
	if p, ok := s.players[playerID]; ok {
		p.IsPlaying = false
	}
	s.playersMu.Unlock()

	val, ok := s.activeCalls.Load(playerID)
	if !ok {
		return
	}
	call := val.(*activeCallState)

	// Instantly clear any queued audio frames so playback stops immediately with 0ms delay
	call.mu.Lock()
	call.buffer = nil
	call.mu.Unlock()

	if call.rtpSession != nil {
		call.rtpSession.ClearBuffer()
	}

	lingerDelay := time.Duration(s.config.IdleHangupDelayMs) * time.Millisecond
	if lingerDelay <= 0 {
		s.terminatePlayerCall(playerID, true)
		return
	}

	call.mu.Lock()
	if call.session.GetState() != domain.StateActive && call.session.GetState() != domain.StateDialing {
		call.mu.Unlock()
		return
	}

	if call.lingerTimer != nil {
		call.lingerTimer.Stop()
	}

	call.lingerTimer = time.AfterFunc(lingerDelay, func() {
		call.mu.Lock()
		if call.lingerTimer == nil {
			call.mu.Unlock()
			return
		}
		call.lingerTimer = nil
		call.mu.Unlock()

		s.logger.Info("Idle linger timeout expired, terminating SIP call", "player_id", playerID)
		s.terminatePlayerCall(playerID, true)
	})
	call.mu.Unlock()
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

	call.mu.Lock()
	if call.lingerTimer != nil {
		call.lingerTimer.Stop()
		call.lingerTimer = nil
	}
	call.mu.Unlock()

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

	call.mu.Lock()
	if call.lingerTimer != nil {
		call.lingerTimer.Stop()
		call.lingerTimer = nil
	}
	call.mu.Unlock()

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

// GetStreamsDebugInfo returns debug information for all registered virtual player streams.
func (s *BridgeService) GetStreamsDebugInfo() map[string]StreamDebugInfo {
	s.playersMu.RLock()
	players := make([]*domain.Player, 0, len(s.players))
	for _, p := range s.players {
		players = append(players, p)
	}
	s.playersMu.RUnlock()

	result := make(map[string]StreamDebugInfo, len(players))
	for _, p := range players {
		result[p.Config.ID] = s.buildStreamDebugInfo(p)
	}
	return result
}

// GetStreamDebugInfo returns debug information for a specific player stream.
func (s *BridgeService) GetStreamDebugInfo(playerID string) (StreamDebugInfo, bool) {
	s.playersMu.RLock()
	p, ok := s.players[playerID]
	s.playersMu.RUnlock()

	if !ok {
		return StreamDebugInfo{}, false
	}
	return s.buildStreamDebugInfo(p), true
}

func (s *BridgeService) buildStreamDebugInfo(p *domain.Player) StreamDebugInfo {
	s.playersMu.RLock()
	id := p.Config.ID
	name := p.Config.Name
	isPlaying := p.IsPlaying
	isGrouped := p.IsGrouped
	vol := p.Volume
	muted := p.IsMuted
	cfg := p.Config
	s.playersMu.RUnlock()

	info := StreamDebugInfo{
		ID:        id,
		Name:      name,
		State:     "idle",
		IsPlaying: isPlaying,
		IsGrouped: isGrouped,
		Volume:    vol,
		Muted:     muted,
		Producers: make([]ProducerDebugInfo, 0, 1),
		Consumers: make([]ConsumerDebugInfo, 0, 1),
	}

	if isPlaying {
		info.State = "playing"
	}

	// 1. Gather Producer Info (Sendspin Ingress)
	ingStats, hasIngress := s.ingress.GetPlayerStats(id)
	var trackStr string
	if ingStats.Metadata.Artist != "" && ingStats.Metadata.Title != "" {
		trackStr = fmt.Sprintf("%s - %s", ingStats.Metadata.Artist, ingStats.Metadata.Title)
	} else if ingStats.Metadata.Title != "" {
		trackStr = ingStats.Metadata.Title
	}

	prodState := "idle"
	if isPlaying {
		prodState = "playing"
	} else if hasIngress && ingStats.Connected {
		prodState = "connected"
	}

	var prodFormat string
	bitrateIn := 0
	if ingStats.Codec != "" {
		if strings.EqualFold(ingStats.Codec, "opus") {
			prodFormat = fmt.Sprintf("OPUS %dHz %dch", ingStats.SampleRate, ingStats.Channels)
			bitrateIn = 128
		} else {
			prodFormat = fmt.Sprintf("%s %dHz %dch %dbit", strings.ToUpper(ingStats.Codec), ingStats.SampleRate, ingStats.Channels, ingStats.BitDepth)
			bitrateIn = (ingStats.SampleRate * ingStats.Channels * ingStats.BitDepth) / 1000
		}
	} else {
		prodFormat = "OPUS 48000Hz 2ch"
		bitrateIn = 128
	}

	ingressURL := ingStats.ServerAddr
	if ingressURL != "" && !strings.HasPrefix(ingressURL, "ws://") && !strings.HasPrefix(ingressURL, "http://") {
		ingressURL = "ws://" + ingressURL + "/sendspin"
	}

	info.Producers = append(info.Producers, ProducerDebugInfo{
		Type:           "Sendspin Ingress",
		URL:            ingressURL,
		Connected:      ingStats.Connected,
		Format:         prodFormat,
		Codec:          ingStats.Codec,
		SampleRate:     ingStats.SampleRate,
		Channels:       ingStats.Channels,
		BitDepth:       ingStats.BitDepth,
		BitrateKbps:    bitrateIn,
		OfferedFormats: ingStats.OfferedFormats,
		State:          prodState,
		Track:          trackStr,
		Artist:         ingStats.Metadata.Artist,
		Title:          ingStats.Metadata.Title,
		Album:          ingStats.Metadata.Album,
		AlbumArtist:    ingStats.Metadata.AlbumArtist,
		ChunksReceived: ingStats.ChunksReceived,
		BytesReceived:  ingStats.BytesReceived,
	})

	// 2. Gather Consumer Info (SIP/RTP Egress)
	allCodecs := domain.PrioritizeCodecs(cfg.Codec, nil)
	var offeredSIPCodecs []string
	for _, c := range allCodecs {
		offeredSIPCodecs = append(offeredSIPCodecs, fmt.Sprintf("%s (pt=%d, clock=%dHz)", strings.ToUpper(string(c)), c.PayloadType(), c.RTPClockRate()))
	}

	autoAnswerDesc := string(cfg.AutoAnswer)
	if cfg.CustomAutoAnswerHeader != "" {
		autoAnswerDesc = fmt.Sprintf("custom (%s)", cfg.CustomAutoAnswerHeader)
	}

	val, hasCall := s.activeCalls.Load(id)
	if hasCall {
		call := val.(*activeCallState)
		call.mu.Lock()
		sessionState := string(call.session.GetState())
		effectiveMode := string(call.session.EffectiveMod)
		priority := call.session.Priority
		bufferedCount := len(call.buffer)
		lingerActive := (call.lingerTimer != nil)
		startTime := call.session.StartTime
		answerTime := call.session.AnswerTime
		callID := ""
		if call.dialog != nil {
			// Dialog wrapper call id
		}
		call.mu.Unlock()

		var activeCodec domain.Codec = cfg.Codec
		var rtpStats RTPStats
		if call.rtpSession != nil {
			rtpStats = call.rtpSession.Stats()
			if rtpStats.Codec != "" {
				activeCodec = rtpStats.Codec
			}
		}

		if lingerActive {
			info.State = "lingering"
			sessionState = "lingering"
		} else if sessionState == string(domain.StateActive) {
			info.State = "active"
		} else if sessionState == string(domain.StateDialing) {
			info.State = "dialing"
		}

		var durationSec float64
		if !answerTime.IsZero() {
			durationSec = time.Since(answerTime).Seconds()
		} else if !startTime.IsZero() {
			durationSec = time.Since(startTime).Seconds()
		}

		var localRTPStr string
		if rtpStats.LocalPort > 0 {
			localRTPStr = fmt.Sprintf("0.0.0.0:%d", rtpStats.LocalPort)
		}

		bitrateOut := 64
		if activeCodec == domain.CodecOpus {
			bitrateOut = 96
		}

		info.Consumers = append(info.Consumers, ConsumerDebugInfo{
			Type:           "SIP/RTP Egress",
			URL:            cfg.SIPTarget,
			CallID:         callID,
			State:          sessionState,
			ConfigCodec:    string(cfg.Codec),
			ActiveCodec:    string(activeCodec),
			OfferedCodecs:  offeredSIPCodecs,
			NegotiatedSDP:  fmt.Sprintf("%s (pt=%d, clock=%dHz)", strings.ToUpper(string(activeCodec)), activeCodec.PayloadType(), activeCodec.RTPClockRate()),
			RTPClockRate:   activeCodec.RTPClockRate(),
			PayloadType:    activeCodec.PayloadType(),
			Format:         fmt.Sprintf("%s %dHz 1ch (%d kbps)", strings.ToUpper(string(activeCodec)), activeCodec.SampleRate(), bitrateOut),
			LocalRTP:       localRTPStr,
			RemoteRTP:      rtpStats.RemoteAddr,
			AutoAnswer:     autoAnswerDesc,
			BufferMode:     effectiveMode,
			Priority:       priority,
			BufferedChunks: bufferedCount,
			LingerActive:   lingerActive,
			PacketsSent:    rtpStats.PacketsSent,
			BytesSent:      rtpStats.BytesSent,
			BitrateKbps:    bitrateOut,
			DurationSec:    durationSec,
		})
	} else {
		bMode := cfg.BufferMode
		if bMode == "" {
			bMode = s.config.DefaultBufferMode
		}
		bitrateOut := 64
		if cfg.Codec == domain.CodecOpus {
			bitrateOut = 96
		}
		info.Consumers = append(info.Consumers, ConsumerDebugInfo{
			Type:          "SIP/RTP Egress",
			URL:           cfg.SIPTarget,
			State:         "idle",
			ConfigCodec:   string(cfg.Codec),
			ActiveCodec:   string(cfg.Codec),
			OfferedCodecs: offeredSIPCodecs,
			NegotiatedSDP: fmt.Sprintf("%s (pt=%d, clock=%dHz)", strings.ToUpper(string(cfg.Codec)), cfg.Codec.PayloadType(), cfg.Codec.RTPClockRate()),
			RTPClockRate:  cfg.Codec.RTPClockRate(),
			PayloadType:   cfg.Codec.PayloadType(),
			Format:        fmt.Sprintf("%s %dHz 1ch (%d kbps)", strings.ToUpper(string(cfg.Codec)), cfg.Codec.SampleRate(), bitrateOut),
			AutoAnswer:    autoAnswerDesc,
			BufferMode:    string(bMode),
			Priority:      cfg.Priority,
			BitrateKbps:   bitrateOut,
		})
	}

	return info
}
