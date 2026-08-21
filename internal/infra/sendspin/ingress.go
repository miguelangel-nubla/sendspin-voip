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
	"github.com/miguelangel-nubla/sendspin-voip/internal/app"
	"github.com/miguelangel-nubla/sendspin-voip/internal/domain"
	"github.com/pion/opus"
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
	client  *protocol.Client
	cancel  context.CancelFunc
	ctx     context.Context

	statsMu         sync.RWMutex
	connected       bool
	currentCodec    string
	currentRate     int
	currentChannels int
	currentBitDepth int
	offeredFormats  []string
	currentMeta     domain.StreamMetadata
	chunksReceived  uint64
	bytesReceived   uint64
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
	ing.discMgr = mgr

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
			if ing.serverAddr == "" {
				ing.serverAddr = addr
				ing.mu.Unlock()
				select {
				case ing.discoveredCh <- addr:
				default:
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

	// If already registered with identical codecs and connected, skip
	if existing, ok := ing.workers[player.ID]; ok {
		if codecsEqual(existing.codecs, codecs) && existing.client != nil && existing.client.IsConnected() {
			return nil
		}
		existing.cancel()
		if existing.client != nil {
			existing.client.Close()
		}
		delete(ing.workers, player.ID)
	}

	ctx, cancel := context.WithCancel(ing.ctx)
	worker := &playerWorker{
		cfg:     player,
		codecs:  codecs,
		handler: handler,
		cancel:  cancel,
		ctx:     ctx,
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
		if worker.client != nil {
			worker.client.Close()
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

	hasOpus := false
	hasL16 := false
	hasG722 := false
	hasG711 := false

	for _, c := range ordered {
		switch c {
		case domain.CodecOpus:
			hasOpus = true
		case domain.CodecL16:
			hasL16 = true
		case domain.CodecG722:
			hasG722 = true
		case domain.CodecPCMU, domain.CodecPCMA:
			hasG711 = true
		}
	}

	// 1. Opus Full-band (Highest fidelity stereo/mono, zero-transcode)
	if hasOpus {
		addFormat(protocol.AudioFormat{Codec: "opus", SampleRate: 48000, Channels: 2, BitDepth: 16})
		addFormat(protocol.AudioFormat{Codec: "opus", SampleRate: 48000, Channels: 1, BitDepth: 16})
	}

	// 2. L16 Uncompressed PCM (48kHz/44.1kHz Stereo)
	if hasL16 {
		addFormat(protocol.AudioFormat{Codec: "pcm", SampleRate: 48000, Channels: 2, BitDepth: 16})
		addFormat(protocol.AudioFormat{Codec: "pcm", SampleRate: 44100, Channels: 2, BitDepth: 16})
	}

	// 3. Wideband 16kHz Mono (Native G.722 match: 1 channel, 16000Hz)
	if hasG722 {
		addFormat(protocol.AudioFormat{Codec: "pcm", SampleRate: 16000, Channels: 1, BitDepth: 16})
	}

	// 4. Narrowband 8kHz Mono (Native G.711 match: 1 channel, 8000Hz)
	if hasG711 {
		addFormat(protocol.AudioFormat{Codec: "pcm", SampleRate: 8000, Channels: 1, BitDepth: 16})
	}

	// 5. Standard PCM Stereo fallbacks
	addFormat(protocol.AudioFormat{Codec: "pcm", SampleRate: 48000, Channels: 2, BitDepth: 16})
	addFormat(protocol.AudioFormat{Codec: "pcm", SampleRate: 44100, Channels: 2, BitDepth: 16})

	return formats, supportCodecs, supportSampleRates, supportChannels
}

// SendPauseToUpstream sends a pause command to Music Assistant for the given player.
// This is called when the remote SIP phone hangs up, to stop MA from continuing to stream.
func (ing *Ingress) SendPauseToUpstream(playerID string) {
	ing.mu.Lock()
	w, ok := ing.workers[playerID]
	ing.mu.Unlock()
	if !ok || w.client == nil || !w.client.IsConnected() {
		return
	}
	payload := map[string]any{
		"controller": map[string]any{"command": "pause"},
	}
	if err := w.client.Send("client/command", payload); err != nil {
		ing.logger.Warn("Failed to send pause to upstream Music Assistant", "player_id", playerID, "err", err)
	} else {
		ing.logger.Info("Sent pause to upstream Music Assistant (phone hung up)", "player_id", playerID)
	}
}

func (ing *Ingress) runPlayerClient(w *playerWorker) {
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

		client := protocol.NewClient(protocol.Config{
			ServerAddr: serverAddr,
			ClientID:   w.cfg.ID,
			Name:       w.cfg.Name,
			Version:    1,
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
			ing.logger.Warn("Failed to connect to Sendspin server, retrying in 3s", "player_id", w.cfg.ID, "err", err)
			time.Sleep(3 * time.Second)
			continue
		}

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

		var offeredFormats []string
		for _, f := range supportedFormats {
			offeredFormats = append(offeredFormats, fmt.Sprintf("%s %dHz %dch %dbit", strings.ToUpper(f.Codec), f.SampleRate, f.Channels, f.BitDepth))
		}

		w.statsMu.Lock()
		w.connected = true
		w.currentCodec = primaryCodec
		w.currentRate = primaryRate
		w.currentChannels = primaryChannels
		w.currentBitDepth = primaryBitDepth
		w.offeredFormats = offeredFormats
		w.statsMu.Unlock()

		w.client = client
		ing.logger.Info("Player client successfully connected to Music Assistant", "player_id", w.cfg.ID)

		var currentMeta domain.StreamMetadata
		var currentCodec = primaryCodec
		var currentRate = primaryRate
		var currentChannels = primaryChannels
		var currentBitDepth = primaryBitDepth

		opusDecoder, _ := opus.NewDecoderWithOutput(48000, 2)
		pcm16Buf := make([]int16, 5760*2) // up to 120ms frame at 48kHz stereo

		done := client.Done()

	eventLoop:
		for {
			select {
			case <-w.ctx.Done():
				w.statsMu.Lock()
				w.connected = false
				w.statsMu.Unlock()
				client.Close()
				return
			case <-done:
				w.statsMu.Lock()
				w.connected = false
				w.statsMu.Unlock()
				ing.logger.Warn("Sendspin player disconnected, will reconnect", "player_id", w.cfg.ID)
				break eventLoop

			case startMsg := <-client.StreamStart:
				if startMsg.Player != nil {
					currentCodec = strings.ToLower(startMsg.Player.Codec)
					if currentCodec == "" {
						currentCodec = "pcm"
					}
					currentRate = startMsg.Player.SampleRate
					currentChannels = startMsg.Player.Channels
					currentBitDepth = startMsg.Player.BitDepth
				}
				w.statsMu.Lock()
				w.currentCodec = currentCodec
				w.currentRate = currentRate
				w.currentChannels = currentChannels
				w.currentBitDepth = currentBitDepth
				w.statsMu.Unlock()

				ing.logger.Info("Sendspin stream start received",
					"player_id", w.cfg.ID,
					"codec", currentCodec,
					"sample_rate", currentRate,
					"channels", currentChannels,
					"bit_depth", currentBitDepth,
					"title", currentMeta.Title,
				)
				_ = client.SendState(protocol.PlayerState{State: "synchronized", Volume: 100, Muted: false})
				w.handler.OnStreamStart(w.cfg.ID, currentMeta)

			case <-client.StreamEnd:
				ing.logger.Info("Sendspin stream end received", "player_id", w.cfg.ID)
				w.handler.OnStreamEnd(w.cfg.ID)

			case <-client.StreamClear:
				ing.logger.Debug("Sendspin stream clear received (flushing buffer)", "player_id", w.cfg.ID)
				w.handler.OnStreamClear(w.cfg.ID)

			case stateMsg := <-client.ServerState:
				if stateMsg.Metadata != nil {
					currentMeta = domain.StreamMetadata{
						Title:       ptrString(stateMsg.Metadata.Title),
						Artist:      ptrString(stateMsg.Metadata.Artist),
						AlbumArtist: ptrString(stateMsg.Metadata.AlbumArtist),
						Album:       ptrString(stateMsg.Metadata.Album),
						StreamTitle: ptrString(stateMsg.Metadata.Title),
					}
					w.statsMu.Lock()
					w.currentMeta = currentMeta
					w.statsMu.Unlock()

					if stateMsg.Metadata.Progress != nil {
						if stateMsg.Metadata.Progress.PlaybackSpeed == 0 {
							w.handler.OnPlaybackState(w.cfg.ID, "paused")
						} else if stateMsg.Metadata.Progress.PlaybackSpeed > 0 {
							w.handler.OnPlaybackState(w.cfg.ID, "playing")
						}
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
					w.handler.OnVolumeChange(w.cfg.ID, cmd.Volume, false)
				} else if cmd.Command == "mute" {
					w.handler.OnVolumeChange(w.cfg.ID, 0, cmd.Mute)
				}

			case chunk := <-client.AudioChunks:
				w.statsMu.Lock()
				w.chunksReceived++
				w.bytesReceived += uint64(len(chunk.Data))
				w.statsMu.Unlock()

				if currentCodec == "opus" {
					var samples []int32
					n, err := opusDecoder.DecodeToInt16(chunk.Data, pcm16Buf)
					if err == nil && n > 0 {
						samples = make([]int32, n*currentChannels)
						for i := 0; i < len(samples); i++ {
							samples[i] = int32(pcm16Buf[i])
						}
					}
					audioChunk := domain.AudioChunk{
						Timestamp:  chunk.Timestamp,
						PlayAt:     time.Now(),
						OpusData:   chunk.Data,
						Samples:    samples,
						SampleRate: 48000,
						Channels:   currentChannels,
						BitDepth:   16,
					}
					w.handler.OnAudioChunk(w.cfg.ID, audioChunk)
				} else {
					samples := decodePCM(chunk.Data, currentBitDepth)
					audioChunk := domain.AudioChunk{
						Timestamp:  chunk.Timestamp,
						PlayAt:     time.Now(),
						Samples:    samples,
						SampleRate: currentRate,
						Channels:   currentChannels,
						BitDepth:   currentBitDepth,
					}
					w.handler.OnAudioChunk(w.cfg.ID, audioChunk)
				}
			}
		}

		w.statsMu.Lock()
		w.connected = false
		w.statsMu.Unlock()
		client.Close()
		time.Sleep(2 * time.Second)
	}
}

func ptrString(p *string) string {
	if p != nil {
		return *p
	}
	return ""
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
		if w.client != nil {
			w.client.Close()
		}
	}
	if ing.discMgr != nil {
		ing.discMgr.Stop()
	}
	return nil
}
