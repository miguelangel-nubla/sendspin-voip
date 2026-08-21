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

const (
	// playerDiscoveryInterval is how often each player re-probes its downstream
	// SIP target for codec capability changes.
	playerDiscoveryInterval = 30 * time.Second
	// shutdownByeTimeout bounds how long shutdown waits for each SIP BYE.
	shutdownByeTimeout = 3 * time.Second
)

// BridgeConfig contains global operational parameters for the bridge.
type BridgeConfig struct {
	DefaultBufferMode  domain.BufferMode
	PickupBufferMs     int
	DrainDelayMs       int
	IdleHangupDelayMs  int
	PreAnswerBufferSec int
	ConflictPolicy     domain.ConflictPolicy
}

type activeCallState struct {
	session *domain.CallSession
	// rtpSession is set once at construction and never reassigned, so it is safe
	// to read without holding mu.
	rtpSession RTPSession

	mu     sync.Mutex
	dialog SIPDialog

	answered               bool
	done                   chan struct{}
	lingerTimer            *time.Timer
	streamStartProgressSec float64
	// lingerGen is bumped whenever a linger timer is armed or cancelled. A timer
	// callback that already fired and is blocked on mu compares the generation it
	// captured, so a stale expiry can never tear down a freshly armed call.
	lingerGen uint64
}

// setDialog publishes the SIP dialog once the INVITE has been answered.
func (c *activeCallState) setDialog(d SIPDialog) {
	c.mu.Lock()
	c.dialog = d
	c.mu.Unlock()
}

// getDialog returns the SIP dialog, or nil if the call was never answered.
func (c *activeCallState) getDialog() SIPDialog {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dialog
}

// cancelLingerLocked stops any armed linger timer. Callers must hold c.mu.
func (c *activeCallState) cancelLingerLocked() {
	if c.lingerTimer != nil {
		c.lingerTimer.Stop()
		c.lingerTimer = nil
	}
	c.lingerGen++
}

// cancelLinger stops any armed linger timer.
func (c *activeCallState) cancelLinger() {
	c.mu.Lock()
	c.cancelLingerLocked()
	c.mu.Unlock()
}

// BridgeService coordinates Sendspin player ingress with SIP call signaling and RTP media streaming.
type BridgeService struct {
	logger      *slog.Logger
	config      BridgeConfig
	arbiter     *domain.TargetArbiter
	sipCaller   SIPCallerPort
	rtpStreamer RTPStreamerPort
	ingress     PlayerIngressPort
	stateStore  StateStorePort

	// ctx is cancelled by Shutdown and bounds every background goroutine the
	// service starts, so nothing re-registers players after teardown begins.
	ctx          context.Context
	cancel       context.CancelFunc
	shutdownOnce sync.Once
	discoveryWG  sync.WaitGroup

	playersMu   sync.RWMutex
	players     map[string]*domain.Player
	activeCalls sync.Map // keyed by playerID -> *activeCallState
	probeCodecs sync.Map // keyed by playerID -> []domain.Codec (last successful probe)
}

// NewBridgeService creates a new bridge service.
func NewBridgeService(
	logger *slog.Logger,
	config BridgeConfig,
	arbiter *domain.TargetArbiter,
	sipCaller SIPCallerPort,
	rtpStreamer RTPStreamerPort,
	ingress PlayerIngressPort,
	stateStore StateStorePort,
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

	ctx, cancel := context.WithCancel(context.Background())

	return &BridgeService{
		logger:      logger,
		config:      config,
		arbiter:     arbiter,
		sipCaller:   sipCaller,
		rtpStreamer: rtpStreamer,
		ingress:     ingress,
		stateStore:  stateStore,
		ctx:         ctx,
		cancel:      cancel,
		players:     make(map[string]*domain.Player),
	}
}

