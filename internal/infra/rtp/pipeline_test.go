package rtp

import (
	"net"
	"testing"
	"time"

	"github.com/miguelangel-nubla/sendspin-voip/internal/app"
	"github.com/miguelangel-nubla/sendspin-voip/internal/domain"
	"github.com/miguelangel-nubla/sendspin-voip/internal/infra/audio"
)

// TestUpstreamPlayer_PreservesStreamHead verifies that on buffer overflow,
// UpstreamPlayer tail-drops instead of evicting index 0 (preserving speech start).
func TestUpstreamPlayer_PreservesStreamHead(t *testing.T) {
	upstream := NewUpstreamPlayer(0)

	firstChunk := domain.AudioChunk{
		Timestamp: 1000,
		Samples:   []int32{1, 2, 3, 4},
	}
	upstream.Push(firstChunk)

	for i := 1; i < defaultUpstreamBufferCapacity; i++ {
		upstream.Push(domain.AudioChunk{
			Timestamp: int64(1000 + i*20000),
			Samples:   []int32{int32(i)},
		})
	}

	if upstream.Len() != defaultUpstreamBufferCapacity {
		t.Fatalf("expected len %d, got %d", defaultUpstreamBufferCapacity, upstream.Len())
	}

	overflowChunk := domain.AudioChunk{
		Timestamp: 99999999,
		Samples:   []int32{999},
	}
	upstream.Push(overflowChunk)

	head, ok := upstream.Pop()
	if !ok {
		t.Fatal("expected head chunk")
	}
	if head.Timestamp != 1000 || len(head.Samples) != 4 || head.Samples[0] != 1 {
		t.Fatalf("head was corrupted or evicted: %+v", head)
	}
}

// TestUpstreamPlayer_OutOfOrderInsertion verifies chronological ordering on insert.
func TestUpstreamPlayer_OutOfOrderInsertion(t *testing.T) {
	upstream := NewUpstreamPlayer(0)

	c1 := domain.AudioChunk{Timestamp: 1000}
	c3 := domain.AudioChunk{Timestamp: 3000}
	c2 := domain.AudioChunk{Timestamp: 2000}

	upstream.Push(c1)
	upstream.Push(c3)
	upstream.Push(c2)

	pop1, _ := upstream.Pop()
	pop2, _ := upstream.Pop()
	pop3, _ := upstream.Pop()

	if pop1.Timestamp != 1000 || pop2.Timestamp != 2000 || pop3.Timestamp != 3000 {
		t.Fatalf("unexpected order: %d, %d, %d", pop1.Timestamp, pop2.Timestamp, pop3.Timestamp)
	}
}

// TestAudioPath_VolumeChangeRewindsAndReEncodesUnplayed verifies that changing volume
// discards converted ready frames and rewinds UpstreamPlayer so all unplayed audio is re-encoded with the new volume.
func TestAudioPath_VolumeChangeRewindsAndReEncodesUnplayed(t *testing.T) {
	transcoder := audio.NewTranscoder()
	upstream := NewUpstreamPlayer(0)
	audioPath := NewAudioPath(transcoder, upstream, domain.CodecG722, 100)

	// Push 2 chunks to upstream (total 2 unplayed chunks)
	chunk1 := domain.AudioChunk{
		Samples:    make([]int32, 1920),
		SampleRate: 48000,
		Channels:   2,
	}
	chunk2 := domain.AudioChunk{
		Samples:    make([]int32, 1920),
		SampleRate: 48000,
		Channels:   2,
	}
	upstream.Push(chunk1)
	upstream.Push(chunk2)

	// Convert both chunks into AudioPath ready frames ahead of time
	if err := audioPath.Fill(2); err != nil {
		t.Fatalf("Fill failed: %v", err)
	}
	if audioPath.ReadyLen() != 2 {
		t.Fatalf("expected 2 ready frames in audioPath, got %d", audioPath.ReadyLen())
	}
	if upstream.UnreadLen() != 0 {
		t.Fatalf("expected 0 unread chunks in upstream, got %d", upstream.UnreadLen())
	}
	if upstream.Len() != 2 {
		t.Fatalf("expected 2 total unplayed chunks in upstream, got %d", upstream.Len())
	}

	// Change volume: must flush ready frames and rewind read cursor in UpstreamPlayer
	audioPath.SetVolume(50)

	if audioPath.ReadyLen() != 0 {
		t.Fatalf("expected ready queue to be flushed on volume change, got %d", audioPath.ReadyLen())
	}
	if upstream.UnreadLen() != 2 {
		t.Fatalf("expected upstream read cursor to rewind to 2 unread chunks, got %d", upstream.UnreadLen())
	}
	if upstream.Len() != 2 {
		t.Fatalf("expected both raw chunks to remain intact in upstream, got len %d", upstream.Len())
	}

	// Re-convert unplayed chunks with the new volume
	if err := audioPath.Fill(2); err != nil {
		t.Fatalf("Fill failed: %v", err)
	}
	if audioPath.ReadyLen() != 2 {
		t.Fatalf("expected audioPath to re-convert all 2 unplayed chunks, got %d", audioPath.ReadyLen())
	}
}

