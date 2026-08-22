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
	portPool   *PortPool
	remoteAddr *net.UDPAddr

	// RFC 3550 RTP state
	ssrc        uint32
	sequenceNum uint16
	timestamp   uint32
	firstPkt    bool
	packetsSent uint64
	bytesSent   uint64

	// RFC 3550 RTCP feedback & RFC 2833 DTMF state
	dtmfHandler        func(digit string)
	lastDTMFTimestamp  uint32
	hasLastDTMF        bool
	remoteJitterMs     float64
	remoteFractionLost float64
	remoteRTTMs        float64
	lastSRCompactNTP   uint32
	lastSRTime         time.Time
	rtcpTickCounter    int
	idleTickCounter    int

	// Playout loop state
	answered    bool
	loopRunning bool
	stopped     bool
	stopChan    chan struct{}
	stopOnce    sync.Once
	wg          sync.WaitGroup
}

// NewTransmitter allocates a local UDP socket on an available even port from the port pool
// and initializes the timed RTP transmitter.
func NewTransmitter(
	logger *slog.Logger,
	audioPath *AudioPath,
	codec domain.Codec,
	portPool *PortPool,
) (*Transmitter, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if portPool == nil {
		portPool = NewPortPool(10000, 20000)
	}

	conn, localPort, err := portPool.Allocate()
	if err != nil {
		return nil, fmt.Errorf("failed to allocate RTP UDP port: %w", err)
	}

	var ssrcBytes [4]byte
	_, _ = rand.Read(ssrcBytes[:])
	ssrc := binary.BigEndian.Uint32(ssrcBytes[:])

	var seqBytes [2]byte
	_, _ = rand.Read(seqBytes[:])
	seq := binary.BigEndian.Uint16(seqBytes[:])

	tx := &Transmitter{
		logger:      logger,
		codec:       codec,
		audioPath:   audioPath,
		conn:        conn,
		localPort:   localPort,
		portPool:    portPool,
		ssrc:        ssrc,
		sequenceNum: seq,
		firstPkt:    true,
		stopChan:    make(chan struct{}),
	}

	tx.wg.Add(1)
	go tx.readLoop()

	return tx, nil
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

// SetDTMFHandler registers a callback invoked when DTMF inband telephone-events arrive.
func (t *Transmitter) SetDTMFHandler(handler func(digit string)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.dtmfHandler = handler
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
		t.wg.Add(1)
		go t.pacingLoop()
	}
	t.mu.Unlock()
}

// ResetMarker triggers the RTP marker bit on the next outgoing packet.
func (t *Transmitter) ResetMarker() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.firstPkt = true
}

// Stats returns runtime transmission statistics including RTCP feedback.
func (t *Transmitter) Stats() (packetsSent, bytesSent uint64, localPort int, remoteAddr string, jitterMs, fractionLost, rttMs float64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	remoteStr := ""
	if t.remoteAddr != nil {
		remoteStr = t.remoteAddr.String()
	}
	return t.packetsSent, t.bytesSent, t.localPort, remoteStr, t.remoteJitterMs, t.remoteFractionLost, t.remoteRTTMs
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

			t.mu.Lock()
			t.rtcpTickCounter++
			shouldSendSR := t.rtcpTickCounter >= 250 // ~5 seconds (250 * 20ms)
			if shouldSendSR {
				t.rtcpTickCounter = 0
			}
			t.mu.Unlock()

			if shouldSendSR {
				t.sendRTCPSenderReport()
			}
		}
	}
}

// sendRTCPSenderReport builds and sends an RFC 3550 RTCP Sender Report.
func (t *Transmitter) sendRTCPSenderReport() {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.stopped || t.conn == nil || t.remoteAddr == nil || t.packetsSent == 0 {
		return
	}

	now := time.Now()
	sr := BuildRTCPSenderReport(t.ssrc, t.timestamp, uint32(t.packetsSent), uint32(t.bytesSent), now)
	sec, frac := TimeToNTP(now)
	t.lastSRCompactNTP = ((sec & 0xFFFF) << 16) | ((frac >> 16) & 0xFFFF)
	t.lastSRTime = now

	// Send to remote RTP address (multiplexed RTCP) and standard RTCP port (RTP port + 1)
	_, _ = t.conn.WriteTo(sr, t.remoteAddr)
	if t.remoteAddr.Port%2 == 0 {
		rtcpAddr := &net.UDPAddr{
			IP:   t.remoteAddr.IP,
			Port: t.remoteAddr.Port + 1,
		}
		_, _ = t.conn.WriteTo(sr, rtcpAddr)
	}
}

// readLoop listens for incoming UDP packets on the bound RTP socket (RTCP feedback and DTMF events).
func (t *Transmitter) readLoop() {
	defer t.wg.Done()

	buf := make([]byte, 1500)
	for {
		t.mu.Lock()
		conn := t.conn
		stopped := t.stopped
		t.mu.Unlock()

		if stopped || conn == nil {
			return
		}

		n, fromAddr, err := conn.ReadFrom(buf)
		if err != nil {
			return
		}

		if n > 0 {
			t.handleIncomingPacket(buf[:n], fromAddr)
		}
	}
}

