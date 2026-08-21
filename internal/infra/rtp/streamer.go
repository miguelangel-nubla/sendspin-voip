package rtp

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/miguelangel-nubla/sendspin-voip/internal/app"
	"github.com/miguelangel-nubla/sendspin-voip/internal/domain"
	"github.com/pion/rtp"
)

// TranscoderFactory creates a fresh audio transcoder for each RTP session.
// G.722 (and similar) encoders are stateful; sessions must not share them.
type TranscoderFactory func() app.AudioTranscoderPort

// Streamer implements app.RTPStreamerPort.
type Streamer struct {
	logger            *slog.Logger
	transcoderFactory TranscoderFactory
	portMin           int
	portMax           int
	mu                sync.Mutex
	lastPort          int
}

// NewStreamer creates a new RTP streamer manager.
// factory must return a new transcoder instance per call (never a shared stateful encoder).
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
		lastPort:          portMin,
	}
}

// CreateSession binds an available UDP port and initializes an RTP session.
func (s *Streamer) CreateSession(codec domain.Codec) (app.RTPSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Find available UDP port in range (even port preferred for RTP)
	var conn *net.UDPConn
	var localPort int

	for attempts := 0; attempts < 100; attempts++ {
		s.lastPort += 2
		if s.lastPort < s.portMin || s.lastPort >= s.portMax {
			s.lastPort = s.portMin
		}
		if s.lastPort%2 != 0 {
			s.lastPort++
		}

		addr := &net.UDPAddr{IP: net.IPv4zero, Port: s.lastPort}
		c, err := net.ListenUDP("udp", addr)
		if err == nil {
			conn = c
			localPort = s.lastPort
			break
		}
	}

	if conn == nil {
		// Fallback to OS-assigned ephemeral port
		addr := &net.UDPAddr{IP: net.IPv4zero, Port: 0}
		c, err := net.ListenUDP("udp", addr)
		if err != nil {
			return nil, fmt.Errorf("failed to bind RTP UDP socket: %w", err)
		}
		conn = c
		localPort = c.LocalAddr().(*net.UDPAddr).Port
	}

	var ssrcBytes [4]byte
	_, _ = rand.Read(ssrcBytes[:])
	ssrc := binary.BigEndian.Uint32(ssrcBytes[:])

	activeCodec := codec
	if activeCodec == "" {
		activeCodec = domain.CodecG722
	}

	sess := &Session{
		logger:                    s.logger,
		transcoder:                s.transcoderFactory(),
		conn:                      conn,
		localPort:                 localPort,
		codec:                     activeCodec,
		ssrc:                      ssrc,
		seq:                       1,
		timestamp:                 1000,
		audioQueue:                make(chan []byte, 125), // ~2.5s @ 20ms; enough for announcement flush, responsive to volume
		stopChan:                  make(chan struct{}),
		isFirstPacketAfterSilence: true,
	}

	return sess, nil
}

// Session implements app.RTPSession.
type Session struct {
	logger     *slog.Logger
	transcoder app.AudioTranscoderPort
	conn       *net.UDPConn
	localPort  int
	codec      domain.Codec
	ssrc       uint32
	seq        uint16
	timestamp  uint32

	remoteAddr *net.UDPAddr
	audioQueue chan []byte
	stopChan   chan struct{}
	stopOnce   sync.Once
	wg         sync.WaitGroup
	mu         sync.Mutex

	// Sample accumulator for fixed 20ms frame slicing
	pcmBuffer []int32

	// Silence and jitter tracking
	lastPacketSentTime        time.Time
	isFirstPacketAfterSilence bool

	packetsSent uint64
	bytesSent   uint64

	// Audio path telemetry (last PushAudio decision)
	pathMode            string
	pathSummary         string
	pathStages          []string
	pathVolumePercent   int
	pathIngressCodec    string
	pathIngressRate     int
	pathIngressChannels int
	passthroughPackets  uint64
	transcodePackets    uint64
}

func (s *Session) LocalPort() int {
	return s.localPort
}

