package sendspin

import (
	"encoding/binary"
	"math"
	"testing"
	"time"

	"github.com/Sendspin/sendspin-go/pkg/protocol"
	"github.com/miguelangel-nubla/sendspin-voip/internal/domain"
	"github.com/pion/opus"
	"github.com/thesyncim/gopus"
)

func TestDecodePCM_16Bit(t *testing.T) {
	// 4 samples of 16-bit little-endian
	raw := make([]byte, 8)
	binary.LittleEndian.PutUint16(raw[0:2], uint16(0))
	binary.LittleEndian.PutUint16(raw[2:4], uint16(1000))
	var negVal int16 = -1000
	binary.LittleEndian.PutUint16(raw[4:6], uint16(negVal))
	binary.LittleEndian.PutUint16(raw[6:8], uint16(32767))

	samples := decodePCM(raw, 16)
	if len(samples) != 4 {
		t.Fatalf("expected 4 samples, got %d", len(samples))
	}
	if samples[0] != 0 || samples[1] != 1000 || samples[2] != -1000 || samples[3] != 32767 {
		t.Errorf("unexpected 16-bit decoded samples: %v", samples)
	}
}

func TestDecodePCM_24Bit(t *testing.T) {
	// 2 samples of 24-bit little-endian:
	// Sample 1: 0x00, 0x80, 0x00 -> 0x008000 (32768 in 24-bit) -> scaled >> 8 = 128
	// Sample 2: 0xFF, 0x7F, 0x7F -> 0x7F7FFF (~8355839 in 24-bit) -> scaled >> 8 = 32639
	raw := []byte{
		0x00, 0x80, 0x00,
		0xFF, 0x7F, 0x7F,
	}

	samples := decodePCM(raw, 24)
	if len(samples) != 2 {
		t.Fatalf("expected 2 samples, got %d", len(samples))
	}
	if samples[0] != 128 {
		t.Errorf("expected sample 0 to be 128, got %d", samples[0])
	}
	if samples[1] != 32639 {
		t.Errorf("expected sample 1 to be 32639, got %d", samples[1])
	}
}

func TestNormalizeChannels(t *testing.T) {
	tests := []struct {
		name     string
		channels int
		fallback int
		want     int
	}{
		{"mono passes through", 1, 2, 1},
		{"stereo passes through", 2, 1, 2},
		{"zero falls back", 0, 1, 1},
		{"negative falls back", -3, 2, 2},
		// A server announcing a layout the Opus decoder was not built for used to
		// be trusted verbatim, which sized the sample slice past the decode buffer
		// and panicked with an index-out-of-range on the first chunk.
		{"unsupported layout falls back", 6, 2, 2},
		{"unsupported layout with bad fallback defaults to stereo", 6, 0, 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := domain.NormalizeChannels(tt.channels, tt.fallback); got != tt.want {
				t.Errorf("NormalizeChannels(%d, %d) = %d, want %d", tt.channels, tt.fallback, got, tt.want)
			}
		})
	}
}

func TestDecodeIncomingAudioChunk_PreservesOpusDecoderStateAcrossFrames(t *testing.T) {
	// Generate 2 consecutive 20ms frames (40ms total) of 440Hz sine wave at 48kHz mono
	const sampleRate = 48000
	const channels = 1
	const frameSamples = sampleRate / 50 // 960 samples (20ms)
	const totalSamples = frameSamples * 2

	pcm16Full := make([]int16, totalSamples)
	for i := 0; i < totalSamples; i++ {
		sine := math.Sin(2 * math.Pi * 440.0 * float64(i) / float64(sampleRate))
		pcm16Full[i] = int16(sine * 16000)
	}

	enc, err := gopus.NewEncoder(gopus.EncoderConfig{
		SampleRate:  sampleRate,
		Channels:    channels,
		Application: gopus.ApplicationAudio,
	})
	if err != nil {
		t.Fatalf("failed to create opus encoder: %v", err)
	}

	rawPkt1 := make([]byte, 1000)
	n1, err := enc.EncodeInt16(pcm16Full[:frameSamples], rawPkt1)
	if err != nil || n1 <= 0 {
		t.Fatalf("failed to encode frame 1: %v (n=%d)", err, n1)
	}
	pkt1 := append([]byte(nil), rawPkt1[:n1]...)

	rawPkt2 := make([]byte, 1000)
	n2, err := enc.EncodeInt16(pcm16Full[frameSamples:], rawPkt2)
	if err != nil || n2 <= 0 {
		t.Fatalf("failed to encode frame 2: %v (n=%d)", err, n2)
	}
	pkt2 := append([]byte(nil), rawPkt2[:n2]...)

	// 1. Decode with stateful pointer decoder (preserves filter state across frames)
	statefulDecoder, err := opus.NewDecoderWithOutput(sampleRate, channels)
	if err != nil {
		t.Fatalf("failed to create opus decoder: %v", err)
	}
	buf := make([]int16, maxOpusFrameSamples*maxOpusChannels)

	chunk1 := protocol.AudioChunk{Data: pkt1, Timestamp: 0}
	chunk2 := protocol.AudioChunk{Data: pkt2, Timestamp: 20000}

	decodedChunk1 := decodeIncomingAudioChunk(chunk1, "opus", sampleRate, channels, 16, time.Now(), &statefulDecoder, true, buf)
	decodedChunk2 := decodeIncomingAudioChunk(chunk2, "opus", sampleRate, channels, 16, time.Now(), &statefulDecoder, true, buf)

	if len(decodedChunk1.Samples) != frameSamples {
		t.Fatalf("expected %d samples for frame 1, got %d", frameSamples, len(decodedChunk1.Samples))
	}
	if len(decodedChunk2.Samples) != frameSamples {
		t.Fatalf("expected %d samples for frame 2, got %d", frameSamples, len(decodedChunk2.Samples))
	}

	// 2. Decode Frame 2 with a stateless / fresh decoder (reproducing the bug where decoder was passed by value)
	statelessDecoder, err := opus.NewDecoderWithOutput(sampleRate, channels)
	if err != nil {
		t.Fatalf("failed to create stateless opus decoder: %v", err)
	}
	statelessDecodedChunk2 := decodeIncomingAudioChunk(chunk2, "opus", sampleRate, channels, 16, time.Now(), &statelessDecoder, true, buf)

	// 3. Verify boundary continuity between Frame 1 and Frame 2.
	// With stateful decoding, the transition across the 20ms boundary (sample 959 -> 960)
	// closely follows the original waveform derivative.
	endOfFrame1 := decodedChunk1.Samples[frameSamples-1]
	startOfFrame2 := decodedChunk2.Samples[0]
	actualStep := float64(startOfFrame2 - endOfFrame1)
	expectedStep := float64(pcm16Full[frameSamples] - pcm16Full[frameSamples-1])

	stepError := math.Abs(actualStep - expectedStep)
	if stepError > 600 {
		t.Errorf("boundary step error across 20ms frames is %f (want <= 600), decoder state was reset across frames", stepError)
	}

	// 4. Assert that stateless decoding causes significant boundary discontinuity
	statelessStartOfFrame2 := statelessDecodedChunk2.Samples[0]
	statelessStep := float64(statelessStartOfFrame2 - endOfFrame1)
	statelessStepError := math.Abs(statelessStep - expectedStep)
	if statelessStepError <= 600 {
		t.Errorf("expected stateless decoding to fail boundary check (> 600), got error %f", statelessStepError)
	}
}
