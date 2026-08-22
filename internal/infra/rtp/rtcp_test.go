package rtp

import (
	"encoding/binary"
	"testing"
	"time"
)

func TestBuildAndParseRTCPSenderReport(t *testing.T) {
	now := time.Now()
	sr := BuildRTCPSenderReport(0x12345678, 96000, 100, 16000, now)

	if len(sr) != 28 {
		t.Fatalf("expected 28 bytes, got %d", len(sr))
	}
	if sr[0] != 0x80 || sr[1] != 200 {
		t.Errorf("unexpected RTCP SR header: %02x %02x", sr[0], sr[1])
	}
	if ssrc := binary.BigEndian.Uint32(sr[4:8]); ssrc != 0x12345678 {
		t.Errorf("expected SSRC 0x12345678, got 0x%08x", ssrc)
	}
	if pkts := binary.BigEndian.Uint32(sr[20:24]); pkts != 100 {
		t.Errorf("expected packet count 100, got %d", pkts)
	}
}

func TestParseRTCPReceiverReport(t *testing.T) {
	// Build a valid 32-byte RTCP Receiver Report with 1 report block
	rr := make([]byte, 32)
	rr[0] = 0x81 // V=2, P=0, RC=1
	rr[1] = 201  // PT=201 (RR)
	binary.BigEndian.PutUint16(rr[2:4], 7)
	binary.BigEndian.PutUint32(rr[4:8], 0x87654321) // SSRC of packet sender

	// Report block:
	binary.BigEndian.PutUint32(rr[8:12], 0x12345678) // SSRC_1
	rr[12] = 12                                      // Fraction lost ~ 4.68%
	rr[13] = 0
	rr[14] = 0
	rr[15] = 5 // 5 packets lost
	binary.BigEndian.PutUint32(rr[16:20], 1005) // Highest seq
	binary.BigEndian.PutUint32(rr[20:24], 160)  // Jitter (160 samples = 20ms @ 8kHz)
	binary.BigEndian.PutUint32(rr[24:28], 0xAABBCCDD)
	binary.BigEndian.PutUint32(rr[28:32], 0x00010000)

	parsed, err := ParseRTCPReceiverReport(rr)
	if err != nil {
		t.Fatalf("ParseRTCPReceiverReport failed: %v", err)
	}
	if parsed == nil {
		t.Fatalf("expected parsed report data, got nil")
	}
	if parsed.SSRC != 0x12345678 {
		t.Errorf("expected source SSRC 0x12345678, got 0x%08x", parsed.SSRC)
	}
	if parsed.PacketsLost != 5 {
		t.Errorf("expected 5 packets lost, got %d", parsed.PacketsLost)
	}
	if parsed.Jitter != 160 {
		t.Errorf("expected jitter 160, got %d", parsed.Jitter)
	}
}
