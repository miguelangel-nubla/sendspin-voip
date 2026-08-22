package rtp

import (
	"encoding/binary"
	"net"
	"testing"
)

func TestParseDTMFPayload(t *testing.T) {
	// Digit '5', End bit set (0x80), volume 10, duration 160
	payload := []byte{5, 0x80 | 10, 0x00, 0xA0}

	evt, err := ParseDTMFPayload(payload)
	if err != nil {
		t.Fatalf("ParseDTMFPayload failed: %v", err)
	}
	if evt.Digit != "5" {
		t.Errorf("expected digit '5', got %s", evt.Digit)
	}
	if !evt.EndOfEvt {
		t.Errorf("expected EndOfEvt to be true")
	}
	if evt.Volume != 10 {
		t.Errorf("expected volume 10, got %d", evt.Volume)
	}
	if evt.Duration != 160 {
		t.Errorf("expected duration 160, got %d", evt.Duration)
	}
}

func TestEventToDigit_SpecialKeys(t *testing.T) {
	tests := []struct {
		event byte
		want  string
	}{
		{10, "*"},
		{11, "#"},
		{12, "A"},
		{13, "B"},
		{14, "C"},
		{15, "D"},
		{16, "FLASH"},
	}

	for _, tt := range tests {
		got, ok := EventToDigit(tt.event)
		if !ok || got != tt.want {
			t.Errorf("EventToDigit(%d) = (%s, %v), want (%s, true)", tt.event, got, ok, tt.want)
		}
	}
}

func TestTransmitter_DTMFDeduplicationAndPeerCheck(t *testing.T) {
	tx, err := NewTransmitter(nil, NewAudioPath(nil, "g722", 100), "g722", NewPortPool(30000, 30100))
	if err != nil {
		t.Fatalf("NewTransmitter failed: %v", err)
	}
	defer tx.DrainAndClose(0)

	var firedCount int
	var lastDigit string
	tx.SetDTMFHandler(func(d string) {
		firedCount++
		lastDigit = d
	})

	remoteAddr := &net.UDPAddr{IP: net.ParseIP("192.168.1.50"), Port: 16384}
	tx.SetRemoteAddr(remoteAddr)

	// Build RFC 4733 DTMF RTP packet: 12-byte RTP header + 4-byte telephone-event payload
	buildDTMFPacket := func(ts uint32, digit byte, endOfEvt bool) []byte {
		pkt := make([]byte, 16)
		pkt[0] = 0x80 // V=2
		pkt[1] = 101  // PT=101
		binary.BigEndian.PutUint16(pkt[2:4], 100)
		binary.BigEndian.PutUint32(pkt[4:8], ts)
		binary.BigEndian.PutUint32(pkt[8:12], 12345) // SSRC

		pkt[12] = digit // e.g. 5
		if endOfEvt {
			pkt[13] = 0x80 | 10
		} else {
			pkt[13] = 10
		}
		binary.BigEndian.PutUint16(pkt[14:16], 160)
		return pkt
	}

	// 1. Packet from unauthorized peer should be dropped
	fakeAddr := &net.UDPAddr{IP: net.ParseIP("10.0.0.99"), Port: 16384}
	tx.handleIncomingPacket(buildDTMFPacket(1000, 5, false), fakeAddr)
	if firedCount != 0 {
		t.Errorf("expected 0 firings from unauthorized sender, got %d", firedCount)
	}

	// 2. Initial packet for keypress #1 (ts=1000)
	tx.handleIncomingPacket(buildDTMFPacket(1000, 5, false), remoteAddr)
	if firedCount != 1 || lastDigit != "5" {
		t.Errorf("expected 1 firing for keypress, got %d (digit=%s)", firedCount, lastDigit)
	}

	// 3. Retransmitted packets for SAME keypress #1 (same ts=1000) should be ignored
	tx.handleIncomingPacket(buildDTMFPacket(1000, 5, false), remoteAddr)
	tx.handleIncomingPacket(buildDTMFPacket(1000, 5, true), remoteAddr)
	tx.handleIncomingPacket(buildDTMFPacket(1000, 5, true), remoteAddr)
	if firedCount != 1 {
		t.Errorf("expected still 1 firing after redundant packets, got %d", firedCount)
	}

	// 4. New keypress #2 (ts=2000)
	tx.handleIncomingPacket(buildDTMFPacket(2000, 11, false), remoteAddr) // digit 11 is '#'
	if firedCount != 2 || lastDigit != "#" {
		t.Errorf("expected 2 firings after second keypress, got %d (digit=%s)", firedCount, lastDigit)
	}
}
