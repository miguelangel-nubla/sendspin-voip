package sendspin

import (
	"context"
	"encoding/binary"
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
	handler app.PlayerEventHandler
	client  *protocol.Client
	cancel  context.CancelFunc
	ctx     context.Context
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
func (ing *Ingress) RegisterPlayer(player domain.PlayerConfig, handler app.PlayerEventHandler) error {
	ing.mu.Lock()
	defer ing.mu.Unlock()

	ctx, cancel := context.WithCancel(ing.ctx)
	worker := &playerWorker{
		cfg:     player,
		handler: handler,
		cancel:  cancel,
		ctx:     ctx,
	}
	ing.workers[player.ID] = worker

	go ing.runPlayerClient(worker)
	return nil
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

		var supportedFormats []protocol.AudioFormat
		var supportCodecs []string
		var supportSampleRates []int
		var supportChannels []int

		switch w.cfg.Codec {
		case domain.CodecOpus:
			supportedFormats = []protocol.AudioFormat{
				{
					Codec:      "opus",
					SampleRate: 48000,
					Channels:   2,
					BitDepth:   16,
				},
				{
					Codec:      "pcm",
					SampleRate: 48000,
					Channels:   2,
					BitDepth:   16,
				},
			}
			supportCodecs = []string{"opus", "pcm"}
			supportSampleRates = []int{48000, 44100}
			supportChannels = []int{2, 1}

		case domain.CodecG722:
			// Advertise 16kHz mono as preferred (Music Assistant resamples & downmixes at source!)
			supportedFormats = []protocol.AudioFormat{
				{
					Codec:      "pcm",
					SampleRate: 16000,
					Channels:   1,
					BitDepth:   16,
				},
				{
					Codec:      "pcm",
					SampleRate: 48000,
					Channels:   2,
					BitDepth:   16,
				},
				{
					Codec:      "pcm",
					SampleRate: 44100,
					Channels:   2,
					BitDepth:   16,
				},
			}
			supportCodecs = []string{"pcm"}
			supportSampleRates = []int{16000, 48000, 44100}
			supportChannels = []int{1, 2}

		case domain.CodecPCMU, domain.CodecPCMA:
			// Advertise 8kHz mono as preferred (Music Assistant resamples & downmixes at source!)
			supportedFormats = []protocol.AudioFormat{
				{
					Codec:      "pcm",
					SampleRate: 8000,
					Channels:   1,
					BitDepth:   16,
				},
				{
					Codec:      "pcm",
					SampleRate: 48000,
					Channels:   2,
					BitDepth:   16,
				},
				{
					Codec:      "pcm",
					SampleRate: 44100,
					Channels:   2,
					BitDepth:   16,
				},
			}
			supportCodecs = []string{"pcm"}
			supportSampleRates = []int{8000, 48000, 44100}
			supportChannels = []int{1, 2}

		default:
			supportedFormats = []protocol.AudioFormat{
				{
					Codec:      "pcm",
					SampleRate: 48000,
					Channels:   2,
					BitDepth:   16,
				},
				{
					Codec:      "pcm",
					SampleRate: 44100,
					Channels:   2,
					BitDepth:   16,
				},
			}
			supportCodecs = []string{"pcm"}
			supportSampleRates = []int{48000, 44100}
			supportChannels = []int{2, 1}
		}

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

		w.client = client
		ing.logger.Info("Player client successfully connected to Music Assistant", "player_id", w.cfg.ID)

		var currentMeta domain.StreamMetadata
		var currentCodec = "pcm"
		var currentRate = 48000
		var currentChannels = 2
		var currentBitDepth = 16

		opusDecoder, _ := opus.NewDecoderWithOutput(48000, 2)
		pcm16Buf := make([]int16, 5760*2) // up to 120ms frame at 48kHz stereo

		done := client.Done()

	eventLoop:
		for {
			select {
			case <-w.ctx.Done():
				client.Close()
				return
			case <-done:
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
				if currentCodec == "opus" {
					if w.cfg.Codec == domain.CodecOpus {
						// Passthrough Opus frame directly
						audioChunk := domain.AudioChunk{
							Timestamp:  chunk.Timestamp,
							PlayAt:     time.Now(),
							OpusData:   chunk.Data,
							SampleRate: 48000,
							Channels:   currentChannels,
							BitDepth:   16,
						}
						w.handler.OnAudioChunk(w.cfg.ID, audioChunk)
					} else {
						// Decode Opus to PCM using pure-Go pion/opus for transcoding to G.722/G.711
						n, err := opusDecoder.DecodeToInt16(chunk.Data, pcm16Buf)
						if err == nil && n > 0 {
							samples := make([]int32, n*currentChannels)
							for i := 0; i < len(samples); i++ {
								samples[i] = int32(pcm16Buf[i])
							}
							audioChunk := domain.AudioChunk{
								Timestamp:  chunk.Timestamp,
								PlayAt:     time.Now(),
								Samples:    samples,
								SampleRate: 48000,
								Channels:   currentChannels,
								BitDepth:   16,
							}
							w.handler.OnAudioChunk(w.cfg.ID, audioChunk)
						}
					}
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
