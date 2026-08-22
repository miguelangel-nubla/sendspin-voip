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

const (
	// playerDiscoveryInterval is how often each player re-probes its downstream
	// SIP target for codec capability changes.
	playerDiscoveryInterval = 30 * time.Second
	// shutdownByeTimeout bounds how long shutdown waits for each SIP BYE.
	shutdownByeTimeout = 3 * time.Second
)

// BridgeConfig contains global operational parameters for the bridge.
type BridgeConfig struct {
	DrainDelayMs      int
	IdleHangupDelayMs int
	ConflictPolicy    domain.ConflictPolicy
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
	if config.DrainDelayMs <= 0 {
		config.DrainDelayMs = 500
	}
	if config.IdleHangupDelayMs < 0 {
		config.IdleHangupDelayMs = 0
	} else if config.IdleHangupDelayMs == 0 {
		config.IdleHangupDelayMs = 5000
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

	drainDelay := time.Duration(s.config.DrainDelayMs) * time.Millisecond
	sessionID := uuid.New().String()
	session := domain.NewCallSession(
		sessionID,
		playerID,
		playerCfg.SIPTarget,
		playerCfg.Priority,
		meta,
		drainDelay,
	)

	s.logger.Info("Stream started on player",
		"player_id", playerID,
		"target", playerCfg.SIPTarget,
		"title", meta.Title,
		"artist", meta.Artist,
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

	// 3. Allocate local RTP socket
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
		if call.rtpSession != nil {
			call.rtpSession.ClearBuffer()
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
	s.playersMu.RUnlock()
	if !exists {
		return StreamDebugInfo{}, false
	}

	ingStats, hasIngress := s.ingress.GetPlayerStats(id)

	var discoveredCodecs []string
	if cached, ok := s.probeCodecs.Load(id); ok {
		for _, c := range cached.([]domain.Codec) {
			discoveredCodecs = append(discoveredCodecs, string(c))
		}
	}

	var callSnap callDiagnosticsSnapshot
	if val, ok := s.activeCalls.Load(id); ok {
		call := val.(*activeCallState)
		call.mu.Lock()
		callSnap.hasCall = true
		callSnap.sessionState = string(call.session.GetState())
		callSnap.priority = call.session.Priority
		callSnap.lingerActive = (call.lingerTimer != nil)
		callSnap.answered = call.answered
		callSnap.startTime = call.session.StartTime
		callSnap.answerTime = call.session.AnswerTime
		callSnap.streamStartProgSec = call.streamStartProgressSec
		if call.dialog != nil {
			callSnap.callID = call.dialog.CallID()
		}
		call.mu.Unlock()

		if call.rtpSession != nil {
			callSnap.rtpStats = call.rtpSession.Stats()
		}
	}

	info := buildStreamDebugInfo(player, ingStats, hasIngress, discoveredCodecs, callSnap)
	return info, true
}


