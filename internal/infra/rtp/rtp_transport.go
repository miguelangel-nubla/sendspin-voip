package rtp

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"net"
	"sync"

	"github.com/pion/rtp"
)

// RTPTransport represents Layer 4 of the audio pipeline.
// It is a pure network transport responsible for packaging 20ms audio payloads
// into RFC 3550 RTP packets and transmitting them over a UDP socket.
type RTPTransport struct {
	mu          sync.Mutex
	conn        *net.UDPConn
	localPort   int
	remoteAddr  *net.UDPAddr
	ssrc        uint32
	sequenceNum uint16
	timestamp   uint32
	packetsSent uint64
	bytesSent   uint64
	closed      bool
}

// NewRTPTransport allocates a local UDP socket on an available port in the given range.
func NewRTPTransport(minPort, maxPort int) (*RTPTransport, error) {
	if minPort <= 0 {
		minPort = 10000
	}
	if maxPort <= 0 || maxPort < minPort {
		maxPort = 20000
	}

	for port := minPort; port <= maxPort; port += 2 {
		addr := &net.UDPAddr{IP: net.ParseIP("0.0.0.0"), Port: port}
		conn, err := net.ListenUDP("udp", addr)
		if err == nil {
			var b [4]byte
			_, _ = rand.Read(b[:])
			ssrc := binary.BigEndian.Uint32(b[:])

			var seqB [2]byte
			_, _ = rand.Read(seqB[:])
			initSeq := binary.BigEndian.Uint16(seqB[:])

			var tsB [4]byte
			_, _ = rand.Read(tsB[:])
			initTs := binary.BigEndian.Uint32(tsB[:])

			return &RTPTransport{
				conn:        conn,
				localPort:   port,
				ssrc:        ssrc,
				sequenceNum: initSeq,
				timestamp:   initTs,
			}, nil
		}
	}

	return nil, fmt.Errorf("no available RTP ports in range %d-%d", minPort, maxPort)
}

// LocalPort returns the bound UDP port.
func (t *RTPTransport) LocalPort() int {
	return t.localPort
}

// SetRemoteAddr configures the target UDP address to send RTP packets to.
func (t *RTPTransport) SetRemoteAddr(remoteAddr *net.UDPAddr) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.remoteAddr = remoteAddr
}

// SendPacket serializes and sends a 20ms audio frame over UDP with RTP header.
func (t *RTPTransport) SendPacket(payload []byte, payloadType uint8, samplesPerPacket uint32, marker bool) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.closed || t.conn == nil || t.remoteAddr == nil {
		return nil
	}

	pkt := rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			Padding:        false,
			Extension:      false,
			Marker:         marker,
			PayloadType:    payloadType,
			SequenceNumber: t.sequenceNum,
			Timestamp:      t.timestamp,
			SSRC:           t.ssrc,
		},
		Payload: payload,
	}

	t.sequenceNum++
	t.timestamp += samplesPerPacket

	raw, err := pkt.Marshal()
	if err != nil {
		return fmt.Errorf("failed to marshal RTP packet: %w", err)
	}

	n, err := t.conn.WriteToUDP(raw, t.remoteAddr)
	if err != nil {
		return err
	}

	t.packetsSent++
	t.bytesSent += uint64(n)
	return nil
}

// Stats returns transmission statistics.
func (t *RTPTransport) Stats() (packetsSent, bytesSent uint64, localPort int, remoteAddr string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	rStr := ""
	if t.remoteAddr != nil {
		rStr = t.remoteAddr.String()
	}
	return t.packetsSent, t.bytesSent, t.localPort, rStr
}

// Close releases the underlying UDP socket.
func (t *RTPTransport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.closed {
		return nil
	}
	t.closed = true
	if t.conn != nil {
		return t.conn.Close()
	}
	return nil
}
