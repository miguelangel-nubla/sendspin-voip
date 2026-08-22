package sendspin

import (
	"cmp"
	"context"
	"encoding/binary"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Sendspin/sendspin-go/pkg/discovery"
	"github.com/Sendspin/sendspin-go/pkg/protocol"
	sendspinsync "github.com/Sendspin/sendspin-go/pkg/sync"
	"github.com/miguelangel-nubla/sendspin-voip/internal/app"
	"github.com/miguelangel-nubla/sendspin-voip/internal/domain"
)

// IngressConfig defines Sendspin ingress adapter settings.
type IngressConfig struct {
	Server   string // "auto" for mDNS, or "ws://host:port" or "host:port"
	BufferMs int
}

type workerState struct {
	client         *protocol.Client
	connected      bool
	codec          string
	rate           int
	channels       int
	bitDepth       int
	offeredFormats []string
	exposedCodecs  []string
	meta           domain.StreamMetadata
	volume         int
	isMuted        bool
	chunksReceived uint64
	bytesReceived  uint64
	clockSync      *sendspinsync.ClockSync
}

type playerWorker struct {
	cfg     domain.PlayerConfig
	codecs  []domain.Codec
	handler app.PlayerEventHandler
	cancel  context.CancelFunc
	ctx     context.Context

	statsMu sync.RWMutex
	state   workerState
}

// setClient publishes the protocol client for the current connection attempt.
func (w *playerWorker) setClient(c *protocol.Client) {
	w.statsMu.Lock()
	w.state.client = c
	w.statsMu.Unlock()
}

// getClient returns the current protocol client, or nil if not connected yet.
func (w *playerWorker) getClient() *protocol.Client {
	w.statsMu.RLock()
	defer w.statsMu.RUnlock()
	return w.state.client
}

func (w *playerWorker) setConnected(connected bool) {
	w.statsMu.Lock()
	w.state.connected = connected
	w.statsMu.Unlock()
}

func (w *playerWorker) sendSynchronizedState(client *protocol.Client) {
	w.statsMu.RLock()
	vol := w.state.volume
	muted := w.state.isMuted
	w.statsMu.RUnlock()
	_ = client.SendState(protocol.PlayerState{State: "synchronized", Volume: vol, Muted: muted})
}

func (w *playerWorker) setVolume(client *protocol.Client, vol int) {
	w.statsMu.Lock()
	w.state.volume = vol
	muted := w.state.isMuted
	w.statsMu.Unlock()
	_ = client.SendState(protocol.PlayerState{State: "synchronized", Volume: vol, Muted: muted})
}

func (w *playerWorker) setMuted(client *protocol.Client, muted bool) {
	w.statsMu.Lock()
	w.state.isMuted = muted
	vol := w.state.volume
	w.statsMu.Unlock()
	_ = client.SendState(protocol.PlayerState{State: "synchronized", Volume: vol, Muted: muted})
}

func (w *playerWorker) setFormatsAndCodecs(offered []string, exposed []string) {
	w.statsMu.Lock()
	w.state.offeredFormats = offered
	w.state.exposedCodecs = exposed
	w.statsMu.Unlock()
}

func (w *playerWorker) setStreamConnected(codec string, rate, channels, bitDepth int, offered []string) {
	w.statsMu.Lock()
	w.state.connected = true
	w.state.codec = codec
	w.state.rate = rate
	w.state.channels = channels
	w.state.bitDepth = bitDepth
	w.state.offeredFormats = offered
	w.statsMu.Unlock()
}

func (w *playerWorker) onStreamStart(codec string, rate, channels, bitDepth int) domain.StreamMetadata {
	w.statsMu.Lock()
	defer w.statsMu.Unlock()
	w.state.codec = codec
	w.state.rate = rate
	w.state.channels = channels
	w.state.bitDepth = bitDepth
	w.state.meta.ProgressUpdated = time.Now()
	return w.state.meta
}

func (w *playerWorker) onStreamEnd() {
	w.statsMu.Lock()
	w.state.meta.ProgressMs = 0
	w.state.meta.ProgressUpdated = time.Time{}
	w.statsMu.Unlock()
}

