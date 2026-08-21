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

// ParseCodec normalizes and parses a codec string.
func ParseCodec(s string) (Codec, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "auto", "best", "":
		return CodecOpus, nil
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

// RTPPacket represents an encoded audio packet ready for network transmission.
type RTPPacket struct {
	Payload   []byte
	Timestamp uint32
	Sequence  uint16
	Duration  time.Duration
}