// RegisterPlayers registers configured players and starts dynamic downstream discovery.
func (s *BridgeService) RegisterPlayers(configs []domain.PlayerConfig) error {
	s.playersMu.Lock()
	for i, cfg := range configs {
		if s.stateStore != nil {
			if rec, ok := s.stateStore.GetPlayerState(cfg.ID); ok {
				if rec.Volume > 0 && rec.Volume <= 100 {
					cfg.DefaultVolume = rec.Volume
					configs[i].DefaultVolume = rec.Volume
				}
			}
		}
		p, err := domain.NewPlayer(cfg)
		if err != nil {
			s.playersMu.Unlock()
			return fmt.Errorf("invalid player config %s: %w", cfg.ID, err)
		}
		if s.stateStore != nil {
			if rec, ok := s.stateStore.GetPlayerState(cfg.ID); ok {
				p.IsMuted = rec.Muted
			}
		}
		s.players[p.Config.ID] = p
	}
	s.playersMu.Unlock()

	// Perform initial probe & publish synchronously so players are available immediately
	for _, cfg := range configs {
		s.probeAndSyncPlayer(cfg)
		s.discoveryWG.Add(1)
		go s.runPlayerDiscoveryLoop(cfg)
	}

	return nil
}

func (s *BridgeService) runPlayerDiscoveryLoop(cfg domain.PlayerConfig) {
	defer s.discoveryWG.Done()

	ticker := time.NewTicker(playerDiscoveryInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			s.probeAndSyncPlayer(cfg)
		}
	}
}