func (w *playerWorker) onMetadata(meta domain.StreamMetadata) {
	w.statsMu.Lock()
	w.state.meta = meta
	w.statsMu.Unlock()
}

func (w *playerWorker) onAudioChunk(chunkLen int, serverTS int64) (time.Time, string, int, int, int) {
	w.statsMu.Lock()
	w.state.chunksReceived++
	w.state.bytesReceived += uint64(chunkLen)
	var playAt time.Time
	if serverTS > 0 && w.state.clockSync != nil {
		playAt = w.state.clockSync.ServerToLocalTime(serverTS)
	} else {
		playAt = time.Now()
	}
	codec := w.state.codec
	rate := w.state.rate
	channels := w.state.channels
	bitDepth := w.state.bitDepth
	w.statsMu.Unlock()
	return playAt, codec, rate, channels, bitDepth
}

func (w *playerWorker) processTimeSync(clientTransmitted, serverReceived, serverTransmitted, clientReceived int64) {
	w.statsMu.Lock()
	if w.state.clockSync != nil {
		w.state.clockSync.ProcessSyncResponse(
			clientTransmitted,
			serverReceived,
			serverTransmitted,
			clientReceived,
		)
	}
	w.statsMu.Unlock()
}

// Ingress implements app.PlayerIngressPort using Sendspin wire protocol.
type Ingress struct {
	logger       *slog.Logger
	config       IngressConfig
	serverAddr   string
	discoveredCh chan string
	workers      map[string]*playerWorker
	mu           sync.Mutex
	discMgr      *discovery.Manager
	ctx          context.Context
	cancel       context.CancelFunc
}

// NewIngress creates a new pure-Go Sendspin player ingress adapter.
func NewIngress(logger *slog.Logger, cfg IngressConfig) *Ingress {
	logger = cmp.Or(logger, slog.Default())
	cfg.BufferMs = cmp.Or(cfg.BufferMs, 500)
	cfg.Server = cmp.Or(cfg.Server, "auto")

	ctx, cancel := context.WithCancel(context.Background())

	ing := &Ingress{
		logger:       logger,
		config:       cfg,
		discoveredCh: make(chan string, 1),
		workers:      make(map[string]*playerWorker),
		ctx:          ctx,
		cancel:       cancel,
	}

	if cfg.Server == "auto" {
		go ing.runDiscovery()
	} else {
		ing.serverAddr = normalizeServerAddr(cfg.Server)
	}

	return ing
}

func normalizeServerAddr(addr string) string {
	addr = strings.TrimPrefix(addr, "ws://")
	addr = strings.TrimPrefix(addr, "http://")
	addr = strings.TrimSuffix(addr, "/sendspin")
	addr = strings.TrimSuffix(addr, "/")
	return addr
}

func (ing *Ingress) runDiscovery() {
	ing.logger.Info("Starting mDNS discovery for Sendspin / Music Assistant server...")
	mgr := discovery.NewManager(discovery.Config{
		ServiceName: "sendspin-voip",
		ServerMode:  false,
	})
	// discMgr is read by StopAll under ing.mu, so publish it under the same lock
	// rather than racing with a concurrent shutdown.
	ing.mu.Lock()
	ing.discMgr = mgr
	ing.mu.Unlock()

	if err := mgr.Browse(); err != nil {
		ing.logger.Warn("mDNS browse failed, will retry", "err", err)
	}

	for {
		select {
		case <-ing.ctx.Done():
			mgr.Stop()
			return
		case srv, ok := <-mgr.Servers():
			if !ok {
				return
			}
			addr := net.JoinHostPort(srv.Host, strconv.Itoa(srv.Port))
			ing.logger.Info("Discovered Sendspin server via mDNS", "name", srv.Name, "address", addr)

			ing.mu.Lock()
			prev := ing.serverAddr
			if prev == "" {
				ing.serverAddr = addr
				ing.mu.Unlock()
				select {
				case ing.discoveredCh <- addr:
				default:
				}
			} else if prev != addr {
				// Allow failover when current server is not connected
				allDown := true
				for _, w := range ing.workers {
					w.statsMu.RLock()
					connected := w.state.connected
					w.statsMu.RUnlock()
					if connected {
						allDown = false
						break
					}
				}
				if allDown {
					ing.logger.Info("Switching Sendspin server after mDNS rediscovery",
						"from", prev, "to", addr)
					ing.serverAddr = addr
					ing.mu.Unlock()
					select {
					case ing.discoveredCh <- addr:
					default:
					}
				} else {
					ing.mu.Unlock()
				}
			} else {
				ing.mu.Unlock()
			}
		}
	}
}