// handleIncomingPacket parses incoming RTCP reports and RFC 2833 / 4733 DTMF packets.
func (t *Transmitter) handleIncomingPacket(data []byte, fromAddr net.Addr) {
	if len(data) < 4 {
		return
	}

	t.mu.Lock()
	remoteAddr := t.remoteAddr
	t.mu.Unlock()

	if remoteAddr == nil {
		return
	}

	if udpFrom, ok := fromAddr.(*net.UDPAddr); ok {
		if !udpFrom.IP.Equal(remoteAddr.IP) {
			return
		}
	}

	version := (data[0] >> 6) & 0x03
	if version != 2 {
		return
	}

	pt := data[1]

	// 1. Check for RTCP Receiver Report (PT=201) or Sender Report (PT=200)
	if pt == 200 || pt == 201 {
		report, err := ParseRTCPReceiverReport(data)
		if err == nil && report != nil {
			t.mu.Lock()
			t.remoteFractionLost = report.FractionLost

			rate := t.codec.RTPClockRate()
			if rate > 0 {
				t.remoteJitterMs = (float64(report.Jitter) / float64(rate)) * 1000.0
			}

			// RTT Calculation: RTT = A - LSR - DLSR
			if report.LSR != 0 && report.DLSR != 0 && t.lastSRCompactNTP != 0 {
				now := time.Now()
				sec, frac := TimeToNTP(now)
				nowCompact := ((sec & 0xFFFF) << 16) | ((frac >> 16) & 0xFFFF)
				if nowCompact >= report.LSR+report.DLSR {
					diff := nowCompact - report.LSR - report.DLSR
					t.remoteRTTMs = (float64(diff) / 65536.0) * 1000.0
				}
			}
			t.mu.Unlock()
		}
		return
	}

	// 2. Check for RFC 2833 / RFC 4733 DTMF telephone-event (PT=101)
	payloadType := pt & 0x7F
	if payloadType == 101 && len(data) >= 16 {
		// RTP header timestamp at bytes [4:8]
		ts := binary.BigEndian.Uint32(data[4:8])
		dtmfPayload := data[12:]
		evt, err := ParseDTMFPayload(dtmfPayload)
		if err == nil && evt != nil && evt.Digit != "" {
			t.mu.Lock()
			handler := t.dtmfHandler
			isNewEvent := !t.hasLastDTMF || t.lastDTMFTimestamp != ts
			if isNewEvent {
				t.hasLastDTMF = true
				t.lastDTMFTimestamp = ts
			}
			t.mu.Unlock()

			if isNewEvent && handler != nil {
				handler(evt.Digit)
			}
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
	samplesPerPacket := uint32((codec.RTPClockRate() * 20) / 1000)

	// 1. Discard stale raw upstream chunks and stale ready frames before conversion (> 150ms behind).
	staleCutoff := now.Add(-150 * time.Millisecond)
	t.audioPath.DiscardBefore(staleCutoff)

	// 2. Transcode up to 35 frames (700ms ahead-of-time)
	_ = t.audioPath.Fill(35)

	// 3. Check scheduled playback time (Sendspin clock synchronization)
	readyFrame, ok := t.audioPath.PeekReady()
	if ok {
		if !readyFrame.PlayAt.IsZero() && now.Before(readyFrame.PlayAt) {
			// Not yet time to play scheduled audio frame; transmit comfort noise / silence
			silencePayload := generateComfortNoise(codec)
			_ = t.sendPacket(silencePayload, codec.PayloadType(), samplesPerPacket, false)
			return
		}

		// 4. Pop the ready 20ms frame
		frame, popped := t.audioPath.PopReady()
		if popped {
			if err := t.sendPacket(frame.Payload, codec.PayloadType(), samplesPerPacket, isMarker); err != nil {
				t.logger.Debug("RTP transmit error", "err", err)
			}

			if isMarker {
				t.mu.Lock()
				t.firstPkt = false
				t.mu.Unlock()
			}
			return
		}
	}

	// 5. When no ready audio frame is available (idle, paused, or buffer underrun),
	// transmit an RFC-compliant comfort noise / silence frame so the phone's decoder
	// receives continuous RTP packets and never executes Packet Loss Concealment (PLC) extrapolation.
	silencePayload := generateComfortNoise(codec)
	_ = t.sendPacket(silencePayload, codec.PayloadType(), samplesPerPacket, false)
}

var (
	silenceOpus = []byte{0xF8, 0xFF, 0xFE}
	silenceG722 = make([]byte, 160)
	silencePCMU = func() []byte {
		buf := make([]byte, 160)
		for i := range buf {
			buf[i] = 0xFF
		}
		return buf
	}()
	silencePCMA = func() []byte {
		buf := make([]byte, 160)
		for i := range buf {
			buf[i] = 0xD5
		}
		return buf
	}()
	silenceL16 = make([]byte, 1920)
)

// generateComfortNoise returns an RFC-compliant silence / comfort noise frame for the codec.
func generateComfortNoise(codec domain.Codec) []byte {
	switch codec {
	case domain.CodecOpus:
		return silenceOpus
	case domain.CodecPCMU:
		return silencePCMU
	case domain.CodecPCMA:
		return silencePCMA
	case domain.CodecG722:
		return silenceG722
	case domain.CodecL16:
		return silenceL16
	default:
		return silenceG722
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

		t.mu.Lock()
		if t.conn != nil {
			_ = t.conn.Close()
		}
		port := t.localPort
		pool := t.portPool
		t.localPort = 0
		t.mu.Unlock()

		t.wg.Wait()

		if pool != nil && port > 0 {
			pool.Release(port)
		}
	})

	if drainDelay > 0 {
		time.Sleep(drainDelay)
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	t.conn = nil
	return nil
}