func (s *BridgeService) probeAndSyncPlayer(cfg domain.PlayerConfig) {
	// Never re-register a player once shutdown has started; doing so would
	// resurrect ingress connections that StopAll is in the middle of closing.
	if s.ctx.Err() != nil {
		return
	}

	ctx, cancel := context.WithTimeout(s.ctx, 4*time.Second)
	defer cancel()

	codecs, err := s.sipCaller.ProbeTarget(ctx, cfg.SIPTarget)
	if err != nil {
		s.logger.Debug("Downstream SIP target probe note", "player_id", cfg.ID, "target", cfg.SIPTarget, "err", err)
		// Keep last successful discovery to avoid flip-flop reconnect storms.
		if cached, ok := s.probeCodecs.Load(cfg.ID); ok {
			codecs = cached.([]domain.Codec)
		} else {
			codecs = domain.PrioritizeCodecs(cfg.Codec, nil)
		}
	} else {
		codecs = domain.PrioritizeCodecs(cfg.Codec, codecs)
		s.probeCodecs.Store(cfg.ID, codecs)
		s.logger.Info("Discovered downstream SIP target capabilities",
			"player_id", cfg.ID,
			"target", cfg.SIPTarget,
			"codecs", codecs,
		)
	}

	if s.ctx.Err() != nil {
		return
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
			// Confirm this call is still the map entry (avoid reuse-after-terminate race)
			if cur, still := s.activeCalls.Load(playerID); !still || cur != call {
				call.mu.Unlock()
			} else {
				call.cancelLingerLocked()
				call.session.Metadata = meta

				startProgSec := float64(meta.ProgressMs) / 1000.0
				call.streamStartProgressSec = startProgSec
				call.mu.Unlock()

				s.logger.Info("Reusing active SIP call session for stream (next track / transition)",
					"player_id", playerID,
					"target", playerCfg.SIPTarget,
					"title", meta.Title,
					"artist", meta.Artist,
				)
				return
			}
		} else {
			call.mu.Unlock()
		}
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
		// Stop upstream Music Assistant for the preempted player so it stops feeding audio
		s.ingress.SendStopToUpstream(preempted.PlayerID)
		// Synchronously terminate the preempted call to prevent INVITE collision / 486 Busy
		s.terminatePlayerCallSync(preempted.PlayerID, false, 500*time.Millisecond)
	}

	// 3. Allocate local RTP socket with 3-stage pipeline
	rtpSess, err := s.rtpStreamer.CreateSession(playerCfg.Codec)
	if err != nil {
		s.logger.Error("Failed to create RTP session", "err", err)
		s.arbiter.ReleaseTarget(session)
		return
	}

	s.playersMu.RLock()
	volume := 100
	if p, ok := s.players[playerID]; ok {
		volume = p.Volume
		if p.IsMuted {
			volume = 0
		}
	}
	s.playersMu.RUnlock()

	rtpSess.SetBufferMode(effectiveMode)
	rtpSess.SetVolume(volume)

	startProgSec := float64(meta.ProgressMs) / 1000.0
	if startProgSec <= 0 {
		if stats, ok := s.ingress.GetPlayerStats(playerID); ok && stats.Metadata.ProgressMs > 0 {
			startProgSec = float64(stats.Metadata.ProgressMs) / 1000.0
		}
	}

	callState := &activeCallState{
		session:                session,
		rtpSession:             rtpSess,
		done:                   make(chan struct{}),
		streamStartProgressSec: startProgSec,
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

	call.setDialog(dialog)

	remoteRTP := dialog.RemoteRTPAddr()
	if remoteRTP == nil {
		s.logger.Error("No remote RTP address returned from SDP", "player_id", cfg.ID)
		s.terminatePlayerCall(cfg.ID, true)
		return
	}

	session.SetState(domain.StateActive)

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

	call.rtpSession.SetBufferMode(session.EffectiveMod)

	if err := call.rtpSession.StartTransmission(remoteRTP); err != nil {
		s.logger.Error("Failed to start RTP transmission", "player_id", cfg.ID, "err", err)
		s.terminatePlayerCall(cfg.ID, true)
		return
	}

	call.mu.Lock()
	call.answered = true
	call.mu.Unlock()

	s.logger.Info("SIP call connected & streaming RTP",
		"player_id", cfg.ID,
		"remote_rtp", remoteRTP.String(),
		"codec", activeCodec,
	)

	// Listen for remote hangup (phone physically hung up)
	select {
	case <-dialog.Done():
		s.logger.Info("Remote SIP phone hung up", "player_id", cfg.ID, "target", cfg.SIPTarget)
		s.ingress.SendStopToUpstream(cfg.ID)
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
		var dialDelay time.Duration
		if !call.session.StartTime.IsZero() && !call.session.AnswerTime.IsZero() {
			dialDelay = call.session.AnswerTime.Sub(call.session.StartTime)
		}
		bufferMode := call.session.EffectiveMod
		call.mu.Unlock()

		if call.rtpSession != nil {
			call.rtpSession.ClearBuffer()
			if bufferMode == domain.BufferModeAnnouncement && dialDelay > 0 {
				call.rtpSession.InjectSilence(dialDelay)
			}
		}
		s.logger.Debug("Flushed audio pipeline buffers on stream clear", "player_id", playerID)
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
			val.(*activeCallState).cancelLinger()
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
	call.cancelLinger()

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

	lingerDelay := time.Duration(s.config.IdleHangupDelayMs) * time.Millisecond

	// Calculate remaining buffer playout duration so the audio finishes playing completely
	var drainDuration time.Duration
	if call.rtpSession != nil {
		stats := call.rtpSession.Stats()
		bufferedFrames := stats.UpstreamChunks + stats.ConversionQueue
		if bufferedFrames > 0 {
			drainDuration = time.Duration(bufferedFrames) * 20 * time.Millisecond
		}
	}

	totalDelay := drainDuration + lingerDelay
	if totalDelay <= 0 {
		s.terminatePlayerCall(playerID, true)
		return
	}

	call.mu.Lock()
	if call.session.GetState() != domain.StateActive && call.session.GetState() != domain.StateDialing {
		call.mu.Unlock()
		return
	}

	call.cancelLingerLocked()
	gen := call.lingerGen

	call.lingerTimer = time.AfterFunc(totalDelay, func() {
		call.mu.Lock()
		// A timer that fired just as it was being cancelled (or replaced) blocks
		// here until the canceller releases mu. The generation check makes that
		// stale expiry a no-op instead of hanging up a call that has since
		// resumed playing.
		if call.lingerGen != gen || call.lingerTimer == nil {
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

// OnVolumeChange updates player volume gain. The Conversion ready queue
// is flushed so the new level applies immediately, while the raw Upstream
// buffer is left intact.
func (s *BridgeService) OnVolumeChange(playerID string, volume int) {
	if volume > 100 {
		volume = 100
	}
	if volume < 0 {
		volume = 0
	}

	var isMuted bool
	s.playersMu.Lock()
	if p, ok := s.players[playerID]; ok {
		p.Volume = volume
		isMuted = p.IsMuted
		s.logger.Debug("Player volume changed", "player_id", playerID, "volume", volume)
	}
	s.playersMu.Unlock()

	if s.stateStore != nil {
		_ = s.stateStore.SetPlayerState(playerID, PlayerStateRecord{Volume: volume, Muted: isMuted})
	}

	s.flushActiveRTPBuffer(playerID)
}

// OnMuteChange updates player mute status.
func (s *BridgeService) OnMuteChange(playerID string, muted bool) {
	var curVol int = 100
	s.playersMu.Lock()
	if p, ok := s.players[playerID]; ok {
		p.IsMuted = muted
		curVol = p.Volume
		s.logger.Debug("Player mute state changed", "player_id", playerID, "muted", muted)
	}
	s.playersMu.Unlock()

	if s.stateStore != nil {
		_ = s.stateStore.SetPlayerState(playerID, PlayerStateRecord{Volume: curVol, Muted: muted})
	}

	s.flushActiveRTPBuffer(playerID)
}

func (s *BridgeService) flushActiveRTPBuffer(playerID string) {
	s.playersMu.RLock()
	volume := 100
	if p, ok := s.players[playerID]; ok {
		volume = p.Volume
		if p.IsMuted {
			volume = 0
		}
	}
	s.playersMu.RUnlock()

	if val, ok := s.activeCalls.Load(playerID); ok {
		call := val.(*activeCallState)
		if call.rtpSession != nil {
			call.rtpSession.SetVolume(volume)
		}
	}
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

	call.cancelLinger()

	call.session.SetState(domain.StateTerminating)
	call.session.Close()

	if dialog := call.getDialog(); dialog != nil {
		byeCtx, cancel := context.WithTimeout(context.Background(), timeout)
		_ = dialog.Bye(byeCtx)
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

	call.cancelLinger()

	call.session.SetState(domain.StateTerminating)
	call.session.Close()

	go func() {
		if dialog := call.getDialog(); dialog != nil {
			byeCtx, cancel := context.WithTimeout(context.Background(), shutdownByeTimeout)
			_ = dialog.Bye(byeCtx)
			cancel()
		}

		drainDelay := time.Duration(s.config.DrainDelayMs) * time.Millisecond
		if call.rtpSession != nil {
			_ = call.rtpSession.DrainAndClose(drainDelay)
		}

		if releaseArbiter {
			s.arbiter.ReleaseTarget(call.session)
		}
		call.session.SetState(domain.StateTerminated)
	}()
}

// Shutdown cleanly stops all active calls and players. It blocks until every
// SIP BYE has been sent (or timed out), because the caller tears the process
// down immediately afterwards.
func (s *BridgeService) Shutdown() {
	s.logger.Info("Shutting down sendspin-voip bridge...")

	// Stop the discovery loops first so nothing re-registers a player behind us.
	s.shutdownOnce.Do(s.cancel)
	s.discoveryWG.Wait()

	// Hang up every active call *synchronously*. terminatePlayerCall defers the
	// BYE to a goroutine, which the process exit would race: the phones would be
	// left off-hook with a dead RTP stream until their own session timer fired.
	var playerIDs []string
	s.activeCalls.Range(func(key, _ any) bool {
		playerIDs = append(playerIDs, key.(string))
		return true
	})

	var wg sync.WaitGroup
	for _, playerID := range playerIDs {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			s.terminatePlayerCallSync(id, true, shutdownByeTimeout)
		}(playerID)
	}
	wg.Wait()

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
		if info, ok := s.GetStreamDebugInfo(p.Config.ID); ok {
			result[p.Config.ID] = info
		}
	}
	return result
}

// GetStreamDebugInfo compiles real-time diagnostics for a virtual player stream.
func (s *BridgeService) GetStreamDebugInfo(id string) (StreamDebugInfo, bool) {
	s.playersMu.RLock()
	player, exists := s.players[id]
	if !exists {
		s.playersMu.RUnlock()
		return StreamDebugInfo{}, false
	}
	cfg := player.Config
	vol := player.Volume
	muted := player.IsMuted
	isPlaying := player.IsPlaying
	isGrouped := player.IsGrouped
	s.playersMu.RUnlock()

	info := StreamDebugInfo{
		ID:        id,
		Name:      cfg.Name,
		State:     "idle",
		IsPlaying: isPlaying,
		IsGrouped: isGrouped,
		Volume:    vol,
		Muted:     muted,
		AudioPath: AudioPathDebugInfo{
			Muted:         muted,
			VolumePercent: vol,
		},
		Producers: make([]ProducerDebugInfo, 0, 1),
		Consumers: make([]ConsumerDebugInfo, 0, 1),
	}

	// 1. Gather Producer Info (Sendspin Ingress)
	ingStats, hasIngress := s.ingress.GetPlayerStats(id)
	prodState := "disconnected"
	if ingStats.Connected {
		if isPlaying {
			prodState = "streaming"
		} else {
			prodState = "connected"
		}
	}

	var trackStr string
	if ingStats.Metadata.Title != "" {
		if ingStats.Metadata.Artist != "" {
			trackStr = fmt.Sprintf("%s - %s", ingStats.Metadata.Artist, ingStats.Metadata.Title)
		} else {
			trackStr = ingStats.Metadata.Title
		}
	}

	prodFormat := "PCM 48000Hz 2ch 16bit"
	bitrateIn := 1536
	if hasIngress && ingStats.Codec != "" {
		if strings.ToLower(ingStats.Codec) == "opus" {
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

	var trackProgSec float64
	if isPlaying {
		trackProgSec = float64(ingStats.Metadata.ProgressMs) / 1000.0
		if !ingStats.Metadata.ProgressUpdated.IsZero() {
			trackProgSec += time.Since(ingStats.Metadata.ProgressUpdated).Seconds()
			if ingStats.Metadata.Duration > 0 && trackProgSec > ingStats.Metadata.Duration.Seconds() {
				trackProgSec = ingStats.Metadata.Duration.Seconds()
			}
		}
	} else if prodState == "paused" {
		trackProgSec = float64(ingStats.Metadata.ProgressMs) / 1000.0
	}

	var discoveredCodecs []string
	if cached, ok := s.probeCodecs.Load(id); ok {
		for _, c := range cached.([]domain.Codec) {
			discoveredCodecs = append(discoveredCodecs, string(c))
		}
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
		ExposedCodecs:  ingStats.ExposedCodecs,
		State:          prodState,
		Track:          trackStr,
		Artist:           ingStats.Metadata.Artist,
		Title:            ingStats.Metadata.Title,
		Album:            ingStats.Metadata.Album,
		AlbumArtist:      ingStats.Metadata.AlbumArtist,
		TrackDurationSec: ingStats.Metadata.Duration.Seconds(),
		TrackProgressSec: trackProgSec,
		ChunksReceived:   ingStats.ChunksReceived,
		BytesReceived:    ingStats.BytesReceived,
	})

	info.AudioPath.IngressCodec = ingStats.Codec
	info.AudioPath.IngressFormat = prodFormat
	if muted {
		info.AudioPath.VolumePercent = 0
	} else {
		info.AudioPath.VolumePercent = vol
	}

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
		lingerActive := (call.lingerTimer != nil)
		answered := call.answered
		startTime := call.session.StartTime
		answerTime := call.session.AnswerTime
		streamStartProgress := call.streamStartProgressSec
		callID := ""
		if call.dialog != nil {
			callID = call.dialog.CallID()
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

		bufferedCount := rtpStats.UpstreamChunks

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
		egressCh := 1
		if activeCodec == domain.CodecOpus {
			bitrateOut = bitrateIn
			if bitrateOut <= 0 {
				bitrateOut = 128
			}
			egressCh = 2
			if rtpStats.PathIngressChannels == 1 {
				egressCh = 1
			}
		}

		egressFormat := fmt.Sprintf("%s %dHz %dch (%d kbps)", strings.ToUpper(string(activeCodec)), activeCodec.SampleRate(), egressCh, bitrateOut)
		info.AudioPath.EgressCodec = string(activeCodec)
		info.AudioPath.EgressFormat = egressFormat
		info.AudioPath.BufferMode = effectiveMode
		if effectiveMode == "announcement" {
			info.AudioPath.PreAnswerBuffered = bufferedCount
		} else {
			info.AudioPath.PreAnswerBuffered = 0
		}
		info.AudioPath.UpstreamChunks = rtpStats.UpstreamChunks
		info.AudioPath.ConversionQueue = rtpStats.ConversionQueue
		info.AudioPath.PassthroughPackets = rtpStats.PassthroughPackets
		info.AudioPath.TranscodePackets = rtpStats.TranscodePackets

		// Calculate buffer timeline positions directly from PlayAt timestamps
		now := time.Now()
		var upStartOffset, upEndOffset, readyStartOffset, readyEndOffset float64

		if !rtpStats.UpstreamPlayAtStart.IsZero() && !rtpStats.UpstreamPlayAtEnd.IsZero() {
			upStartOffset = rtpStats.UpstreamPlayAtStart.Sub(now).Seconds()
			if upStartOffset < 0 {
				upStartOffset = 0
			}
			upEndOffset = rtpStats.UpstreamPlayAtEnd.Sub(now).Seconds() + 0.02
			if upEndOffset < upStartOffset {
				upEndOffset = upStartOffset
			}
		} else if rtpStats.UpstreamChunks > 0 {
			upEndOffset = float64(rtpStats.UpstreamChunks*20) / 1000.0
		}

		if !rtpStats.ReadyPlayAtStart.IsZero() && !rtpStats.ReadyPlayAtEnd.IsZero() {
			readyStartOffset = rtpStats.ReadyPlayAtStart.Sub(now).Seconds()
			if readyStartOffset < 0 {
				readyStartOffset = 0
			}
			readyEndOffset = rtpStats.ReadyPlayAtEnd.Sub(now).Seconds() + 0.02
			if readyEndOffset < readyStartOffset {
				readyEndOffset = readyStartOffset
			}
		} else if rtpStats.ConversionQueue > 0 {
			readyEndOffset = float64(rtpStats.ConversionQueue*20) / 1000.0
		}

		info.AudioPath.PlayheadSec = trackProgSec
		info.AudioPath.BufferStartSec = trackProgSec + upStartOffset
		info.AudioPath.BufferEndSec = trackProgSec + upEndOffset
		info.AudioPath.ReadyStartSec = trackProgSec + readyStartOffset
		info.AudioPath.ReadyEndSec = trackProgSec + readyEndOffset

		if effectiveMode == "announcement" {
			var dialDelaySec float64
			if !startTime.IsZero() {
				if answered && !answerTime.IsZero() {
					dialDelaySec = answerTime.Sub(startTime).Seconds()
				} else {
					dialDelaySec = time.Since(startTime).Seconds()
				}
			}
			info.AudioPath.PreAnswerBuffered = int(dialDelaySec / 0.02)

			if !answered {
				info.AudioPath.HoldBackStartSec = streamStartProgress
				info.AudioPath.HoldBackEndSec = streamStartProgress + dialDelaySec
			} else {
				phonePlayoutSec := streamStartProgress + durationSec
				if phonePlayoutSec > trackProgSec {
					phonePlayoutSec = trackProgSec
				}
				info.AudioPath.HoldBackStartSec = phonePlayoutSec
				info.AudioPath.HoldBackEndSec = phonePlayoutSec + dialDelaySec
			}
		} else {
			info.AudioPath.PreAnswerBuffered = 0
		}

		volDesc := volumeStageForDebug(info.AudioPath.VolumePercent, muted)

		switch {
		case !answered:
			info.AudioPath.Mode = "buffering"
			info.AudioPath.Passthrough = false
			info.AudioPath.Summary = fmt.Sprintf("1. Upstream buffer (%d chunks, mode=%s) → 2. Transcode (%s) → 3. RTP %s",
				bufferedCount, effectiveMode, volDesc, strings.ToUpper(string(activeCodec)))
			info.AudioPath.Stages = []string{
				fmt.Sprintf("Stage 1: Upstream Ingestion & Raw Buffer (%d chunks, start protected)", bufferedCount),
				fmt.Sprintf("Stage 2: Transcoding & Gain (%s, ready on SIP 200 OK)", volDesc),
				fmt.Sprintf("Stage 3: Downstream RTP Playout (%s mode, waiting for answer)", strings.ToUpper(effectiveMode)),
			}
		case rtpStats.PathMode != "":
			info.AudioPath.Mode = rtpStats.PathMode
			info.AudioPath.Passthrough = rtpStats.PathMode == "opus_passthrough"
			info.AudioPath.Summary = rtpStats.PathSummary
			info.AudioPath.Stages = rtpStats.PathStages
			if rtpStats.PathVolumePercent > 0 || rtpStats.PathMode != "" {
				info.AudioPath.VolumePercent = rtpStats.PathVolumePercent
			}
			if rtpStats.PathIngressCodec != "" {
				info.AudioPath.IngressCodec = rtpStats.PathIngressCodec
				info.AudioPath.IngressFormat = fmt.Sprintf("%s %dHz %dch",
					strings.ToUpper(rtpStats.PathIngressCodec), rtpStats.PathIngressRate, rtpStats.PathIngressChannels)
			}
		case answered:
			info.AudioPath.Mode = "transcode"
			info.AudioPath.Summary = prodFormat + " → transcode (" + volDesc + ") → RTP " + strings.ToUpper(string(activeCodec))
			info.AudioPath.Stages = []string{
				"Stage 1: Upstream Ingress (" + prodFormat + ")",
				fmt.Sprintf("Stage 2: Transcode (%s) → %s", volDesc, strings.ToUpper(string(activeCodec))),
				fmt.Sprintf("Stage 3: Downstream Playout (%s, 20ms pacing)", strings.ToUpper(effectiveMode)),
			}
		default:
			info.AudioPath.Mode = "dialing"
			info.AudioPath.Summary = "SIP dialing — media path initializing"
			info.AudioPath.Stages = []string{"Stage 1: Upstream Ingress (" + prodFormat + ")", "Stage 2: Codec Negotiation", "Stage 3: RTP Pending"}
		}

		info.Consumers = append(info.Consumers, ConsumerDebugInfo{
			Type:             "SIP/RTP Egress",
			URL:              cfg.SIPTarget,
			CallID:           callID,
			State:            sessionState,
			ConfigCodec:      string(cfg.Codec),
			ActiveCodec:      string(activeCodec),
			DiscoveredCodecs: discoveredCodecs,
			OfferedCodecs:    offeredSIPCodecs,
			NegotiatedSDP:    fmt.Sprintf("%s (pt=%d, clock=%dHz)", strings.ToUpper(string(activeCodec)), activeCodec.PayloadType(), activeCodec.RTPClockRate()),
			RTPClockRate:     activeCodec.RTPClockRate(),
			PayloadType:      activeCodec.PayloadType(),
			Format:           egressFormat,
			LocalRTP:         localRTPStr,
			RemoteRTP:        rtpStats.RemoteAddr,
			AutoAnswer:       autoAnswerDesc,
			BufferMode:       effectiveMode,
			Priority:         priority,
			BufferedChunks:   bufferedCount,
			LingerActive:     lingerActive,
			PacketsSent:      rtpStats.PacketsSent,
			BytesSent:        rtpStats.BytesSent,
			BitrateKbps:      bitrateOut,
			DurationSec:      durationSec,
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
		egressFormat := fmt.Sprintf("%s %dHz 1ch (%d kbps)", strings.ToUpper(string(cfg.Codec)), cfg.Codec.SampleRate(), bitrateOut)
		info.AudioPath.EgressCodec = string(cfg.Codec)
		info.AudioPath.EgressFormat = egressFormat
		info.AudioPath.BufferMode = string(bMode)
		info.AudioPath.Summary = "idle — " + prodFormat + " ⇢ (no call) ⇢ " + strings.ToUpper(string(cfg.Codec))
		info.AudioPath.Stages = []string{
			"Stage 1: Upstream Ingress (" + prodFormat + ")",
			"Stage 2: No active SIP session",
			"Stage 3: Configured Egress " + strings.ToUpper(string(cfg.Codec)),
		}

		info.Consumers = append(info.Consumers, ConsumerDebugInfo{
			Type:             "SIP/RTP Egress",
			URL:              cfg.SIPTarget,
			State:            "idle",
			ConfigCodec:      string(cfg.Codec),
			ActiveCodec:      string(cfg.Codec),
			DiscoveredCodecs: discoveredCodecs,
			OfferedCodecs:    offeredSIPCodecs,
			NegotiatedSDP:    fmt.Sprintf("%s (pt=%d, clock=%dHz)", strings.ToUpper(string(cfg.Codec)), cfg.Codec.PayloadType(), cfg.Codec.RTPClockRate()),
			RTPClockRate:     cfg.Codec.RTPClockRate(),
			PayloadType:      cfg.Codec.PayloadType(),
			Format:           egressFormat,
			AutoAnswer:       autoAnswerDesc,
			BufferMode:       string(bMode),
			Priority:         cfg.Priority,
			BitrateKbps:      bitrateOut,
		})
	}

	return info, true
}

func volumeStageForDebug(volumePercent int, muted bool) string {
	if muted || volumePercent <= 0 {
		return "volume mute"
	}
	if volumePercent >= 100 {
		return "volume 100% (0 dB)"
	}
	db := (float64(volumePercent)/100.0)*60.0 - 60.0
	return fmt.Sprintf("volume %d%% (%.0f dB)", volumePercent, db)
}