func (s *Session) Stats() app.RTPStats {
	s.mu.Lock()
	defer s.mu.Unlock()

	remoteStr := ""
	if s.remoteAddr != nil {
		remoteStr = s.remoteAddr.String()
	}

	return app.RTPStats{
		LocalPort:           s.localPort,
		RemoteAddr:          remoteStr,
		Codec:               s.codec,
		PacketsSent:         s.packetsSent,
		BytesSent:           s.bytesSent,
		PathMode:            s.pathMode,
		PathSummary:         s.pathSummary,
		PathStages:          append([]string(nil), s.pathStages...),
		PathVolumePercent:   s.pathVolumePercent,
		PathIngressCodec:    s.pathIngressCodec,
		PathIngressRate:     s.pathIngressRate,
		PathIngressChannels: s.pathIngressChannels,
		PassthroughPackets:  s.passthroughPackets,
		TranscodePackets:    s.transcodePackets,
	}
}

func (s *Session) StartTransmission(remoteAddr *net.UDPAddr) error {
	s.mu.Lock()
	s.remoteAddr = remoteAddr
	s.mu.Unlock()

	s.wg.Add(1)
	go s.pacingLoop()
	return nil
}

// SetCodec updates the active transmission codec.
func (s *Session) SetCodec(codec domain.Codec) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if codec == "" || codec == s.codec {
		return
	}
	s.codec = codec
	s.pcmBuffer = nil
	s.isFirstPacketAfterSilence = true
	if resetter, ok := s.transcoder.(interface{ Reset() }); ok {
		resetter.Reset()
	}
}

// 20ms packet pacer ticker loop (50 packets per second).
func (s *Session) pacingLoop() {
	defer s.wg.Done()

	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-s.stopChan:
			return
		case <-ticker.C:
			s.mu.Lock()
			codec := s.codec
			s.mu.Unlock()
			samplesPerPacket := (codec.RTPClockRate() * 20) / 1000

			select {
			case payload := <-s.audioQueue:
				s.sendRTPPacket(payload, samplesPerPacket, codec)
			default:
				// Silence or idle interval
			}
		}
	}
}

func (s *Session) sendRTPPacket(payload []byte, timestampDelta uint32, codec domain.Codec) {
	now := time.Now()

	s.mu.Lock()
	remote := s.remoteAddr
	seq := s.seq
	isFirst := s.isFirstPacketAfterSilence

	// If there was an idle/silence gap > 60ms between packets, align timestamp to elapsed wall-clock time
	if !isFirst && !s.lastPacketSentTime.IsZero() {
		elapsed := now.Sub(s.lastPacketSentTime)
		if elapsed > 60*time.Millisecond {
			isFirst = true
			clockRate := uint32(codec.RTPClockRate())
			elapsedTicks := uint32(elapsed.Seconds() * float64(clockRate))
			s.timestamp += elapsedTicks
		} else {
			s.timestamp += timestampDelta
		}
	} else if isFirst && !s.lastPacketSentTime.IsZero() {
		elapsed := now.Sub(s.lastPacketSentTime)
		if elapsed > 0 {
			clockRate := uint32(codec.RTPClockRate())
			elapsedTicks := uint32(elapsed.Seconds() * float64(clockRate))
			s.timestamp += elapsedTicks
		}
	}

	ts := s.timestamp
	s.lastPacketSentTime = now
	s.isFirstPacketAfterSilence = false
	s.seq++
	s.mu.Unlock()

	if remote == nil || len(payload) == 0 {
		return
	}

	pkt := &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    codec.PayloadType(),
			SequenceNumber: seq,
			Timestamp:      ts,
			SSRC:           s.ssrc,
			Marker:         isFirst || seq == 1,
		},
		Payload: payload,
	}

	raw, err := pkt.Marshal()
	if err != nil {
		return
	}

	n, err := s.conn.WriteToUDP(raw, remote)
	if err == nil && n > 0 {
		s.mu.Lock()
		s.packetsSent++
		s.bytesSent += uint64(n)
		s.mu.Unlock()
	}
}

