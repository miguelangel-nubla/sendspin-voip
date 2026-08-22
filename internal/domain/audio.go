package domain

import (
	"fmt"
	"strings"
	"time"
)

// Codec represents an audio codec used for SIP/RTP streaming.
type Codec string

const (
	CodecPCMU Codec = "pcmu" // G.711 u-law (8kHz mono, payload type 0)
	CodecPCMA Codec = "pcma" // G.711 A-law (8kHz mono, payload type 8)
	CodecG722 Codec = "g722" // G.722 Wideband HD Voice (16kHz mono, payload type 9, RTP clock 8kHz)
	CodecOpus Codec = "opus" // Opus (48kHz, payload type 96/dynamic)
	CodecL16  Codec = "l16"  // Linear 16-bit uncompressed PCM (48kHz/44.1kHz, payload type 10/11/97)
)

// DefaultCodecPreferences is the canonical list of supported audio codecs in descending fidelity order.
var DefaultCodecPreferences = []Codec{
	CodecOpus,
	CodecL16,
	CodecG722,
	CodecPCMU,
	CodecPCMA,
}

// PrioritizeCodecs returns an ordered codec list.
// If preferred is set AND present in available, it is moved to the front.
// Preferred is never injected when it was not discovered/advertised in available.
// If available is empty/nil, DefaultCodecPreferences is used (preferred still only moves to front if listed there).
func PrioritizeCodecs(preferred Codec, available []Codec) []Codec {
	base := available
	if len(base) == 0 {
		base = DefaultCodecPreferences
	}

	res := make([]Codec, 0, len(base))
	seen := make(map[Codec]bool, len(base))
	for _, c := range base {
		if c == "" || seen[c] {
			continue
		}
		res = append(res, c)
		seen[c] = true
	}

	if preferred == "" || !seen[preferred] {
		return res
	}

	// Move preferred to front without adding undiscovered codecs.
	out := make([]Codec, 0, len(res))
	out = append(out, preferred)
	for _, c := range res {
		if c != preferred {
			out = append(out, c)
		}
	}
	return out
}

// ParseCodec normalizes and parses a codec string.
// "auto", "best", and empty string return an empty Codec (no preference — use discovery order).
func ParseCodec(s string) (Codec, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "auto", "best", "":
		return "", nil
	case "pcmu", "g711u", "ulaw", "u-law", "g711_ulaw":
		return CodecPCMU, nil
	case "pcma", "g711a", "alaw", "a-law", "g711_alaw":
		return CodecPCMA, nil
	case "g722", "g.722", "g722_64k":
		return CodecG722, nil
	case "opus":
		return CodecOpus, nil
	case "l16", "pcm16", "linear16", "pcm_s16be":
		return CodecL16, nil
	default:
		return "", fmt.Errorf("unsupported codec: %s (supported: auto, opus, l16, g722, pcmu, pcma)", s)
	}
}

// NormalizeSIPTarget canonicalizes a SIP URI for arbiter keying and comparisons.
func NormalizeSIPTarget(target string) string {
	t := strings.TrimSpace(strings.ToLower(target))
	t = strings.Trim(t, "<>")
	t = strings.TrimPrefix(t, "sips:")
	t = strings.TrimPrefix(t, "sip:")
	t = strings.TrimSpace(t)
	if t == "" {
		return ""
	}
	return "sip:" + t
}

// PayloadType returns the standard RFC RTP payload type for the codec.
func (c Codec) PayloadType() uint8 {
	switch c {
	case CodecPCMU:
		return 0
	case CodecPCMA:
		return 8
	case CodecG722:
		return 9
	case CodecOpus:
		return 96
	case CodecL16:
		return 97
	default:
		return 0
	}
}

// SampleRate returns the actual audio sample rate required by the encoder.
func (c Codec) SampleRate() int {
	switch c {
	case CodecPCMU, CodecPCMA:
		return 8000
	case CodecG722:
		return 16000
	case CodecOpus, CodecL16:
		return 48000
	default:
		return 8000
	}
}

// RTPClockRate returns the clock rate advertised in SDP and used for RTP timestamps.
// Note: G.722 uses 8000 in RFC 3551 for historic reasons even though it samples at 16000.
func (c Codec) RTPClockRate() uint32 {
	switch c {
	case CodecPCMU, CodecPCMA:
		return 8000
	case CodecG722:
		return 8000 // RFC 3551 legacy clock rate
	case CodecOpus, CodecL16:
		return 48000
	default:
		return 8000
	}
}

// NormalizeChannels clamps an announced channel count to a valid layout (1 or 2),
// falling back to fallback (and finally stereo) when given an unsupported value.
func NormalizeChannels(channels, fallback int) int {
	if channels == 1 || channels == 2 {
		return channels
	}
	if fallback == 1 || fallback == 2 {
		return fallback
	}
	return 2
}

