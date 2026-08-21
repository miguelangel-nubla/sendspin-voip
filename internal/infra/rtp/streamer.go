package rtp

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/pion/rtp"
	"github.com/miguelangel-nubla/sendspin-voip/internal/app"
	"github.com/miguelangel-nubla/sendspin-voip/internal/domain"
)

// Streamer implements app.RTPStreamerPort.
type Streamer struct {
	logger     *slog.Logger
	transcoder app.AudioTranscoderPort
	portMin    int
	portMax    int
	mu         sync.Mutex
	lastPort   int
}

// NewStreamer creates a new RTP streamer manager.
func NewStreamer(logger *slog.Logger, transcoder app.AudioTranscoderPort, portMin, portMax int) *Streamer {
	if logger == nil {
		logger = slog.Default()
	}
	if portMin <= 0 {
		portMin = 10000
	}
	if portMax <= portMin {
		portMax = 20000
	}

	return &Streamer{
		logger:     logger,
		transcoder: transcoder,
		portMin:    portMin,
		portMax:    portMax,
		lastPort:   portMin,
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

	sess := &Session{
		logger:     s.logger,
		transcoder: s.transcoder,
		conn:       conn,
		localPort:  localPort,
		codec:      codec,
		ssrc:       ssrc,
		seq:        1,
		timestamp:  1000,
		audioQueue: make(chan []byte, 1000), // 20ms packet buffer (up to 20s, prevents dropping bursts)
		stopChan:   make(chan struct{}),
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

	packetsSent uint64
	bytesSent   uint64
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
		LocalPort:   s.localPort,
		RemoteAddr:  remoteStr,
		Codec:       s.codec,
		PacketsSent: s.packetsSent,
		BytesSent:   s.bytesSent,
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
	s.codec = codec
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
	s.mu.Lock()
	remote := s.remoteAddr
	seq := s.seq
	ts := s.timestamp
	s.seq++
	s.timestamp += timestampDelta
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
			Marker:         seq == 1,
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

// PushAudio accumulates raw PCM samples or forwards native Opus frames to the pacer queue.
func (s *Session) PushAudio(chunk domain.AudioChunk, volumePercent int) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Direct Opus passthrough: if incoming chunk already has encoded Opus frame
	if len(chunk.OpusData) > 0 && s.codec == domain.CodecOpus {
		select {
		case s.audioQueue <- chunk.OpusData:
		default:
			// Queue full; drop to maintain real-time pacing
		}
		return nil
	}

	// Samples per 20ms frame at source rate (e.g. 48000 * 0.02 * 2 = 1920 interleaved samples)
	frameSamples := (chunk.SampleRate * chunk.Channels * 20) / 1000
	if frameSamples <= 0 {
		frameSamples = 1920
	}

	s.pcmBuffer = append(s.pcmBuffer, chunk.Samples...)

	for len(s.pcmBuffer) >= frameSamples {
		frame := s.pcmBuffer[:frameSamples]
		s.pcmBuffer = s.pcmBuffer[frameSamples:]

		payload, err := s.transcoder.Transcode(
			frame,
			chunk.SampleRate,
			chunk.Channels,
			s.codec,
			volumePercent,
		)
		if err != nil {
			return err
		}

		select {
		case s.audioQueue <- payload:
		default:
			// Queue full; drop to maintain real-time pacing
		}
	}

	return nil
}

// ClearBuffer flushes any pending PCM samples and queued RTP packets.
func (s *Session) ClearBuffer() {
	s.mu.Lock()
	s.pcmBuffer = nil
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