// opusPCMDecoder is optionally implemented by the transcoder for Opus volume re-encode.
type opusPCMDecoder interface {
	DecodeOpusToPCM(opusData []byte, channels int) ([]int32, error)
}

// PushAudio accumulates raw PCM samples or forwards native Opus frames to the pacer queue.
// Opus passthrough is used only at full volume; otherwise decoded PCM is gain-adjusted and
// re-encoded via the transcoder so volume/mute always apply.
func (s *Session) PushAudio(chunk domain.AudioChunk, volumePercent int) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if volumePercent > 100 {
		volumePercent = 100
	}
	if volumePercent < 0 {
		volumePercent = 0
	}

	ingressCodec := "pcm"
	if len(chunk.OpusData) > 0 {
		ingressCodec = "opus"
	}
	ingressRate := chunk.SampleRate
	ingressCh := chunk.Channels
	if ingressRate <= 0 {
		ingressRate = 48000
	}
	if ingressCh <= 0 {
		ingressCh = 2
	}

	// Direct Opus passthrough only when volume is unity (no gain stage needed)
	if len(chunk.OpusData) > 0 && s.codec == domain.CodecOpus && volumePercent >= 100 {
		s.recordPath(pathSnapshot{
			mode:            "opus_passthrough",
			volume:          volumePercent,
			ingressCodec:    ingressCodec,
			ingressRate:     ingressRate,
			ingressChannels: ingressCh,
			decodedLocally:  false,
			passthrough:     true,
		})
		select {
		case s.audioQueue <- chunk.OpusData:
			s.passthroughPackets++
		default:
			// Queue full; drop to maintain real-time pacing
		}
		return nil
	}

	samples := chunk.Samples
	sampleRate := chunk.SampleRate
	channels := chunk.Channels
	decodedLocally := false

	// Any non-passthrough path needs PCM. If ingress sent Opus without usable samples
	// (decode miss upstream), decode locally for every egress codec — not only Opus.
	if len(samples) == 0 && len(chunk.OpusData) > 0 {
		if dec, ok := s.transcoder.(opusPCMDecoder); ok {
			decoded, err := dec.DecodeOpusToPCM(chunk.OpusData, channels)
			if err != nil {
				return err
			}
			samples = decoded
			sampleRate = 48000
			if channels != 1 && channels != 2 {
				channels = 2
			}
			decodedLocally = true
		}
	}

	if len(samples) == 0 {
		return nil
	}
	if sampleRate <= 0 {
		sampleRate = 48000
	}
	if channels <= 0 {
		channels = 2
	}

	s.recordPath(pathSnapshot{
		mode:            "transcode",
		volume:          volumePercent,
		ingressCodec:    ingressCodec,
		ingressRate:     sampleRate,
		ingressChannels: channels,
		decodedLocally:  decodedLocally || (ingressCodec == "opus" && len(chunk.Samples) > 0),
		passthrough:     false,
		egressChannels:  channels,
	})

	// Samples per 20ms frame at source rate (e.g. 48000 * 0.02 * 2 = 1920 interleaved samples)
	frameSamples := (sampleRate * channels * 20) / 1000
	if frameSamples <= 0 {
		frameSamples = 1920
	}

	s.pcmBuffer = append(s.pcmBuffer, samples...)

	for len(s.pcmBuffer) >= frameSamples {
		frame := s.pcmBuffer[:frameSamples]
		s.pcmBuffer = s.pcmBuffer[frameSamples:]

		payload, err := s.transcoder.Transcode(
			frame,
			sampleRate,
			channels,
			s.codec,
			volumePercent,
		)
		if err != nil {
			return err
		}
		if len(payload) == 0 {
			continue // Opus may buffer until a full encode frame is ready
		}

		select {
		case s.audioQueue <- payload:
			s.transcodePackets++
		default:
			// Queue full; drop to maintain real-time pacing
		}
	}

	return nil
}

type pathSnapshot struct {
	mode            string
	volume          int
	ingressCodec    string
	ingressRate     int
	ingressChannels int
	decodedLocally  bool
	passthrough     bool
	egressChannels  int // 0 = derive from codec defaults
}

