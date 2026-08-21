package audio

import (
	"math"
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
	// 7. Test L16 linear PCM (48kHz mono 16-bit: 96000 bytes)
	l16Payload, err := trans.Transcode(srcSamples, 48000, 2, domain.CodecL16, 100)
	if err != nil {
		t.Fatalf("L16 transcode failed: %v", err)
	}
	if len(l16Payload) != 48000*2 {
		t.Errorf("expected %d bytes for L16 1 second at 48kHz, got %d", 48000*2, len(l16Payload))
	}
}

func TestTranscoder_OpusEncodeWithVolume(t *testing.T) {
	trans := NewTranscoder()

	// 20ms of 48kHz mono (after stereo downmix path: feed stereo 20ms)
	src := make([]int32, 48000/50*2) // 1920 interleaved
	for i := range src {
		src[i] = 8000
	}

	payload, err := trans.Transcode(src, 48000, 2, domain.CodecOpus, 50)
	if err != nil {
		t.Fatalf("opus encode at 50%% volume failed: %v", err)
	}
	if len(payload) == 0 {
		t.Fatal("expected non-empty opus packet for 20ms frame")
	}

	muted, err := trans.Transcode(src, 48000, 2, domain.CodecOpus, 0)
	if err != nil {
		t.Fatalf("opus encode muted failed: %v", err)
	}
	if len(muted) == 0 {
		t.Fatal("expected silence opus packet when muted")
	}
}

func TestApplyVolume_DBTaper(t *testing.T) {
	trans := NewTranscoder()
	src := []int32{10000, -10000, 5000}

	half := trans.ApplyVolume(append([]int32(nil), src...), 50)
	// 50% → −30 dB → factor ≈ 0.03162
	const wantFactor = 0.031622776
	got := float64(half[0]) / 10000.0
	if got < wantFactor*0.9 || got > wantFactor*1.1 {
		t.Fatalf("50%% factor got %f, want ~%f (−30 dB)", got, wantFactor)
	}

	full := trans.ApplyVolume(append([]int32(nil), src...), 100)
	if full[0] != 10000 {
		t.Fatalf("100%% should be unity, got %d", full[0])
	}

	oldCurve := math.Pow(0.5, 1.5) // previous curve ≈ 0.35 (−9 dB)
	if got >= oldCurve*0.5 {
		t.Fatalf("50%% should be much quieter than old pow(1.5) curve (%f), got %f", oldCurve, got)
	}
}

func TestTranscoder_OpusDecodeThenVolumeEncode(t *testing.T) {
	trans := NewTranscoder()

	// Build a real opus packet first
	src := make([]int32, 1920)
	for i := range src {
		src[i] = 12000
	}
	pkt, err := trans.Transcode(src, 48000, 2, domain.CodecOpus, 100)
	if err != nil || len(pkt) == 0 {
		t.Fatalf("setup encode failed: %v len=%d", err, len(pkt))
	}

	trans.Reset()

	pcm, err := trans.DecodeOpusToPCM(pkt, 2)
	if err != nil {
		t.Fatalf("DecodeOpusToPCM: %v", err)
	}
	if len(pcm) < 960 {
		t.Fatalf("expected decoded pcm, got %d samples", len(pcm))
	}

	quiet, err := trans.Transcode(pcm, 48000, 2, domain.CodecOpus, 25)
	if err != nil {
		t.Fatalf("re-encode at 25%%: %v", err)
	}
	if len(quiet) == 0 {
		t.Fatal("expected re-encoded packet")
	}
}

// TestApplyVolume_DoesNotMutateInput pins that gain and mute never write
// through the caller's slice. DownmixToMono returns its argument unchanged for
// mono input, so the previous in-place zeroing on mute clobbered the caller's
// PCM frame rather than producing a separate silent one.
func TestApplyVolume_DoesNotMutateInput(t *testing.T) {
	tr := NewTranscoder()

	for _, volume := range []int{0, 50} {
		input := []int32{100, -200, 300, -400}
		original := append([]int32(nil), input...)

		out := tr.ApplyVolume(input, volume)

		for i := range input {
			if input[i] != original[i] {
				t.Fatalf("volume %d mutated the input slice at %d: got %d, want %d",
					volume, i, input[i], original[i])
			}
		}
		if len(out) != len(input) {
			t.Errorf("volume %d: expected %d output samples, got %d", volume, len(input), len(out))
		}
	}
}

func TestApplyVolume_MuteProducesSilence(t *testing.T) {
	tr := NewTranscoder()
	out := tr.ApplyVolume([]int32{1000, -1000, 32767}, 0)
	for i, v := range out {
		if v != 0 {
			t.Errorf("expected silence at %d, got %d", i, v)
		}
	}
}
