package audio

import (
	"testing"

	"github.com/miguelangel-nubla/sendspin-voip/internal/domain"
)

func TestTranscoder_StereoToMonoAndResample(t *testing.T) {
	trans := NewTranscoder()

	// Generate 1 second of 48kHz stereo sine/triangular mock samples (96000 interleaved samples)
	srcSamples := make([]int32, 48000*2)
	for i := 0; i < len(srcSamples); i++ {
		srcSamples[i] = int32((i % 1000) * 10)
	}

	// 1. Test G.711 PCMU (48kHz stereo -> 8kHz mono G.711u)
	pcmu, err := trans.Transcode(srcSamples, 48000, 2, domain.CodecPCMU, 100)
	if err != nil {
		t.Fatalf("PCMU transcode failed: %v", err)
	}
	if len(pcmu) != 8000 {
		t.Errorf("expected 8000 PCMU bytes for 1 second at 8kHz, got %d", len(pcmu))
	}

	// 2. Test G.722 (48kHz stereo -> 16kHz mono G.722 ADPCM @ 64kbps: 8000 bytes)
	g722Payload, err := trans.Transcode(srcSamples, 48000, 2, domain.CodecG722, 100)
	if err != nil {
		t.Fatalf("G722 transcode failed: %v", err)
	}
	if len(g722Payload) != 8000 {
		t.Errorf("expected 8000 G.722 bytes (64kbps) for 1 second, got %d", len(g722Payload))
	}

	// 3. Test G.711 PCMA (48kHz stereo -> 8kHz mono G.711a)
	pcma, err := trans.Transcode(srcSamples, 48000, 2, domain.CodecPCMA, 100)
	if err != nil {
		t.Fatalf("PCMA transcode failed: %v", err)
	}
	if len(pcma) != 8000 {
		t.Errorf("expected 8000 PCMA bytes for 1 second at 8kHz, got %d", len(pcma))
	}

	// 4. Test volume scaling (mute)
	muted, err := trans.Transcode(srcSamples, 48000, 2, domain.CodecPCMU, 0)
	if err != nil {
		t.Fatalf("muted transcode failed: %v", err)
	}
	for i, b := range muted {
		// PCMU silence byte is 0xFF (u-law 0)
		if b != 0xFF {
			t.Errorf("expected silence byte 0xFF at index %d, got 0x%X", i, b)
			break
		}
	}

	// 5. Test volume scaling (partial volume 50%)
	vol50, err := trans.Transcode(srcSamples, 48000, 2, domain.CodecPCMU, 50)
	if err != nil {
		t.Fatalf("50%% volume transcode failed: %v", err)
	}
	if len(vol50) != 8000 {
		t.Errorf("expected 8000 bytes at 50%% volume, got %d", len(vol50))
	}

	// 6. Test extreme sample clamping
	extremeSamples := []int32{100000, -100000, 32767, -32768, 0}
	clampedPayload, err := trans.Transcode(extremeSamples, 8000, 1, domain.CodecPCMU, 100)
	if err != nil {
		t.Fatalf("clamping transcode failed: %v", err)
	}
	if len(clampedPayload) != len(extremeSamples) {
		t.Errorf("expected %d bytes, got %d", len(extremeSamples), len(clampedPayload))
	}
}