// TestDownstreamPlayer_LiveDialDelayInstantCatchUp verifies that after a slow SIP dial (e.g. 2s),
// DownstreamPlayer instantly discards stale audio before conversion and begins streaming real-time live frames.
func TestDownstreamPlayer_LiveDialDelayInstantCatchUp(t *testing.T) {
	streamer := NewStreamer(nil, func() app.AudioTranscoderPort {
		return audio.NewTranscoder()
	}, 20800, 20850)

	sess, err := streamer.CreateSession(domain.CodecG722)
	if err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}
	defer func() { _ = sess.DrainAndClose(0) }()

	// Simulate 100 chunks (~2 seconds) pushed during slow SIP dial (scheduled in the past)
	pastBase := time.Now().Add(-2 * time.Second)
	for i := 0; i < 100; i++ {
		_ = sess.PushAudio(domain.AudioChunk{
			PlayAt:     pastBase.Add(time.Duration(i*20) * time.Millisecond),
			Timestamp:  int64(i * 20000),
			Samples:    make([]int32, 1920),
			SampleRate: 48000,
			Channels:   2,
		}, 100)
	}

	// Now push a chunk scheduled for RIGHT NOW (live audio)
	nowChunk := domain.AudioChunk{
		PlayAt:     time.Now().Add(5 * time.Millisecond),
		Timestamp:  2000000,
		Samples:    make([]int32, 1920),
		SampleRate: 48000,
		Channels:   2,
	}
	_ = sess.PushAudio(nowChunk, 100)

	recvConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer recvConn.Close()

	// Call answers
	if err := sess.StartTransmission(recvConn.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatal(err)
	}

	_ = recvConn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	buf := make([]byte, 1500)
	n, _, err := recvConn.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("failed to receive live RTP packet immediately after answer: %v (possible catch-up trickle bug)", err)
	}
	if n < 12 {
		t.Fatalf("expected valid RTP packet, got size %d", n)
	}
}

// TestPipeline_ClearBufferFlushesAllLayers verifies that ClearBuffer resets all layers.
func TestPipeline_ClearBufferFlushesAllLayers(t *testing.T) {
	streamer := NewStreamer(nil, func() app.AudioTranscoderPort {
		return audio.NewTranscoder()
	}, 21000, 21050)

	sess, err := streamer.CreateSession(domain.CodecG722)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sess.DrainAndClose(0) }()

	_ = sess.PushAudio(domain.AudioChunk{
		Timestamp:  1000,
		Samples:    make([]int32, 1920),
		SampleRate: 48000,
		Channels:   2,
	}, 100)

	sess.ClearBuffer()

	statsAfter := sess.Stats()
	if statsAfter.UpstreamChunks != 0 || statsAfter.ConversionQueue != 0 {
		t.Fatalf("expected 0 chunks after ClearBuffer, got upstream=%d conv=%d", statsAfter.UpstreamChunks, statsAfter.ConversionQueue)
	}
}

// TestGaplessPlayback_NextSongPreBuffering verifies that when Sendspin buffers the next song
// before the current one finishes, the PlayAt timeline transitions seamlessly without gap or glitch.
func TestGaplessPlayback_NextSongPreBuffering(t *testing.T) {
	streamer := NewStreamer(nil, func() app.AudioTranscoderPort {
		return audio.NewTranscoder()
	}, 21100, 21150)

	sess, err := streamer.CreateSession(domain.CodecG722)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sess.DrainAndClose(0) }()

	baseTime := time.Now().Add(20 * time.Millisecond)

	// Song A (44.1kHz stereo): 2 chunks scheduled at T+20ms and T+40ms
	_ = sess.PushAudio(domain.AudioChunk{
		PlayAt:     baseTime,
		Timestamp:  100000,
		Samples:    make([]int32, 1764),
		SampleRate: 44100,
		Channels:   2,
	}, 100)

	_ = sess.PushAudio(domain.AudioChunk{
		PlayAt:     baseTime.Add(20 * time.Millisecond),
		Timestamp:  120000,
		Samples:    make([]int32, 1764),
		SampleRate: 44100,
		Channels:   2,
	}, 100)

	// Song B (48kHz stereo): pre-buffered next song scheduled at T+40ms (even if server timestamp resets)
	_ = sess.PushAudio(domain.AudioChunk{
		PlayAt:     baseTime.Add(40 * time.Millisecond),
		Timestamp:  0, // Server timestamp reset for new track
		Samples:    make([]int32, 1920),
		SampleRate: 48000,
		Channels:   2,
	}, 100)

	recvConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer recvConn.Close()

	if err := sess.StartTransmission(recvConn.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatal(err)
	}

	// Read packets across song boundary
	_ = recvConn.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
	buf := make([]byte, 1500)
	packetCount := 0
	for i := 0; i < 3; i++ {
		n, _, err := recvConn.ReadFromUDP(buf)
		if err != nil {
			break
		}
		if n >= 12 {
			packetCount++
		}
	}

	if packetCount < 3 {
		t.Fatalf("expected 3 packets received across gapless song transition, got %d", packetCount)
	}
}
