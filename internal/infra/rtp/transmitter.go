package rtp

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/miguelangel-nubla/sendspin-voip/internal/domain"
	"github.com/pion/rtp"
)

// Transmitter coordinates the 20ms playout timing, RFC 3550 packetization,
// and UDP transmission for an active RTP audio session.
type Transmitter struct {
	mu         sync.Mutex
	logger     *slog.Logger
	codec      domain.Codec
	audioPath  *AudioPath
	conn       *net.UDPConn
	localPort  int
	remoteAddr *net.UDPAddr

	// RFC 3550 RTP state
	ssrc        uint32
	sequenceNum uint16
	timestamp   uint32
	firstPkt    bool
	packetsSent uint64
	bytesSent   uint64

	// Playout loop state
	answered    bool
	loopRunning bool
	stopped     bool
	stopChan    chan struct{}
	stopOnce    sync.Once
	wg          sync.WaitGroup
}

// NewTransmitter allocates a local UDP socket on an available even port in the given range
// and initializes the timed RTP transmitter.
func NewTransmitter(
	logger *slog.Logger,
	audioPath *AudioPath,
	codec domain.Codec,
	minPort, maxPort int,
) (*Transmitter, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if minPort <= 0 {
		minPort = 10000
	}
	if maxPort <= 0 || maxPort < minPort {
		maxPort = 20000
	}

	var conn *net.UDPConn
	var localPort int

	for port := minPort; port <= maxPort; port += 2 {
		addr := &net.UDPAddr{IP: net.ParseIP("0.0.0.0"), Port: port}
		c, err := net.ListenUDP("udp", addr)
		if err == nil {
			conn = c
			localPort = port
			break
		}
	}

	if conn == nil {
		return nil, fmt.Errorf("failed to bind UDP port in range [%d, %d]", minPort, maxPort)
	}

	var ssrcBytes [4]byte
	_, _ = rand.Read(ssrcBytes[:])
	ssrc := binary.BigEndian.Uint32(ssrcBytes[:])

	var seqBytes [2]byte
	_, _ = rand.Read(seqBytes[:])
	seq := binary.BigEndian.Uint16(seqBytes[:])

	return &Transmitter{
		logger:      logger,
		codec:       codec,
		audioPath:   audioPath,
		conn:        conn,
		localPort:   localPort,
		ssrc:        ssrc,
		sequenceNum: seq,
		firstPkt:    true,
		stopChan:    make(chan struct{}),
	}, nil
}

// LocalPort returns the bound local UDP port.
func (t *Transmitter) LocalPort() int {
	return t.localPort
}

// SetRemoteAddr configures the destination address for outgoing RTP packets.
func (t *Transmitter) SetRemoteAddr(addr *net.UDPAddr) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.remoteAddr = addr
}

// SetCodec updates the transmission codec.
func (t *Transmitter) SetCodec(codec domain.Codec) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.codec = codec
	t.firstPkt = true
}

// SetAnswered marks the call as answered and starts the 20ms pacing loop.
func (t *Transmitter) SetAnswered(answered bool) {
	t.mu.Lock()
	if t.stopped || t.answered == answered {
		t.mu.Unlock()
		return
	}
	t.answered = answered
	t.firstPkt = true
	shouldStart := answered && !t.loopRunning
	if shouldStart {
		t.loopRunning = true
	}
	t.mu.Unlock()

	if shouldStart {
		t.wg.Add(1)
		go t.pacingLoop()
	}
}

// ResetMarker triggers the RTP marker bit on the next outgoing packet.
func (t *Transmitter) ResetMarker() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.firstPkt = true
}

// Stats returns runtime transmission statistics.
func (t *Transmitter) Stats() (packetsSent, bytesSent uint64, localPort int, remoteAddr string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	remoteStr := ""
	if t.remoteAddr != nil {
		remoteStr = t.remoteAddr.String()
	}
	return t.packetsSent, t.bytesSent, t.localPort, remoteStr
}

// pacingLoop executes every 20ms to release ready audio frames to the network.
func (t *Transmitter) pacingLoop() {
	defer t.wg.Done()

	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-t.stopChan:
			return
		case <-ticker.C:
			t.step()
		}
	}
}

// step executes a single 20ms playout tick.
func (t *Transmitter) step() {
	t.mu.Lock()
	answered := t.answered
	codec := t.codec
	remoteAddr := t.remoteAddr
	isMarker := t.firstPkt
	t.mu.Unlock()

	if !answered || remoteAddr == nil {
		return
	}

	now := time.Now()

	// 1. Discard stale raw upstream chunks and stale ready frames before conversion (> 150ms behind).
	staleCutoff := now.Add(-150 * time.Millisecond)
	t.audioPath.DiscardBefore(staleCutoff)

	// 2. Transcode up to 35 frames (700ms ahead-of-time)
	_ = t.audioPath.Fill(35)

	// 3. Check scheduled playback time (Sendspin clock synchronization)
	readyFrame, ok := t.audioPath.PeekReady()
	if !ok {
		return
	}
	if !readyFrame.PlayAt.IsZero() && now.Before(readyFrame.PlayAt) {
		return
	}

	// 4. Pop the ready 20ms frame
	frame, ok := t.audioPath.PopReady()
	if !ok {
		return
	}

	// 5. Build RFC 3550 RTP packet and transmit over UDP
	samplesPerPacket := uint32((codec.RTPClockRate() * 20) / 1000)
	if err := t.sendPacket(frame.Payload, codec.PayloadType(), samplesPerPacket, isMarker); err != nil {
		t.logger.Debug("RTP transmit error", "err", err)
	}

	if isMarker {
		t.mu.Lock()
		t.firstPkt = false
		t.mu.Unlock()
	}
}

// sendPacket packages and transmits an RFC 3550 RTP packet over UDP.
func (t *Transmitter) sendPacket(payload []byte, payloadType uint8, samplesPerPacket uint32, marker bool) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.stopped || t.conn == nil || t.remoteAddr == nil {
		return nil
	}

	pkt := &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    payloadType,
			SequenceNumber: t.sequenceNum,
			Timestamp:      t.timestamp,
			SSRC:           t.ssrc,
			Marker:         marker,
		},
		Payload: payload,
	}

	raw, err := pkt.Marshal()
	if err != nil {
		return fmt.Errorf("failed to marshal RTP packet: %w", err)
	}

	n, err := t.conn.WriteTo(raw, t.remoteAddr)
	if err != nil {
		return fmt.Errorf("failed to send RTP packet to %s: %w", t.remoteAddr, err)
	}

	t.sequenceNum++
	t.timestamp += samplesPerPacket
	t.packetsSent++
	t.bytesSent += uint64(n)

	return nil
}

// DrainAndClose stops playout, waits for optional drain delay, and closes the socket.
func (t *Transmitter) DrainAndClose(drainDelay time.Duration) error {
	t.stopOnce.Do(func() {
		t.mu.Lock()
		t.stopped = true
		t.mu.Unlock()
		close(t.stopChan)
	})
	t.wg.Wait()

	if drainDelay > 0 {
		time.Sleep(drainDelay)
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if t.conn != nil {
		err := t.conn.Close()
		t.conn = nil
		return err
	}
	return nil
}
