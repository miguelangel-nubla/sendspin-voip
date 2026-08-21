package rtp

import (
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/miguelangel-nubla/sendspin-voip/internal/app"
	"github.com/miguelangel-nubla/sendspin-voip/internal/domain"
)

// TranscoderFactory creates a fresh audio transcoder for each RTP session.
type TranscoderFactory func() app.AudioTranscoderPort

// Streamer implements app.RTPStreamerPort.
type Streamer struct {
	logger            *slog.Logger
	transcoderFactory TranscoderFactory
	portMin           int
	portMax           int
	mu                sync.Mutex
}

// NewStreamer creates a new RTP streamer manager.
func NewStreamer(logger *slog.Logger, factory TranscoderFactory, portMin, portMax int) *Streamer {
	if logger == nil {
		logger = slog.Default()
	}
	if factory == nil {
		panic("rtp.NewStreamer: transcoder factory is required")
	}
	if portMin <= 0 {
		portMin = 10000
	}
	if portMax <= portMin {
		portMax = 20000
	}

	return &Streamer{
		logger:            logger,
		transcoderFactory: factory,
		portMin:           portMin,
		portMax:           portMax,
	}
}

// CreateSession allocates UDP transport and initializes a session with the 4-layer decoupled audio architecture.
func (s *Streamer) CreateSession(codec domain.Codec) (app.RTPSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	activeCodec := codec
	if activeCodec == "" {
		activeCodec = domain.CodecG722
	}

	// 1. Layer 4: RTP Transport (UDP socket & packetizer)
	transport, err := NewRTPTransport(s.portMin, s.portMax)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize RTP transport: %w", err)
	}

	// 2. Layer 1: Upstream Player (raw timeline buffer)
	upstream := NewUpstreamPlayer(0)

	// 3. Layer 2: Audio Path (DSP & conversion engine wrapping Layer 1)
	audioPath := NewAudioPath(s.transcoderFactory(), upstream, activeCodec, 100)

	// 4. Layer 3: Downstream Player (playout mode & timing policy wrapping Layer 2 & Layer 4)
	downstream := NewDownstreamPlayer(
		s.logger,
		domain.BufferModeAnnouncement,
		activeCodec,
		audioPath,
		transport,
	)

	sess := &Session{
		logger:     s.logger,
		codec:      activeCodec,
		bufferMode: domain.BufferModeAnnouncement,
		upstream:   upstream,
		audioPath:  audioPath,
		downstream: downstream,
		transport:  transport,
	}
	sess.curVolume.Store(100)

	return sess, nil
}

// Session implements app.RTPSession coordinating the 4 decoupled layers.
type Session struct {
	mu         sync.Mutex
	logger     *slog.Logger
	codec      domain.Codec
	bufferMode domain.BufferMode
	answered   bool
	curVolume  atomic.Int32

	upstream   *UpstreamPlayer
	audioPath  *AudioPath
	downstream *DownstreamPlayer
	transport  *RTPTransport
}

// LocalPort returns the bound UDP port on RTPTransport.
func (s *Session) LocalPort() int {
	return s.transport.LocalPort()
}

// SetBufferMode sets playout policy in DownstreamPlayer.
func (s *Session) SetBufferMode(mode domain.BufferMode) {
	s.mu.Lock()
	if mode == "" {
		mode = domain.BufferModeAnnouncement
	}
	s.bufferMode = mode
	s.mu.Unlock()

	s.downstream.SetBufferMode(mode)
}

// SetAnswered marks call as answered in DownstreamPlayer.
func (s *Session) SetAnswered(answered bool) {
	s.mu.Lock()
	s.answered = answered
	s.mu.Unlock()

	s.downstream.SetAnswered(answered)
}

// SetVolume updates volume in AudioPath, rewinding unplayed upstream chunks for re-encoding.
func (s *Session) SetVolume(volumePercent int) {
	s.curVolume.Store(int32(volumePercent))
	s.audioPath.SetVolume(volumePercent)
}

