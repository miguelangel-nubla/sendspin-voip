package audio

import (
	"encoding/binary"
	"fmt"
	"math"
	"sync"

	"github.com/gotranspile/g722"
	"github.com/miguelangel-nubla/sendspin-voip/internal/domain"
	"github.com/thesyncim/gopus"
	resampler "github.com/tphakala/go-audio-resampler"
	"github.com/zaf/g711"
)

// Transcoder implements app.AudioTranscoderPort with high-performance pure Go transcoding.
// Instances are per-RTP-session: G.722 and Opus encoders are stateful.
type Transcoder struct {
	mu         sync.Mutex
	g722Enc    *g722.Encoder
	opusEnc    *gopus.Encoder
	opusEncCh  int
	opusDec    *gopus.Decoder
	opusDecCh  int
	opusPCMBuf []int16 // accumulates to Opus frame (960/ch @ 48 kHz / 20 ms)
}

// NewTranscoder creates a new audio transcoder.
func NewTranscoder() *Transcoder {
	return &Transcoder{}
}

// Reset clears any cached encoder state.
func (t *Transcoder) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.g722Enc = nil
	if t.opusEnc != nil {
		t.opusEnc.Reset()
	}
	t.opusEnc = nil
	t.opusEncCh = 0
	if t.opusDec != nil {
		t.opusDec.Reset()
	}
	t.opusDec = nil
	t.opusDecCh = 0
	t.opusPCMBuf = nil
}

// DecodeOpusToPCM decodes an Opus packet to interleaved int32 PCM at 48 kHz.
// channels should match the stream (1 or 2); invalid values default to 2.
func (t *Transcoder) DecodeOpusToPCM(opusData []byte, channels int) ([]int32, error) {
	if len(opusData) == 0 {
		return nil, nil
	}
	channels = domain.NormalizeChannels(channels, 2)

	t.mu.Lock()
	defer t.mu.Unlock()

	if t.opusDec == nil || t.opusDecCh != channels {
		dec, err := gopus.NewDecoder(gopus.DecoderConfig{
			SampleRate: 48000,
			Channels:   channels,
		})
		if err != nil {
			return nil, fmt.Errorf("opus decoder init: %w", err)
		}
		t.opusDec = dec
		t.opusDecCh = channels
	}

	// Up to 120ms stereo at 48 kHz
	pcm := make([]int16, 5760*channels)
	n, err := t.opusDec.DecodeInt16(opusData, pcm)
	if err != nil {
		return nil, fmt.Errorf("opus decode: %w", err)
	}
	if n <= 0 {
		return nil, nil
	}

	total := n * channels
	out := make([]int32, total)
	for i := 0; i < total; i++ {
		out[i] = int32(pcm[i])
	}
	return out, nil
}

// Transcode converts raw PCM samples to target codec payload with volume scaling.
// Mono-only SIP codecs (G.711/G.722/L16) downmix to mono. Opus preserves stereo.
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

	srcChannels = domain.NormalizeChannels(srcChannels, 2)
	if srcRate <= 0 {
		srcRate = 48000
	}

	// Opus: keep stereo when present (matches SDP opus/48000/2 and passthrough).
	if dstCodec == domain.CodecOpus {
		return t.transcodeOpus(samples, srcRate, srcChannels, volumePercent)
	}

	// Legacy VoIP codecs are mono-only.
	mono := t.DownmixToMono(samples, srcChannels)
	mono = t.ApplyVolume(mono, volumePercent)
	dstRate := dstCodec.SampleRate()
	resampled := t.Resample(mono, srcRate, dstRate)

	pcm16 := clampToInt16(resampled)

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

	case domain.CodecL16:
		payload := make([]byte, len(pcm16)*2)
		for i, v := range pcm16 {
			binary.BigEndian.PutUint16(payload[i*2:], uint16(v))
		}
		return payload, nil

	default:
		return nil, fmt.Errorf("unsupported destination codec: %s", dstCodec)
	}
}

