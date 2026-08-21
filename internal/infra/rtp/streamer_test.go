package rtp

import (
	"net"
	"testing"
	"time"

	"github.com/miguelangel-nubla/sendspin-voip/internal/app"
	"github.com/miguelangel-nubla/sendspin-voip/internal/domain"
	"github.com/miguelangel-nubla/sendspin-voip/internal/infra/audio"
)

func TestStreamer_CreateSessionAndPushAudio(t *testing.T) {
	streamer := NewStreamer(nil, func() app.AudioTranscoderPort {
		return audio.NewTranscoder()
	}, 20000, 20050)

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

func TestStreamer_OpusPassthroughOnlyAtFullVolume(t *testing.T) {
	streamer := NewStreamer(nil, func() app.AudioTranscoderPort {
		return audio.NewTranscoder()
	}, 20100, 20150)

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

	mockOpus := []byte{0xFC, 0xFF, 0xFE, 0x01, 0x02, 0x03}
	chunk := domain.AudioChunk{
		OpusData:   mockOpus,
		Samples:    make([]int32, 1920),
		SampleRate: 48000,
		Channels:   2,
		BitDepth:   16,
	}

	if err := sess.PushAudio(chunk, 100); err != nil {
		t.Fatalf("PushAudio passthrough failed: %v", err)
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

func TestStreamer_OpusToG722DecodesWhenSamplesMissing(t *testing.T) {
	streamer := NewStreamer(nil, func() app.AudioTranscoderPort {
		return audio.NewTranscoder()
	}, 20300, 20350)

	sess, err := streamer.CreateSession(domain.CodecG722)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	defer func() { _ = sess.DrainAndClose(0) }()

	recvConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer recvConn.Close()
	if err := sess.StartTransmission(recvConn.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatal(err)
	}

	// Build a real Opus packet, then feed it as OpusData-only (no Samples) toward G.722
	setup := audio.NewTranscoder()
	src := make([]int32, 1920)
	for i := range src {
		src[i] = 4000
	}
	opusPkt, err := setup.Transcode(src, 48000, 2, domain.CodecOpus, 100)
	if err != nil || len(opusPkt) == 0 {
		t.Fatalf("setup opus: %v len=%d", err, len(opusPkt))
	}

	chunk := domain.AudioChunk{
		OpusData:   opusPkt,
		Samples:    nil, // force local decode path
		SampleRate: 48000,
		Channels:   2,
		BitDepth:   16,
	}
	if err := sess.PushAudio(chunk, 100); err != nil {
		t.Fatalf("PushAudio Opus→G722: %v", err)
	}

	_ = recvConn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	buf := make([]byte, 1500)
	n, _, err := recvConn.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("expected G.722 RTP after Opus decode: %v", err)
	}
	if n < 12 {
		t.Fatalf("packet too small: %d", n)
	}
	stats := sess.Stats()
	if stats.PathMode != "transcode" {
		t.Fatalf("expected transcode path, got %q", stats.PathMode)
	}
}

func TestStreamer_OpusVolumeReencodes(t *testing.T) {
	streamer := NewStreamer(nil, func() app.AudioTranscoderPort {
		return audio.NewTranscoder()
	}, 20200, 20250)

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

	if err := sess.StartTransmission(recvConn.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatalf("StartTransmission failed: %v", err)
	}

	samples := make([]int32, 1920)
	for i := range samples {
		samples[i] = 5000
	}
	chunk := domain.AudioChunk{
		OpusData:   []byte{0xAA, 0xBB}, // must NOT be forwarded at volume 50
		Samples:    samples,
		SampleRate: 48000,
		Channels:   2,
		BitDepth:   16,
	}

	if err := sess.PushAudio(chunk, 50); err != nil {
		t.Fatalf("PushAudio volume path failed: %v", err)
	}

	_ = recvConn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	buf := make([]byte, 1500)
	n, _, err := recvConn.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("expected re-encoded opus RTP packet: %v", err)
	}
	if n <= 12+2 {
		t.Fatalf("packet looks like raw passthrough of tiny OpusData, size=%d", n)
	}
	// Payload should not be the 2-byte stub
	if n == 14 && buf[12] == 0xAA && buf[13] == 0xBB {
		t.Fatal("received passthrough stub instead of re-encoded opus")
	}
}
