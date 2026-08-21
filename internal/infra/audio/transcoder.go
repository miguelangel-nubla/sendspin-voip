package audio

import (
	"fmt"
	"math"
	"sync"

	"github.com/gotranspile/g722"
	"github.com/miguelangel-nubla/sendspin-voip/internal/domain"
	resampler "github.com/tphakala/go-audio-resampler"
	"github.com/zaf/g711"
)

// Transcoder implements app.AudioTranscoderPort with high-performance pure Go transcoding.
type Transcoder struct {
	mu      sync.Mutex
	g722Enc *g722.Encoder
}

// NewTranscoder creates a new audio transcoder.
func NewTranscoder() *Transcoder {
	return &Transcoder{}
}

// Transcode converts raw PCM samples to target codec payload with downmixing, resampling, and volume scaling.
func (t *Transcoder) Transcode(
	samples []int32,
	srcRate int,
	srcChannels int,
	dstCodec domain.Codec,
	volumePercent int,
) ([]byte, error) {
	if len(samples) == 0 {
		return nil, nil
	}

	if srcChannels <= 0 {
		srcChannels = 2
	}
	if srcRate <= 0 {
		srcRate = 48000
	}

	// 1. Downmix to Mono (int32)
	mono := t.DownmixToMono(samples, srcChannels)

	// 2. Apply digital software volume gain
	mono = t.ApplyVolume(mono, volumePercent)

	// 3. Resample to target sample rate
	dstRate := dstCodec.SampleRate()
	resampled := t.Resample(mono, srcRate, dstRate)

	// 4. Convert int32 samples to int16 with clean range clamping
	pcm16 := make([]int16, len(resampled))
	for i, s := range resampled {
		val := s
		if val > 32767 {
			val = 32767
		} else if val < -32768 {
			val = -32768
		}
		pcm16[i] = int16(val)
	}

	// 5. Encode into target codec
	switch dstCodec {
	case domain.CodecPCMU:
		payload := make([]byte, len(pcm16))
		for i, v := range pcm16 {
			payload[i] = g711.EncodeUlawFrame(v)
		}
		return payload, nil

	case domain.CodecPCMA:
		payload := make([]byte, len(pcm16))
		for i, v := range pcm16 {
			payload[i] = g711.EncodeAlawFrame(v)
		}
		return payload, nil

	case domain.CodecG722:
		t.mu.Lock()
		if t.g722Enc == nil {
			t.g722Enc = g722.NewEncoder(g722.Rate64000, 0)
		}
		dstLen := len(pcm16) / 2
		if len(pcm16)%2 != 0 {
			dstLen++
		}
		dst := make([]byte, dstLen)
		n := t.g722Enc.Encode(dst, pcm16)
		t.mu.Unlock()
		return dst[:n], nil

	case domain.CodecOpus:
		return nil, fmt.Errorf("opus codec streaming is supported via direct Sendspin stream passthrough; PCM to Opus software re-encoding is not supported")

	default:
		return nil, fmt.Errorf("unsupported destination codec: %s", dstCodec)
	}
}

// DownmixToMono mixes multi-channel interleaved PCM samples into a single mono channel.
func (t *Transcoder) DownmixToMono(samples []int32, channels int) []int32 {
	if channels <= 1 {
		return samples
	}

	frames := len(samples) / channels
	mono := make([]int32, frames)

	for i := 0; i < frames; i++ {
		var sum int64
		for ch := 0; ch < channels; ch++ {
			sum += int64(samples[i*channels+ch])
		}
		mono[i] = int32(sum / int64(channels))
	}
	return mono
}

// ApplyVolume applies a linear amplitude multiplier based on volume percentage [0-100].
func (t *Transcoder) ApplyVolume(samples []int32, volumePercent int) []int32 {
	if volumePercent >= 100 {
		return samples
	}
	if volumePercent <= 0 {
		for i := range samples {
			samples[i] = 0
		}
		return samples
	}

	// Perceptual audio scaling curve (logarithmic / polynomial approximation)
	gain := float64(volumePercent) / 100.0
	factor := math.Pow(gain, 1.5)

	out := make([]int32, len(samples))
	for i, s := range samples {
		out[i] = int32(float64(s) * factor)
	}
	return out
}

// Resample converts mono PCM samples from srcRate to dstRate using libsoxr polyphase FIR filtering with anti-aliasing.
func (t *Transcoder) Resample(samples []int32, srcRate, dstRate int) []int32 {
	if srcRate == dstRate || len(samples) == 0 {
		return samples
	}

	inF32 := make([]float32, len(samples))
	for i, s := range samples {
		inF32[i] = float32(s)
	}

	// QualityLow provides 16-bit / 102dB SNR precision with full Nyquist anti-aliasing cutoff and SIMD acceleration
	outF32, err := resampler.ResampleMonoFloat32(inF32, float64(srcRate), float64(dstRate), resampler.QualityLow)
	if err != nil || len(outF32) == 0 {
		return samples
	}

	out := make([]int32, len(outF32))
	for i, f := range outF32 {
		out[i] = int32(f)
	}
	return out
}