func (t *Transcoder) transcodeOpus(samples []int32, srcRate, srcChannels, volumePercent int) ([]byte, error) {
	channels := srcChannels
	if channels != 1 && channels != 2 {
		// Odd layouts → stereo downmix of first 2 / average; safest is mono.
		samples = t.DownmixToMono(samples, srcChannels)
		channels = 1
	}

	samples = t.ApplyVolume(samples, volumePercent)

	if srcRate != 48000 {
		if channels == 1 {
			samples = t.Resample(samples, srcRate, 48000)
		} else {
			// Per-channel resample then re-interleave
			left := make([]int32, len(samples)/2)
			right := make([]int32, len(samples)/2)
			for i := 0; i < len(left); i++ {
				left[i] = samples[i*2]
				right[i] = samples[i*2+1]
			}
			left = t.Resample(left, srcRate, 48000)
			right = t.Resample(right, srcRate, 48000)
			n := len(left)
			if len(right) < n {
				n = len(right)
			}
			out := make([]int32, n*2)
			for i := 0; i < n; i++ {
				out[i*2] = left[i]
				out[i*2+1] = right[i]
			}
			samples = out
		}
	}

	return t.encodeOpus(clampToInt16(samples), channels)
}

func clampToInt16(samples []int32) []int16 {
	pcm16 := make([]int16, len(samples))
	for i, s := range samples {
		val := s
		if val > 32767 {
			val = 32767
		} else if val < -32768 {
			val = -32768
		}
		pcm16[i] = int16(val)
	}
	return pcm16
}

// encodeOpus encodes 48 kHz PCM (mono or stereo interleaved) into an Opus packet.
func (t *Transcoder) encodeOpus(pcm16 []int16, channels int) ([]byte, error) {
	channels = domain.NormalizeChannels(channels, 1)

	t.mu.Lock()
	defer t.mu.Unlock()

	if t.opusEnc == nil || t.opusEncCh != channels {
		enc, err := gopus.NewEncoder(gopus.EncoderConfig{
			SampleRate:  48000,
			Channels:    channels,
			Application: gopus.ApplicationAudio,
		})
		if err != nil {
			return nil, fmt.Errorf("opus encoder init: %w", err)
		}
		_ = enc.SetBitrate(96_000)
		t.opusEnc = enc
		t.opusEncCh = channels
		t.opusPCMBuf = nil
	}

	frameSize := t.opusEnc.FrameSize() // samples per channel
	if frameSize <= 0 {
		frameSize = 960
	}
	needed := frameSize * channels

	t.opusPCMBuf = append(t.opusPCMBuf, pcm16...)
	if len(t.opusPCMBuf) < needed {
		return nil, nil
	}

	frame := t.opusPCMBuf[:needed]
	t.opusPCMBuf = t.opusPCMBuf[needed:]
	if len(t.opusPCMBuf) == 0 {
		t.opusPCMBuf = t.opusPCMBuf[:0]
	}

	out := make([]byte, 4000)
	n, err := t.opusEnc.EncodeInt16(frame, out)
	if err != nil {
		return nil, fmt.Errorf("opus encode: %w", err)
	}
	if n <= 0 {
		return nil, nil
	}
	payload := make([]byte, n)
	copy(payload, out[:n])
	return payload, nil
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

// ApplyVolume applies perceptual (dB-tapered) gain for UI volume 0–100.
//
// A linear 50% amplitude cut is only −6 dB, which sounds nearly as loud as full
// scale. We map the slider onto a −60 dB … 0 dB fader instead (common for media
// players), so 50% ≈ −30 dB (~1/32 amplitude) and reads as clearly quieter.
//
// The returned slice is always safe for the caller to keep: this never writes
// through the input. That matters for mute, which used to zero in place —
// DownmixToMono returns its argument unchanged for mono input, so muting a
// mono stream overwrote the caller's own PCM frame.
func (t *Transcoder) ApplyVolume(samples []int32, volumePercent int) []int32 {
	if volumePercent >= 100 {
		return samples
	}
	if volumePercent <= 0 {
		return make([]int32, len(samples))
	}

	const minDB = -60.0
	db := (float64(volumePercent)/100.0)*(-minDB) + minDB // 0→-60, 100→0
	factor := math.Pow(10, db/20.0)

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