// RegisterPlayer spawns a virtual Sendspin player client.
func (ing *Ingress) RegisterPlayer(player domain.PlayerConfig, handler app.PlayerEventHandler) error {
	return ing.RegisterPlayerWithCodecs(player, nil, handler)
}

// RegisterPlayerWithCodecs creates or updates a virtual Sendspin player with dynamically discovered downstream codecs.
func (ing *Ingress) RegisterPlayerWithCodecs(player domain.PlayerConfig, codecs []domain.Codec, handler app.PlayerEventHandler) error {
	ing.mu.Lock()
	defer ing.mu.Unlock()

	initialVol := player.DefaultVolume
	initialMuted := false

	// If already registered with identical codecs and connected, skip
	if existing, ok := ing.workers[player.ID]; ok {
		existing.statsMu.RLock()
		existingClient := existing.state.client
		if existing.state.volume >= 0 && existing.state.volume <= 100 {
			initialVol = existing.state.volume
		}
		initialMuted = existing.state.isMuted
		existing.statsMu.RUnlock()

		if slices.Equal(existing.codecs, codecs) && existingClient != nil && existingClient.IsConnected() {
			return nil
		}

		existing.cancel()
		if existingClient != nil {
			existingClient.Close()
		}
		delete(ing.workers, player.ID)
	}

	if initialVol < 0 || initialVol > 100 {
		initialVol = 100
	}

	ctx, cancel := context.WithCancel(ing.ctx)
	worker := &playerWorker{
		cfg:     player,
		codecs:  codecs,
		handler: handler,
		cancel:  cancel,
		ctx:     ctx,
		state: workerState{
			volume:    initialVol,
			isMuted:   initialMuted,
			clockSync: sendspinsync.NewClockSync(),
		},
	}
	ing.workers[player.ID] = worker

	go ing.runPlayerClient(worker)
	return nil
}

// UnregisterPlayer stops and disconnects a virtual player client from Music Assistant.
func (ing *Ingress) UnregisterPlayer(playerID string) error {
	ing.mu.Lock()
	defer ing.mu.Unlock()

	if worker, ok := ing.workers[playerID]; ok {
		worker.cancel()
		if client := worker.getClient(); client != nil {
			_ = client.SendGoodbye("shutdown")
			client.Close()
		}
		delete(ing.workers, playerID)
		ing.logger.Info("Unregistered virtual player from Music Assistant", "player_id", playerID)
	}
	return nil
}

func drainChannel[T any](ch <-chan T) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

