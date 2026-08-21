package rtp

import (
	"net"
	"testing"
	"time"

	"github.com/miguelangel-nubla/sendspin-voip/internal/domain"
	"github.com/miguelangel-nubla/sendspin-voip/internal/infra/audio"
)

func TestStreamer_CreateSessionAndPushAudio(t *testing.T) {
	transcoder := audio.NewTranscoder()
	streamer := NewStreamer(nil, transcoder, 20000, 20050)

	sess, err := streamer.CreateSession(domain.CodecG722)
	if err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}
	defer func() { _ = sess.DrainAndClose(0) }()

	if sess.LocalPort() < 20000 || sess.LocalPort() >= 20050 {
		t.Errorf("expected port in range [20000, 20050), got %d", sess.LocalPort())
	}

	// Mock UDP receiver socket
	recvConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("failed to bind mock receiver socket: %v", err)
	}
	defer recvConn.Close()

	destAddr := recvConn.LocalAddr().(*net.UDPAddr)

	if err := sess.StartTransmission(destAddr); err != nil {
		t.Fatalf("StartTransmission failed: %v", err)
	}

	// Push 20ms of audio (48kHz stereo = 1920 samples)
	chunk := domain.AudioChunk{
		Samples:    make([]int32, 1920),
		SampleRate: 48000,
		Channels:   2,
		BitDepth:   16,
	}

	if err := sess.PushAudio(chunk, 100); err != nil {
		t.Fatalf("PushAudio failed: %v", err)
	}

	// Read packet on receiver
	_ = recvConn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	buf := make([]byte, 1500)
	n, _, err := recvConn.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("failed to receive RTP packet: %v", err)
	}

	if n < 12 { // RTP header is at least 12 bytes
		t.Errorf("expected RTP packet size >= 12, got %d", n)
	}
}

func TestStreamer_OpusPassthrough(t *testing.T) {
	transcoder := audio.NewTranscoder()
	streamer := NewStreamer(nil, transcoder, 20100, 20150)

	sess, err := streamer.CreateSession(domain.CodecOpus)
	if err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}
	defer func() { _ = sess.DrainAndClose(0) }()

	recvConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("failed to bind mock receiver socket: %v", err)
	}
	defer recvConn.Close()

	destAddr := recvConn.LocalAddr().(*net.UDPAddr)

	if err := sess.StartTransmission(destAddr); err != nil {
		t.Fatalf("StartTransmission failed: %v", err)
	}

	// Mock Opus frame
	mockOpus := []byte{0xFC, 0xFF, 0xFE, 0x01, 0x02, 0x03}
	chunk := domain.AudioChunk{
		OpusData:   mockOpus,
		SampleRate: 48000,
		Channels:   2,
		BitDepth:   16,
	}

	if err := sess.PushAudio(chunk, 100); err != nil {
		t.Fatalf("PushAudio for Opus failed: %v", err)
	}

	_ = recvConn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	buf := make([]byte, 1500)
	n, _, err := recvConn.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("failed to receive Opus RTP packet: %v", err)
	}

	if n < 12+len(mockOpus) {
		t.Errorf("expected RTP packet size >= %d, got %d", 12+len(mockOpus), n)
	}
}