func (s *Session) recordPath(p pathSnapshot) {
	s.pathMode = p.mode
	s.pathVolumePercent = p.volume
	s.pathIngressCodec = p.ingressCodec
	s.pathIngressRate = p.ingressRate
	s.pathIngressChannels = p.ingressChannels

	outCh := p.egressChannels
	if s.codec != domain.CodecOpus {
		outCh = 1 // G.711 / G.722 / L16 are mono
	} else if outCh <= 0 {
		outCh = p.ingressChannels
		if outCh != 1 && outCh != 2 {
			outCh = 2
		}
	}

	inLabel := fmt.Sprintf("%s %dHz %dch", strings.ToUpper(p.ingressCodec), p.ingressRate, p.ingressChannels)
	outLabel := fmt.Sprintf("%s %dHz %dch", strings.ToUpper(string(s.codec)), s.codec.SampleRate(), outCh)
	volLabel := volumeStageLabel(p.volume)

	var stages []string
	stages = append(stages, "ingress "+inLabel)

	if p.passthrough {
		stages = append(stages, "passthrough (no decode/encode, "+volLabel+")")
		stages = append(stages, "RTP "+outLabel)
		s.pathSummary = inLabel + " → passthrough → RTP " + strings.ToUpper(string(s.codec))
	} else {
		if p.ingressCodec == "opus" || p.decodedLocally {
			if p.decodedLocally {
				stages = append(stages, "decode Opus → PCM")
			} else {
				stages = append(stages, "PCM from ingress Opus decode")
			}
		}
		// Mono-only codecs always downmix; Opus keeps stereo
		if s.codec != domain.CodecOpus && p.ingressChannels > 1 {
			stages = append(stages, "downmix → mono")
		}
		stages = append(stages, volLabel)
		if s.codec == domain.CodecOpus {
			if p.ingressRate != 48000 {
				stages = append(stages, fmt.Sprintf("resample %dHz → 48000Hz", p.ingressRate))
			}
			chLabel := "mono"
			if outCh == 2 {
				chLabel = "stereo"
			}
			stages = append(stages, "encode OPUS ("+chLabel+")")
		} else {
			if p.ingressRate != s.codec.SampleRate() {
				stages = append(stages, fmt.Sprintf("resample %dHz → %dHz", p.ingressRate, s.codec.SampleRate()))
			}
			stages = append(stages, "encode "+strings.ToUpper(string(s.codec))+" (mono)")
		}
		stages = append(stages, "RTP "+outLabel)
		s.pathSummary = inLabel + " → transcode (" + volLabel + ") → RTP " + strings.ToUpper(string(s.codec))
	}
	s.pathStages = stages
}

func volumeStageLabel(volumePercent int) string {
	if volumePercent <= 0 {
		return "volume mute (silence)"
	}
	if volumePercent >= 100 {
		return "volume 100% (0 dB)"
	}
	// Match transcoder −60 dB … 0 dB taper
	db := (float64(volumePercent)/100.0)*60.0 - 60.0
	return fmt.Sprintf("volume %d%% (%.0f dB)", volumePercent, db)
}

// ClearBuffer flushes any pending PCM samples and queued RTP packets.
func (s *Session) ClearBuffer() {
	s.mu.Lock()
	s.pcmBuffer = nil
	s.isFirstPacketAfterSilence = true
	if resetter, ok := s.transcoder.(interface{ Reset() }); ok {
		resetter.Reset()
	}
	s.mu.Unlock()

	// Drain all pending packets from audioQueue
	for {
		select {
		case <-s.audioQueue:
		default:
			return
		}
	}
}

// DrainAndClose waits for drainDelay to allow the remote jitter buffer to play out, then closes the socket.
func (s *Session) DrainAndClose(drainDelay time.Duration) error {
	s.stopOnce.Do(func() {
		if drainDelay > 0 {
			time.Sleep(drainDelay)
		}
		close(s.stopChan)
		s.wg.Wait()
		if s.conn != nil {
			_ = s.conn.Close()
		}
	})
	return nil
}