// BuildSupportedFormatsForCodecs generates the exact ordered AudioFormat capability list based on discovered SIP codecs.
func BuildSupportedFormatsForCodecs(codecs []domain.Codec, preferred domain.Codec) ([]protocol.AudioFormat, []string, []int, []int) {
	var formats []protocol.AudioFormat
	var supportCodecs []string
	var supportSampleRates []int
	var supportChannels []int

	seenFmt := make(map[string]bool)
	seenCodec := make(map[string]bool)
	seenRate := make(map[int]bool)
	seenChan := make(map[int]bool)

	addFormat := func(f protocol.AudioFormat) {
		key := fmt.Sprintf("%s-%d-%d-%d", f.Codec, f.SampleRate, f.Channels, f.BitDepth)
		if !seenFmt[key] {
			formats = append(formats, f)
			seenFmt[key] = true
		}
		if !seenCodec[f.Codec] {
			supportCodecs = append(supportCodecs, f.Codec)
			seenCodec[f.Codec] = true
		}
		if !seenRate[f.SampleRate] {
			supportSampleRates = append(supportSampleRates, f.SampleRate)
			seenRate[f.SampleRate] = true
		}
		if !seenChan[f.Channels] {
			supportChannels = append(supportChannels, f.Channels)
			seenChan[f.Channels] = true
		}
	}

	ordered := domain.PrioritizeCodecs(preferred, codecs)

	for _, c := range ordered {
		switch c {
		case domain.CodecOpus:
			// Opus Full-band (Highest fidelity stereo/mono, zero-transcode)
			addFormat(protocol.AudioFormat{Codec: "opus", SampleRate: 48000, Channels: 2, BitDepth: 16})
			addFormat(protocol.AudioFormat{Codec: "opus", SampleRate: 48000, Channels: 1, BitDepth: 16})
		case domain.CodecL16:
			// L16 Uncompressed PCM (48kHz/44.1kHz Stereo)
			addFormat(protocol.AudioFormat{Codec: "pcm", SampleRate: 48000, Channels: 2, BitDepth: 16})
			addFormat(protocol.AudioFormat{Codec: "pcm", SampleRate: 44100, Channels: 2, BitDepth: 16})
		case domain.CodecG722:
			// Wideband 16kHz Mono (Native G.722 match: 1 channel, 16000Hz)
			addFormat(protocol.AudioFormat{Codec: "pcm", SampleRate: 16000, Channels: 1, BitDepth: 16})
		case domain.CodecPCMU, domain.CodecPCMA:
			// Narrowband 8kHz Mono (Native G.711 match: 1 channel, 8000Hz)
			addFormat(protocol.AudioFormat{Codec: "pcm", SampleRate: 8000, Channels: 1, BitDepth: 16})
		}
	}

	// Standard PCM Stereo fallbacks
	addFormat(protocol.AudioFormat{Codec: "pcm", SampleRate: 48000, Channels: 2, BitDepth: 16})
	addFormat(protocol.AudioFormat{Codec: "pcm", SampleRate: 44100, Channels: 2, BitDepth: 16})

	return formats, supportCodecs, supportSampleRates, supportChannels
}

// SendStopToUpstream sends a stop command to Music Assistant for the given player.
func (ing *Ingress) SendStopToUpstream(playerID string) {
	ing.sendCommandToUpstream(playerID, map[string]any{"command": "stop"})
}

// SendNextToUpstream sends a next track command to Music Assistant.
func (ing *Ingress) SendNextToUpstream(playerID string) {
	ing.sendCommandToUpstream(playerID, map[string]any{"command": "next"})
}

// SendPlayPauseToUpstream sends a play/pause toggle command to Music Assistant.
func (ing *Ingress) SendPlayPauseToUpstream(playerID string) {
	ing.sendCommandToUpstream(playerID, map[string]any{"command": "play_pause"})
}

// SendVolumeToUpstream sends a volume change command to Music Assistant.
func (ing *Ingress) SendVolumeToUpstream(playerID string, volume int) {
	ing.mu.Lock()
	w, ok := ing.workers[playerID]
	ing.mu.Unlock()
	if !ok {
		return
	}
	if client := w.getClient(); client != nil && client.IsConnected() {
		w.setVolume(client, volume)
	}
}

// SendMuteToUpstream sends a mute change command to Music Assistant.
func (ing *Ingress) SendMuteToUpstream(playerID string, muted bool) {
	ing.mu.Lock()
	w, ok := ing.workers[playerID]
	ing.mu.Unlock()
	if !ok {
		return
	}
	if client := w.getClient(); client != nil && client.IsConnected() {
		w.setMuted(client, muted)
	}
}

func (ing *Ingress) sendCommandToUpstream(playerID string, cmd map[string]any) {
	ing.mu.Lock()
	w, ok := ing.workers[playerID]
	ing.mu.Unlock()
	if !ok {
		return
	}
	client := w.getClient()
	if client == nil || !client.IsConnected() {
		return
	}
	payload := map[string]any{
		"controller": cmd,
	}
	if err := client.Send("client/command", payload); err != nil {
		ing.logger.Warn("Failed to send command to upstream Music Assistant", "player_id", playerID, "cmd", cmd, "err", err)
	} else {
		ing.logger.Info("Sent command to upstream Music Assistant", "player_id", playerID, "cmd", cmd)
	}
}