// SetCodec updates target transmission codec across AudioPath, DownstreamPlayer, and RTPTransport.
func (s *Session) SetCodec(codec domain.Codec) {
	s.mu.Lock()
	if codec == "" || codec == s.codec {
		s.mu.Unlock()
		return
	}
	s.codec = codec
	s.mu.Unlock()

	s.audioPath.SetCodec(codec)
	s.downstream.SetCodec(codec)
}

// StartTransmission configures remote RTP target address and triggers playout.
func (s *Session) StartTransmission(remoteAddr *net.UDPAddr) error {
	s.transport.SetRemoteAddr(remoteAddr)
	s.SetAnswered(true)
	return nil
}

// PushAudio pushes incoming raw audio chunk into the UpstreamPlayer timeline.
// Uses lock-free atomic check on volume so WebSocket ingress is never blocked on DSP transcode batches.
func (s *Session) PushAudio(chunk domain.AudioChunk, volumePercent int) error {
	if volumePercent >= 0 {
		old := s.curVolume.Swap(int32(volumePercent))
		if old != int32(volumePercent) {
			s.audioPath.SetVolume(volumePercent)
		}
	}
	s.upstream.Push(chunk)
	return nil
}

// InjectSilence feeds raw PCM silence into UpstreamPlayer to preserve the announcement holdback buffer lag.
func (s *Session) InjectSilence(duration time.Duration) {
	if duration <= 0 {
		return
	}
	// 20ms of stereo 48kHz PCM silence (48000 * 2 * 0.02 = 1920 int32 samples)
	sampleCount := (48000 * 2 * 20) / 1000
	numChunks := int(duration / (20 * time.Millisecond))
	for i := 0; i < numChunks; i++ {
		s.upstream.Push(domain.AudioChunk{
			Samples:    make([]int32, sampleCount),
			SampleRate: 48000,
			Channels:   2,
			BitDepth:   16,
		})
	}
}

// ClearBuffer synchronously resets all layers on seek or stream stop.
func (s *Session) ClearBuffer() {
	s.audioPath.Clear()
	s.downstream.ResetMarker()
}

// Stats returns comprehensive runtime metrics across all 4 layers.
func (s *Session) Stats() app.RTPStats {
	s.mu.Lock()
	codec := s.codec
	mode := s.bufferMode
	answered := s.answered
	s.mu.Unlock()

	packetsSent, bytesSent, localPort, remoteStr := s.transport.Stats()
	apStats := s.audioPath.Stats()
	upChunks := s.upstream.Len()
	upOldest, upNewest, _ := s.upstream.PlayAtBounds()
	readyOldest, readyNewest, _ := s.audioPath.ReadyPlayAtBounds()

	pathMode := apStats.Mode
	if pathMode == "" {
		if !answered {
			pathMode = "buffering"
		} else {
			pathMode = "transcode"
		}
	}

	return app.RTPStats{
		LocalPort:           localPort,
		RemoteAddr:          remoteStr,
		Codec:               codec,
		PacketsSent:         packetsSent,
		BytesSent:           bytesSent,
		PathMode:            pathMode,
		PathSummary:         apStats.Summary,
		PathStages:          apStats.Stages,
		PathVolumePercent:   apStats.VolumePercent,
		PathIngressCodec:    apStats.IngressCodec,
		PathIngressRate:     apStats.IngressRate,
		PathIngressChannels: apStats.IngressChannels,
		PassthroughPackets:  apStats.PassthroughPackets,
		TranscodePackets:    apStats.TranscodePackets,
		UpstreamChunks:      upChunks,
		ConversionQueue:     apStats.ReadyFrames,
		UpstreamPlayAtStart: upOldest,
		UpstreamPlayAtEnd:   upNewest,
		ReadyPlayAtStart:    readyOldest,
		ReadyPlayAtEnd:      readyNewest,
		BufferMode:          mode,
		Answered:            answered,
	}
}

// DrainAndClose stops playout, drains pending frames, and closes the RTP socket.
func (s *Session) DrainAndClose(drainDelay time.Duration) error {
	s.downstream.Stop()

	if drainDelay > 0 {
		time.Sleep(drainDelay)
	}

	return s.transport.Close()
}
