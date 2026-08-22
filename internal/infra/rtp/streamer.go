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

// CreateSession allocates UDP transport and initializes a session with the 3-stage decoupled audio architecture.
func (s *Streamer) CreateSession(codec domain.Codec) (app.RTPSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	activeCodec := codec
	if activeCodec == "" {
		activeCodec = domain.CodecG722
	}

	// 1. Upstream (raw timeline buffer)
	upstream := NewUpstreamPlayer(0)

	// 2. Audio Path (DSP & conversion engine wrapping Upstream)
	audioPath := NewAudioPath(s.transcoderFactory(), upstream, activeCodec, 100)

	// 3. Transmitter (20ms playout pacing, PlayAt timing sync, RFC 3550 RTP, and UDP socket)
	transmitter, err := NewTransmitter(s.logger, audioPath, activeCodec, s.portMin, s.portMax)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize RTP transmitter: %w", err)
	}

	sess := &Session{
		logger:      s.logger,
		codec:       activeCodec,
		upstream:    upstream,
		audioPath:   audioPath,
		transmitter: transmitter,
	}
	sess.curVolume.Store(100)

	return sess, nil
}

// Session implements app.RTPSession coordinating the 3 decoupled stages.
type Session struct {
	mu        sync.Mutex
	logger    *slog.Logger
	codec     domain.Codec
	answered  bool
	curVolume atomic.Int32

	upstream    *UpstreamPlayer
	audioPath   *AudioPath
	transmitter *Transmitter
}

// LocalPort returns the bound UDP port on Transmitter.
func (s *Session) LocalPort() int {
	return s.transmitter.LocalPort()
}

// SetAnswered marks call as answered in Transmitter.
func (s *Session) SetAnswered(answered bool) {
	s.mu.Lock()
	s.answered = answered
	s.mu.Unlock()

	s.transmitter.SetAnswered(answered)
}

// SetVolume updates volume in AudioPath, rewinding unplayed upstream chunks for re-encoding.
func (s *Session) SetVolume(volumePercent int) {
	s.curVolume.Store(int32(volumePercent))
	s.audioPath.SetVolume(volumePercent)
}

// SetCodec updates target transmission codec across AudioPath and Transmitter.
func (s *Session) SetCodec(codec domain.Codec) {
	s.mu.Lock()
	if codec == "" || codec == s.codec {
		s.mu.Unlock()
		return
	}
	s.codec = codec
	s.mu.Unlock()

	s.audioPath.SetCodec(codec)
	s.transmitter.SetCodec(codec)
}

// StartTransmission configures remote RTP target address and triggers playout.
func (s *Session) StartTransmission(remoteAddr *net.UDPAddr) error {
	s.transmitter.SetRemoteAddr(remoteAddr)
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

// ClearBuffer synchronously resets all layers on seek or stream stop.
func (s *Session) ClearBuffer() {
	s.audioPath.Clear()
	s.transmitter.ResetMarker()
}

// Stats returns comprehensive runtime metrics across all pipeline stages.
func (s *Session) Stats() app.RTPStats {
	s.mu.Lock()
	codec := s.codec
	answered := s.answered
	s.mu.Unlock()

	packetsSent, bytesSent, localPort, remoteStr := s.transmitter.Stats()
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
		Answered:            answered,
	}
}

// DrainAndClose stops playout, drains pending frames, and closes the RTP socket.
func (s *Session) DrainAndClose(drainDelay time.Duration) error {
	return s.transmitter.DrainAndClose(drainDelay)
}