func (ing *Ingress) runPlayerClient(w *playerWorker) {
	consecutiveFails := 0

	for {
		select {
		case <-w.ctx.Done():
			return
		default:
		}

		serverAddr := ing.getServerAddr()
		if serverAddr == "" {
			select {
			case <-w.ctx.Done():
				return
			case serverAddr = <-ing.discoveredCh:
			case <-time.After(2 * time.Second):
				continue
			}
		}

		ing.logger.Info("Connecting player client to Sendspin server",
			"player_id", w.cfg.ID,
			"player_name", w.cfg.Name,
			"server", serverAddr,
		)

		supportedFormats, supportCodecs, supportSampleRates, supportChannels := BuildSupportedFormatsForCodecs(w.codecs, w.cfg.Codec)

		var offeredFmtStrings []string
		for _, f := range supportedFormats {
			offeredFmtStrings = append(offeredFmtStrings, fmt.Sprintf("%s %dHz %dch %dbit", strings.ToUpper(f.Codec), f.SampleRate, f.Channels, f.BitDepth))
		}
		w.setFormatsAndCodecs(offeredFmtStrings, supportCodecs)

		client := protocol.NewClient(protocol.Config{
			ServerAddr:     serverAddr,
			ClientID:       w.cfg.ID,
			Name:           w.cfg.Name,
			Version:        1,
			SupportedRoles: []string{"player@v1", "metadata@v1", "controller@v1"},
			PlayerV1Support: protocol.PlayerV1Support{
				SupportedFormats:   supportedFormats,
				BufferCapacity:     1048576,
				SupportedCommands:  []string{"volume", "mute"},
				SupportCodecs:      supportCodecs,
				SupportSampleRates: supportSampleRates,
				SupportChannels:    supportChannels,
				SupportBitDepth:    []int{16},
			},
		})

		if err := client.Connect(); err != nil {
			consecutiveFails++
			ing.logger.Warn("Failed to connect to Sendspin server, retrying in 3s",
				"player_id", w.cfg.ID, "err", err, "fails", consecutiveFails)
			if consecutiveFails >= 3 && strings.EqualFold(ing.config.Server, "auto") {
				ing.clearServerAddr(serverAddr)
			}
			time.Sleep(3 * time.Second)
			continue
		}
		consecutiveFails = 0

		var primaryCodec = "opus"
		var primaryRate = 48000
		var primaryChannels = 2
		var primaryBitDepth = 16
		if len(supportedFormats) > 0 {
			primaryCodec = supportedFormats[0].Codec
			primaryRate = supportedFormats[0].SampleRate
			primaryChannels = supportedFormats[0].Channels
			primaryBitDepth = supportedFormats[0].BitDepth
			if primaryBitDepth <= 0 {
				primaryBitDepth = 16
			}
		}

		w.setStreamConnected(primaryCodec, primaryRate, primaryChannels, primaryBitDepth, offeredFmtStrings)
		w.setClient(client)
		w.sendSynchronizedState(client)

		w.statsMu.RLock()
		initVol := w.state.volume
		w.statsMu.RUnlock()
		ing.logger.Info("Player client successfully connected to Music Assistant", "player_id", w.cfg.ID, "volume", initVol)

		var currentCodec = primaryCodec
		var currentRate = primaryRate
		var currentChannels = primaryChannels
		var currentBitDepth = primaryBitDepth

		done := client.Done()
		_ = client.SendTimeSync(time.Now().UnixMicro())

		timeSyncTicker := time.NewTicker(2 * time.Second)

	eventLoop:
		for {
			select {
			case <-w.ctx.Done():
				timeSyncTicker.Stop()
				w.setConnected(false)
				_ = client.SendGoodbye("shutdown")
				client.Close()
				return
			case <-done:
				timeSyncTicker.Stop()
				w.setConnected(false)
				ing.logger.Warn("Sendspin player disconnected, will reconnect", "player_id", w.cfg.ID)
				break eventLoop

			case <-timeSyncTicker.C:
				_ = client.SendTimeSync(time.Now().UnixMicro())

			case timeResp := <-client.TimeSyncResp:
				w.processTimeSync(
					timeResp.ClientTransmitted,
					timeResp.ServerReceived,
					timeResp.ServerTransmitted,
					time.Now().UnixMicro(),
				)

			case startMsg := <-client.StreamStart:
				// Drain any remaining stale chunks in client.AudioChunks channel from previous stream/seek
				drainChannel(client.AudioChunks)
				if startMsg.Player != nil {
					currentCodec = cmp.Or(strings.ToLower(startMsg.Player.Codec), "pcm")
					currentRate = cmp.Or(startMsg.Player.SampleRate, primaryRate)
					currentChannels = domain.NormalizeChannels(startMsg.Player.Channels, primaryChannels)
					currentBitDepth = cmp.Or(startMsg.Player.BitDepth, 16)
				}
				meta := w.onStreamStart(currentCodec, currentRate, currentChannels, currentBitDepth)

				ing.logger.Info("Sendspin stream start received",
					"player_id", w.cfg.ID,
					"codec", currentCodec,
					"sample_rate", currentRate,
					"channels", currentChannels,
					"bit_depth", currentBitDepth,
					"title", meta.Title,
					"progress_ms", meta.ProgressMs,
				)
				w.sendSynchronizedState(client)
				w.handler.OnStreamStart(w.cfg.ID, meta)

			case <-client.StreamEnd:
				ing.logger.Info("Sendspin stream end received", "player_id", w.cfg.ID)
				w.onStreamEnd()
				w.handler.OnStreamEnd(w.cfg.ID)

			case <-client.StreamClear:
				ing.logger.Debug("Sendspin stream clear received (flushing buffer)", "player_id", w.cfg.ID)
				drainChannel(client.AudioChunks)
				w.handler.OnStreamClear(w.cfg.ID)

			case stateMsg := <-client.ServerState:
				if stateMsg.Metadata != nil {
					meta, pbState := parseServerMetadata(stateMsg.Metadata)
					w.onMetadata(meta)
					w.handler.OnMetadata(w.cfg.ID, meta)

					if pbState != "" {
						w.handler.OnPlaybackState(w.cfg.ID, pbState)
					}
				}

			case grpMsg := <-client.GroupUpdate:
				isGrouped := grpMsg.GroupID != nil && *grpMsg.GroupID != ""
				w.handler.OnGroupUpdate(w.cfg.ID, isGrouped)
				if grpMsg.PlaybackState != nil && *grpMsg.PlaybackState != "" {
					w.handler.OnPlaybackState(w.cfg.ID, *grpMsg.PlaybackState)
				}

			case cmd := <-client.ControlMsgs:
				if cmd.Command == "volume" {
					w.setVolume(client, cmd.Volume)
					w.handler.OnVolumeChange(w.cfg.ID, cmd.Volume)
				} else if cmd.Command == "mute" {
					w.setMuted(client, cmd.Mute)
					w.handler.OnMuteChange(w.cfg.ID, cmd.Mute)
				}

			case chunk := <-client.AudioChunks:
				playAt, codec, rate, channels, bitDepth := w.onAudioChunk(len(chunk.Data), chunk.Timestamp)
				audioChunk := decodeIncomingAudioChunk(
					chunk, codec, rate, channels, bitDepth, playAt,
				)
				w.handler.OnAudioChunk(w.cfg.ID, audioChunk)
			}
		}

		w.setConnected(false)
		client.Close()
		consecutiveFails++
		if consecutiveFails >= 3 && strings.EqualFold(ing.config.Server, "auto") {
			ing.clearServerAddr(serverAddr)
		}
		time.Sleep(2 * time.Second)
	}
}