// FormatAudioDescription returns a human-readable stream description for arbitrary audio formats.
func FormatAudioDescription(codec string, sampleRate, channels, bitDepth int) string {
	codec = strings.ToUpper(strings.TrimSpace(codec))
	if codec == "" {
		codec = "PCM"
	}
	channels = NormalizeChannels(channels, 2)
	if sampleRate <= 0 {
		sampleRate = 48000
	}
	if codec == "OPUS" {
		return fmt.Sprintf("OPUS %dHz %dch", sampleRate, channels)
	}
	if bitDepth <= 0 {
		bitDepth = 16
	}
	return fmt.Sprintf("%s %dHz %dch %dbit", codec, sampleRate, channels, bitDepth)
}

// CalculateBitrateKbps estimates audio bitrate in kbps for the given stream parameters.
func CalculateBitrateKbps(codec string, sampleRate, channels, bitDepth int) int {
	if strings.EqualFold(codec, "opus") {
		return 128
	}
	if sampleRate <= 0 {
		sampleRate = 48000
	}
	channels = NormalizeChannels(channels, 2)
	if bitDepth <= 0 {
		bitDepth = 16
	}
	return (sampleRate * channels * bitDepth) / 1000
}

// AudioChunk represents audio from Sendspin (either raw PCM samples or native Opus data) with timestamp information.
type AudioChunk struct {
	Timestamp  int64     // Server timestamp in microseconds
	PlayAt     time.Time // Local playback target time computed by clock sync
	Samples    []int32   // Interleaved PCM samples (scaled to 16-bit range)
	OpusData   []byte    // Native Opus encoded frame (for passthrough)
	SampleRate int       // Source sample rate (e.g. 44100, 48000)
	Channels   int       // Source channels (e.g. 1, 2)
	BitDepth   int       // Source bit depth (e.g. 16, 24)
}

// DisplayName returns an uppercase presentation name for the codec.
func (c Codec) DisplayName() string {
	switch c {
	case CodecG722:
		return "G.722"
	case CodecPCMU:
		return "G.711u"
	case CodecPCMA:
		return "G.711a"
	default:
		return strings.ToUpper(string(c))
	}
}

// DefaultBitrateKbps returns the standard expected bitrate in kbps.
func (c Codec) DefaultBitrateKbps() int {
	switch c {
	case CodecOpus:
		return 128
	case CodecL16:
		return 1536
	default:
		return 64
	}
}

// DefaultChannels returns the standard channel layout count.
func (c Codec) DefaultChannels() int {
	switch c {
	case CodecOpus, CodecL16:
		return 2
	default:
		return 1
	}
}

// FormatDescription returns a human-readable stream description, e.g. "G.722 16000Hz 1ch (64 kbps)".
func (c Codec) FormatDescription(channels, bitrateKbps int) string {
	if channels <= 0 {
		channels = c.DefaultChannels()
	}
	if bitrateKbps <= 0 {
		bitrateKbps = c.DefaultBitrateKbps()
	}
	return fmt.Sprintf("%s %dHz %dch (%d kbps)", c.DisplayName(), c.SampleRate(), channels, bitrateKbps)
}

// SDPDescription returns SDP summary string, e.g. "OPUS (pt=96, clock=48000Hz)".
func (c Codec) SDPDescription() string {
	return fmt.Sprintf("%s (pt=%d, clock=%dHz)", strings.ToUpper(string(c)), c.PayloadType(), c.RTPClockRate())
}

// FullName returns the expanded title for the codec used in documentation and web UI.
func (c Codec) FullName() string {
	switch c {
	case CodecOpus:
		return "Opus Interactive Audio"
	case CodecL16:
		return "L16 Linear PCM (Uncompressed)"
	case CodecG722:
		return "G.722 HD Voice"
	case CodecPCMU:
		return "G.711 µ-law (PCMU)"
	case CodecPCMA:
		return "G.711 A-law (PCMA)"
	default:
		return c.DisplayName()
	}
}

// SDPEncodingName returns the standard RFC 4566 / 3551 rtpmap encoding parameter (e.g. "opus/48000/2").
func (c Codec) SDPEncodingName() string {
	switch c {
	case CodecOpus:
		return "opus/48000/2"
	case CodecL16:
		return "L16/48000/1"
	case CodecG722:
		return "G722/8000"
	case CodecPCMU:
		return "PCMU/8000"
	case CodecPCMA:
		return "PCMA/8000"
	default:
		return string(c)
	}
}

// LongDescription returns a descriptive summary of the codec capabilities.
func (c Codec) LongDescription() string {
	switch c {
	case CodecOpus:
		return "High fidelity multi-room audio with zero-copy passthrough when volume is 100%"
	case CodecL16:
		return "Studio master uncompressed linear PCM audio streaming"
	case CodecG722:
		return "Wideband HD VoIP codec with crystal clear speech synthesis"
	case CodecPCMU:
		return "Universal standard telephony codec (North America/Japan standard)"
	case CodecPCMA:
		return "Universal standard telephony codec (International/European standard)"
	default:
		return ""
	}
}

