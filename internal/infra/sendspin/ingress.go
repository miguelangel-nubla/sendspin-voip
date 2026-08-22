package sendspin

import (
	"context"
	"encoding/binary"
	"fmt"
	"log/slog"
	"net"
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

type playerWorker struct {
	cfg     domain.PlayerConfig
	codecs  []domain.Codec
	handler app.PlayerEventHandler
	cancel  context.CancelFunc
	ctx     context.Context

	statsMu sync.RWMutex
	// client is written by the worker goroutine on every (re)connect and read by
	// callers on the Ingress API surface, so it lives under statsMu like the rest
	// of the worker's mutable state. Use setClient/getClient.
	client          *protocol.Client
	connected       bool
	currentCodec    string
	currentRate     int
	currentChannels int
	currentBitDepth int
	offeredFormats  []string
	exposedCodecs   []string
	currentMeta     domain.StreamMetadata
	currentVolume   int
	isMuted         bool
	chunksReceived  uint64
	bytesReceived   uint64
	// clockSync implements official Sendspin Kalman filter time synchronization.
	clockSync *sendspinsync.ClockSync
}

// setClient publishes the protocol client for the current connection attempt.
func (w *playerWorker) setClient(c *protocol.Client) {
	w.statsMu.Lock()
	w.client = c
	w.statsMu.Unlock()
}

// getClient returns the current protocol client, or nil if not connected yet.
func (w *playerWorker) getClient() *protocol.Client {
	w.statsMu.RLock()
	defer w.statsMu.RUnlock()
	return w.client
}

func (w *playerWorker) sendSynchronizedState(client *protocol.Client) {
	w.statsMu.RLock()
	vol := w.currentVolume
	muted := w.isMuted
	w.statsMu.RUnlock()
	_ = client.SendState(protocol.PlayerState{State: "synchronized", Volume: vol, Muted: muted})
}

func (w *playerWorker) setVolume(client *protocol.Client, vol int) {
	w.statsMu.Lock()
	w.currentVolume = vol
	muted := w.isMuted
	w.statsMu.Unlock()
	_ = client.SendState(protocol.PlayerState{State: "synchronized", Volume: vol, Muted: muted})
}

func (w *playerWorker) setMuted(client *protocol.Client, muted bool) {
	w.statsMu.Lock()
	w.isMuted = muted
	vol := w.currentVolume
	w.statsMu.Unlock()
	_ = client.SendState(protocol.PlayerState{State: "synchronized", Volume: vol, Muted: muted})
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
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.BufferMs <= 0 {
		cfg.BufferMs = 500
	}
	if cfg.Server == "" {
		cfg.Server = "auto"
	}

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
					connected := w.connected
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
// RegisterPlayer creates and starts a virtual Sendspin player client.
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
		existingClient := existing.client
		if existing.currentVolume > 0 {
			initialVol = existing.currentVolume
		}
		initialMuted = existing.isMuted
		existing.statsMu.RUnlock()

		if codecsEqual(existing.codecs, codecs) && existingClient != nil && existingClient.IsConnected() {
			return nil
		}

		existing.cancel()
		if existingClient != nil {
			existingClient.Close()
		}
		delete(ing.workers, player.ID)
	}

	if initialVol <= 0 || initialVol > 100 {
		initialVol = 100
	}

	ctx, cancel := context.WithCancel(ing.ctx)
	worker := &playerWorker{
		cfg:           player,
		codecs:        codecs,
		handler:       handler,
		cancel:        cancel,
		ctx:           ctx,
		currentVolume: initialVol,
		isMuted:       initialMuted,
		clockSync:     sendspinsync.NewClockSync(),
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

func codecsEqual(a, b []domain.Codec) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
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
// This is called when the remote SIP phone hangs up (or the call is preempted), to
// stop MA from continuing to stream since there is no longer anywhere to deliver it.
func (ing *Ingress) SendStopToUpstream(playerID string) {
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
		"controller": map[string]any{"command": "stop"},
	}
	if err := client.Send("client/command", payload); err != nil {
		ing.logger.Warn("Failed to send stop to upstream Music Assistant", "player_id", playerID, "err", err)
	} else {
		ing.logger.Info("Sent stop to upstream Music Assistant (call ended)", "player_id", playerID)
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
		w.statsMu.Lock()
		w.offeredFormats = offeredFmtStrings
		w.exposedCodecs = supportCodecs
		w.statsMu.Unlock()

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

		w.statsMu.Lock()
		w.connected = true
		w.currentCodec = primaryCodec
		w.currentRate = primaryRate
		w.currentChannels = primaryChannels
		w.currentBitDepth = primaryBitDepth
		w.offeredFormats = offeredFmtStrings
		w.statsMu.Unlock()

		w.setClient(client)
		w.sendSynchronizedState(client)
		w.statsMu.RLock()
		initVol := w.currentVolume
		w.statsMu.RUnlock()
		ing.logger.Info("Player client successfully connected to Music Assistant", "player_id", w.cfg.ID, "volume", initVol)

		var currentMeta domain.StreamMetadata
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
				w.statsMu.Lock()
				w.connected = false
				w.statsMu.Unlock()
				_ = client.SendGoodbye("shutdown")
				client.Close()
				return
			case <-done:
				timeSyncTicker.Stop()
				w.statsMu.Lock()
				w.connected = false
				w.statsMu.Unlock()
				ing.logger.Warn("Sendspin player disconnected, will reconnect", "player_id", w.cfg.ID)
				break eventLoop

			case <-timeSyncTicker.C:
				_ = client.SendTimeSync(time.Now().UnixMicro())

			case timeResp := <-client.TimeSyncResp:
				t4 := time.Now().UnixMicro()
				w.statsMu.Lock()
				if w.clockSync != nil {
					w.clockSync.ProcessSyncResponse(
						timeResp.ClientTransmitted,
						timeResp.ServerReceived,
						timeResp.ServerTransmitted,
						t4,
					)
				}
				w.statsMu.Unlock()

			case startMsg := <-client.StreamStart:
				// Drain any remaining stale chunks in client.AudioChunks channel from previous stream/seek
				drainChannel(client.AudioChunks)
				if startMsg.Player != nil {
					currentCodec = strings.ToLower(startMsg.Player.Codec)
					if currentCodec == "" {
						currentCodec = "pcm"
					}
					// Normalize the announced format up front.
					currentRate = startMsg.Player.SampleRate
					if currentRate <= 0 {
						currentRate = primaryRate
					}
					currentChannels = domain.NormalizeChannels(startMsg.Player.Channels, primaryChannels)
					currentBitDepth = startMsg.Player.BitDepth
					if currentBitDepth <= 0 {
						currentBitDepth = 16
					}
				}
				w.statsMu.Lock()
				w.currentCodec = currentCodec
				w.currentRate = currentRate
				w.currentChannels = currentChannels
				w.currentBitDepth = currentBitDepth
				w.statsMu.Unlock()

				currentMeta.ProgressUpdated = time.Now()
				w.statsMu.Lock()
				w.currentMeta = currentMeta
				w.statsMu.Unlock()

				ing.logger.Info("Sendspin stream start received",
					"player_id", w.cfg.ID,
					"codec", currentCodec,
					"sample_rate", currentRate,
					"channels", currentChannels,
					"bit_depth", currentBitDepth,
					"title", currentMeta.Title,
					"progress_ms", currentMeta.ProgressMs,
				)
				w.sendSynchronizedState(client)
				w.handler.OnStreamStart(w.cfg.ID, currentMeta)

			case <-client.StreamEnd:
				ing.logger.Info("Sendspin stream end received", "player_id", w.cfg.ID)
				w.statsMu.Lock()
				w.currentMeta.ProgressMs = 0
				w.currentMeta.ProgressUpdated = time.Time{}
				w.statsMu.Unlock()
				w.handler.OnStreamEnd(w.cfg.ID)

			case <-client.StreamClear:
				ing.logger.Debug("Sendspin stream clear received (flushing buffer)", "player_id", w.cfg.ID)
				drainChannel(client.AudioChunks)
				w.handler.OnStreamClear(w.cfg.ID)

			case stateMsg := <-client.ServerState:
				if stateMsg.Metadata != nil {
					meta, pbState := parseServerMetadata(stateMsg.Metadata)
					currentMeta = meta
					w.statsMu.Lock()
					w.currentMeta = currentMeta
					w.statsMu.Unlock()
					w.handler.OnMetadata(w.cfg.ID, currentMeta)

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
				w.statsMu.Lock()
				w.chunksReceived++
				w.bytesReceived += uint64(len(chunk.Data))
				w.statsMu.Unlock()

				playAt := resolvePlayAt(w, chunk.Timestamp)
				audioChunk := decodeIncomingAudioChunk(
					chunk, currentCodec, currentRate, currentChannels, currentBitDepth, playAt,
				)
				w.handler.OnAudioChunk(w.cfg.ID, audioChunk)
			}
		}

		w.statsMu.Lock()
		w.connected = false
		w.statsMu.Unlock()
		client.Close()
		consecutiveFails++
		if consecutiveFails >= 3 && strings.EqualFold(ing.config.Server, "auto") {
			ing.clearServerAddr(serverAddr)
		}
		time.Sleep(2 * time.Second)
	}
}

func resolvePlayAt(w *playerWorker, serverTS int64) time.Time {
	if serverTS <= 0 {
		return time.Now()
	}
	w.statsMu.RLock()
	cs := w.clockSync
	w.statsMu.RUnlock()

	if cs != nil {
		return cs.ServerToLocalTime(serverTS)
	}

	return time.Now()
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
		Connected:      w.connected,
		Codec:          w.currentCodec,
		SampleRate:     w.currentRate,
		Channels:       w.currentChannels,
		BitDepth:       w.currentBitDepth,
		OfferedFormats: w.offeredFormats,
		ExposedCodecs:  w.exposedCodecs,
		Metadata:       w.currentMeta,
		ChunksReceived: w.chunksReceived,
		BytesReceived:  w.bytesReceived,
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