func (ing *Ingress) clearServerAddr(current string) {
	ing.mu.Lock()
	defer ing.mu.Unlock()
	if ing.serverAddr == current {
		ing.logger.Info("Clearing Sendspin server address to allow mDNS rediscovery", "was", current)
		ing.serverAddr = ""
	}
}

func parseServerMetadata(meta *protocol.MetadataState) (domain.StreamMetadata, string) {
	if meta == nil {
		return domain.StreamMetadata{}, ""
	}
	var trackDuration time.Duration
	var trackProgMs int
	var pbState string
	if meta.Progress != nil {
		if meta.Progress.TrackDuration > 0 {
			trackDuration = time.Duration(meta.Progress.TrackDuration) * time.Millisecond
		}
		trackProgMs = meta.Progress.TrackProgress
		if meta.Progress.PlaybackSpeed == 0 {
			pbState = "paused"
		} else if meta.Progress.PlaybackSpeed > 0 {
			pbState = "playing"
		}
	}
	return domain.StreamMetadata{
		Title:           ptrString(meta.Title),
		Artist:          ptrString(meta.Artist),
		AlbumArtist:     ptrString(meta.AlbumArtist),
		Album:           ptrString(meta.Album),
		StreamTitle:     ptrString(meta.Title),
		Duration:        trackDuration,
		ProgressMs:      trackProgMs,
		ProgressUpdated: time.Now(),
	}, pbState
}

func ptrString(p *string) string {
	if p != nil {
		return *p
	}
	return ""
}

func decodeIncomingAudioChunk(
	chunk protocol.AudioChunk,
	currentCodec string,
	currentRate, currentChannels, currentBitDepth int,
	playAt time.Time,
) domain.AudioChunk {
	if currentCodec == "opus" {
		return domain.AudioChunk{
			Timestamp:  chunk.Timestamp,
			PlayAt:     playAt,
			OpusData:   chunk.Data,
			SampleRate: 48000,
			Channels:   currentChannels,
			BitDepth:   16,
		}
	}

	samples := decodePCM(chunk.Data, currentBitDepth)
	return domain.AudioChunk{
		Timestamp:  chunk.Timestamp,
		PlayAt:     playAt,
		Samples:    samples,
		SampleRate: currentRate,
		Channels:   currentChannels,
		BitDepth:   currentBitDepth,
	}
}

func decodePCM(data []byte, bitDepth int) []int32 {
	if bitDepth == 24 {
		numSamples := len(data) / 3
		samples := make([]int32, numSamples)
		for i := 0; i < numSamples; i++ {
			offset := i * 3
			// Sign-extend 24-bit little-endian sample to int32
			val := int32(data[offset]) | (int32(data[offset+1]) << 8) | (int32(int8(data[offset+2])) << 16)
			// Normalize from 24-bit [-8388608, 8388607] to 16-bit [-32768, 32767]
			samples[i] = val >> 8
		}
		return samples
	}

	numSamples := len(data) / 2
	samples := make([]int32, numSamples)
	for i := 0; i < numSamples; i++ {
		val := int16(binary.LittleEndian.Uint16(data[i*2:]))
		samples[i] = int32(val)
	}
	return samples
}

func (ing *Ingress) getServerAddr() string {
	ing.mu.Lock()
	defer ing.mu.Unlock()
	return ing.serverAddr
}

// GetPlayerStats retrieves current ingress stats and metadata for a player.
func (ing *Ingress) GetPlayerStats(playerID string) (app.IngressPlayerStats, bool) {
	ing.mu.Lock()
	w, ok := ing.workers[playerID]
	serverAddr := ing.serverAddr
	ing.mu.Unlock()

	if !ok {
		return app.IngressPlayerStats{}, false
	}

	w.statsMu.RLock()
	defer w.statsMu.RUnlock()

	return app.IngressPlayerStats{
		ServerAddr:     serverAddr,
		Connected:      w.state.connected,
		Codec:          w.state.codec,
		SampleRate:     w.state.rate,
		Channels:       w.state.channels,
		BitDepth:       w.state.bitDepth,
		OfferedFormats: w.state.offeredFormats,
		ExposedCodecs:  w.state.exposedCodecs,
		Metadata:       w.state.meta,
		ChunksReceived: w.state.chunksReceived,
		BytesReceived:  w.state.bytesReceived,
	}, true
}

// StopAll stops all virtual players and cancels background tasks.
func (ing *Ingress) StopAll() error {
	ing.mu.Lock()
	defer ing.mu.Unlock()

	ing.cancel()
	for _, w := range ing.workers {
		w.cancel()
		if client := w.getClient(); client != nil {
			_ = client.SendGoodbye("shutdown")
			client.Close()
		}
	}
	if ing.discMgr != nil {
		ing.discMgr.Stop()
	}
	return nil
}
