package rtp

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/miguelangel-nubla/sendspin-voip/internal/app"
	"github.com/miguelangel-nubla/sendspin-voip/internal/domain"
)

// ReadyFrame represents a single 20ms audio frame converted and ready for playout.
type ReadyFrame struct {
	Payload        []byte
	PlayAt         time.Time
	TimestampUs    int64
	Passthrough    bool
	Codec          domain.Codec
	SampleRate     int
	Channels       int
	ChunksConsumed int
}

// AudioPath represents Layer 2 of the audio pipeline.
// It is the DSP & Conversion engine that reads raw audio from UpstreamPlayer,
// applies gain and codec encoding, and maintains an ahead-of-time buffer of converted 20ms ready frames.
type AudioPath struct {
	mu         sync.Mutex
	transcoder app.AudioTranscoderPort
	upstream   *UpstreamPlayer
	codec      domain.Codec
	volume     int // 0 to 100

	// Intermediate 20ms sample accumulator for PCM
	pcmBuffer          []int32
	pcmSampleRate      int
	pcmChannels        int
	pcmBitDepth        int
	pcmTimestampUs     int64
	pcmPlayAt          time.Time
	pcmAccumulatedTime time.Duration
	chunksPendingCount int
	ingressRawCodec     string
	ingressRawRate      int
	ingressRawChannels  int
	ingressRawBitDepth  int
	ingressRawBytesRate int

	// Converted 20ms ready frames buffer
	ready []ReadyFrame

	// Telemetry & diagnostics
	pathMode            string
	pathVolumePercent   int
	pathIngressCodec    string
	pathIngressRate     int
	pathIngressChannels int
	pathDecodedLocally  bool
	passthroughPackets  uint64
	transcodePackets    uint64
}

// NewAudioPath creates a new AudioPath conversion engine wrapping an UpstreamPlayer raw timeline.
func NewAudioPath(transcoder app.AudioTranscoderPort, upstream *UpstreamPlayer, codec domain.Codec, initialVolume int) *AudioPath {
	if initialVolume < 0 {
		initialVolume = 0
	}
	if initialVolume > 100 {
		initialVolume = 100
	}
	return &AudioPath{
		transcoder: transcoder,
		upstream:   upstream,
		codec:      codec,
		volume:     initialVolume,
		ready:      make([]ReadyFrame, 0, 32),
	}
}

// SetCodec updates the target transmission codec.
func (a *AudioPath) SetCodec(codec domain.Codec) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.codec == codec {
		return
	}
	a.codec = codec
	a.ready = nil
	a.pcmBuffer = nil
	a.chunksPendingCount = 0
	if resetter, ok := a.transcoder.(interface{ Reset() }); ok {
		resetter.Reset()
	}
	if a.upstream != nil {
		a.upstream.RewindRead()
	}
}

// SetVolume updates the output volume (0-100).
// It flushes the converted ready frame buffer and rewinds the UpstreamPlayer read cursor,
// ensuring all unplayed raw audio is re-transcoded with the new gain without skipping.
func (a *AudioPath) SetVolume(volumePercent int) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if volumePercent > 100 {
		volumePercent = 100
	}
	if volumePercent < 0 {
		volumePercent = 0
	}
	if a.volume == volumePercent {
		return
	}
	a.volume = volumePercent
	a.ready = nil
	a.pcmBuffer = nil
	a.chunksPendingCount = 0
	if resetter, ok := a.transcoder.(interface{ Reset() }); ok {
		resetter.Reset()
	}
	if a.upstream != nil {
		a.upstream.RewindRead()
	}
}

// Fill pulls raw chunks from the encapsulated UpstreamPlayer using the read cursor
// and converts them into 20ms ready frames until the ready buffer reaches maxReadyFrames.
func (a *AudioPath) Fill(maxReadyFrames int) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.upstream == nil {
		return nil
	}

	for len(a.ready) < maxReadyFrames {
		chunk, ok := a.upstream.PeekNext()
		if !ok {
			break
		}
		a.chunksPendingCount++
		if err := a.processChunkLocked(chunk); err != nil {
			a.chunksPendingCount--
			return err
		}
		a.upstream.AdvanceRead()
	}
	return nil
}

// ProcessChunkLocked converts a raw chunk into ready frames (caller must hold a.mu).
func (a *AudioPath) processChunkLocked(chunk domain.AudioChunk) error {
	inCodec := "pcm"
	if len(chunk.OpusData) > 0 {
		inCodec = "opus"
	}

	inRate := chunk.SampleRate
	if inRate <= 0 {
		inRate = 48000
	}
	inChannels := chunk.Channels
	if inChannels <= 0 {
		inChannels = 2
	}
	inBitDepth := chunk.BitDepth
	if inBitDepth <= 0 {
		inBitDepth = 16
	}

	a.ingressRawCodec = inCodec
	a.ingressRawRate = inRate
	a.ingressRawChannels = inChannels
	a.ingressRawBitDepth = inBitDepth

	// 1. Opus Passthrough path: when input has OpusData, target codec is Opus, and volume is 100% (0 dB)
	if len(chunk.OpusData) > 0 && a.codec == domain.CodecOpus && a.volume >= 100 {
		payload := append([]byte(nil), chunk.OpusData...)
		consumed := a.chunksPendingCount
		a.chunksPendingCount = 0
		a.ready = append(a.ready, ReadyFrame{
			Payload:        payload,
			PlayAt:         chunk.PlayAt,
			TimestampUs:    chunk.Timestamp,
			Passthrough:    true,
			Codec:          domain.CodecOpus,
			SampleRate:     48000,
			Channels:       inChannels,
			ChunksConsumed: consumed,
		})
		a.passthroughPackets++
		a.pathMode = "opus_passthrough"
		a.pathVolumePercent = a.volume
		a.pathIngressCodec = inCodec
		a.pathIngressRate = inRate
		a.pathIngressChannels = inChannels
		a.pathDecodedLocally = false
		return nil
	}

	// 2. Transcode path
	var pcmSamples []int32
	var decodedLocally bool

	if len(chunk.Samples) > 0 {
		pcmSamples = chunk.Samples
	} else if len(chunk.OpusData) > 0 {
		decoded, err := a.transcoder.DecodeOpusToPCM(chunk.OpusData, inChannels)
		if err != nil {
			return fmt.Errorf("opus decode failed: %w", err)
		}
		pcmSamples = decoded
		decodedLocally = true
	}

	if len(pcmSamples) == 0 {
		return nil
	}

	if a.pcmSampleRate != 0 && (a.pcmSampleRate != inRate || a.pcmChannels != inChannels) {
		// Flush any leftover tail samples from previous track before switching format
		a.flushPcmTailLocked()
		a.pcmBuffer = nil
		a.pcmAccumulatedTime = 0
	}
	a.pcmSampleRate = inRate
	a.pcmChannels = inChannels
	a.pcmBitDepth = inBitDepth

	if len(a.pcmBuffer) == 0 {
		a.pcmTimestampUs = chunk.Timestamp
		a.pcmPlayAt = chunk.PlayAt
		a.pcmAccumulatedTime = 0
	}

	a.pcmBuffer = append(a.pcmBuffer, pcmSamples...)
	samplesPer20ms := (inRate * 20 / 1000) * inChannels
	if samplesPer20ms <= 0 {
		return nil
	}

	for len(a.pcmBuffer) >= samplesPer20ms {
		frameSamples := a.pcmBuffer[:samplesPer20ms]
		a.pcmBuffer = a.pcmBuffer[samplesPer20ms:]

		framePlayAt := a.pcmPlayAt
		if !framePlayAt.IsZero() {
			framePlayAt = framePlayAt.Add(a.pcmAccumulatedTime)
		}
		frameTimestampUs := a.pcmTimestampUs + a.pcmAccumulatedTime.Microseconds()
		a.pcmAccumulatedTime += 20 * time.Millisecond

		encoded, err := a.transcoder.Transcode(
			frameSamples,
			inRate,
			inChannels,
			a.codec,
			a.volume,
		)
		if err != nil {
			return fmt.Errorf("transcode to %s failed: %w", a.codec, err)
		}

		outCh := 1
		if a.codec == domain.CodecOpus && inChannels == 2 {
			outCh = 2
		}

		consumed := a.chunksPendingCount
		a.chunksPendingCount = 0

		a.ready = append(a.ready, ReadyFrame{
			Payload:        encoded,
			PlayAt:         framePlayAt,
			TimestampUs:    frameTimestampUs,
			Passthrough:    false,
			Codec:          a.codec,
			SampleRate:     a.codec.SampleRate(),
			Channels:       outCh,
			ChunksConsumed: consumed,
		})
		a.transcodePackets++
	}

	a.pathMode = "transcode"
	a.pathVolumePercent = a.volume
	a.pathIngressCodec = inCodec
	a.pathIngressRate = inRate
	a.pathIngressChannels = inChannels
	a.pathDecodedLocally = decodedLocally

	return nil
}

func (a *AudioPath) flushPcmTailLocked() {
	if len(a.pcmBuffer) == 0 || a.pcmSampleRate <= 0 {
		return
	}
	samplesPer20ms := (a.pcmSampleRate * 20 / 1000) * a.pcmChannels
	if samplesPer20ms <= 0 {
		return
	}

	// Pad remainder with silence up to 20ms
	tail := make([]int32, samplesPer20ms)
	copy(tail, a.pcmBuffer)

	encoded, err := a.transcoder.Transcode(
		tail,
		a.pcmSampleRate,
		a.pcmChannels,
		a.codec,
		a.volume,
	)
	if err != nil {
		return
	}

	framePlayAt := a.pcmPlayAt
	if !framePlayAt.IsZero() {
		framePlayAt = framePlayAt.Add(a.pcmAccumulatedTime)
	}

	outCh := 1
	if a.codec == domain.CodecOpus && a.pcmChannels == 2 {
		outCh = 2
	}

	consumed := a.chunksPendingCount
	a.chunksPendingCount = 0

	a.ready = append(a.ready, ReadyFrame{
		Payload:        encoded,
		PlayAt:         framePlayAt,
		TimestampUs:    a.pcmTimestampUs + a.pcmAccumulatedTime.Microseconds(),
		Passthrough:    false,
		Codec:          a.codec,
		SampleRate:     a.codec.SampleRate(),
		Channels:       outCh,
		ChunksConsumed: consumed,
	})
	a.transcodePackets++
}

// PopReady retrieves and removes the oldest 20ms ready frame, automatically
// acknowledging completed raw chunks in the underlying UpstreamPlayer.
func (a *AudioPath) PopReady() (ReadyFrame, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if len(a.ready) == 0 {
		return ReadyFrame{}, false
	}
	frame := a.ready[0]
	a.ready = a.ready[1:]
	if a.upstream != nil && frame.ChunksConsumed > 0 {
		a.upstream.AcknowledgePlayed(frame.ChunksConsumed)
	}
	return frame, true
}

// PeekReady returns the oldest ready frame without removing it.
func (a *AudioPath) PeekReady() (ReadyFrame, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if len(a.ready) == 0 {
		return ReadyFrame{}, false
	}
	return a.ready[0], true
}

// DiscardBefore purges stale raw chunks from the UpstreamPlayer before conversion,
// and instantly drops any already-converted ready frames whose scheduled PlayAt is earlier than cutoff.
func (a *AudioPath) DiscardBefore(cutoff time.Time) int {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.upstream != nil {
		a.upstream.DiscardBefore(cutoff)
	}

	dropped := 0
	for len(a.ready) > 0 {
		f := a.ready[0]
		if !f.PlayAt.IsZero() && f.PlayAt.Before(cutoff) {
			a.ready = a.ready[1:]
			if a.upstream != nil && f.ChunksConsumed > 0 {
				a.upstream.AcknowledgePlayed(f.ChunksConsumed)
			}
			dropped++
		} else {
			break
		}
	}
	return dropped
}

// ReadyLen returns the count of queued 20ms ready frames.
func (a *AudioPath) ReadyLen() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.ready)
}

// Clear flushes all converted ready frames, sample accumulators, and resets the UpstreamPlayer.
func (a *AudioPath) Clear() {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.ready = nil
	a.pcmBuffer = nil
	a.chunksPendingCount = 0
	if resetter, ok := a.transcoder.(interface{ Reset() }); ok {
		resetter.Reset()
	}
	if a.upstream != nil {
		a.upstream.Clear()
	}
}

// StatsSnapshot captures telemetry for AudioPath.
type AudioPathStats struct {
	Mode               string
	Summary            string
	Stages             []string
	VolumePercent      int
	IngressCodec       string
	IngressRate        int
	IngressChannels    int
	PassthroughPackets uint64
	TranscodePackets   uint64
	ReadyFrames        int
	ReadyStartUs       int64
	ReadyEndUs         int64
}

// ReadyPlayAtBounds returns the wall-clock PlayAt timestamps of the oldest and newest ready frames.
func (a *AudioPath) ReadyPlayAtBounds() (oldest, newest time.Time, count int) {
	a.mu.Lock()
	defer a.mu.Unlock()

	n := len(a.ready)
	if n == 0 {
		return time.Time{}, time.Time{}, 0
	}
	return a.ready[0].PlayAt, a.ready[n-1].PlayAt, n
}

// Stats returns a snapshot of AudioPath diagnostics.
func (a *AudioPath) Stats() AudioPathStats {
	a.mu.Lock()
	defer a.mu.Unlock()

	var readyStartUs, readyEndUs int64
	if len(a.ready) > 0 {
		readyStartUs = a.ready[0].TimestampUs
		readyEndUs = a.ready[len(a.ready)-1].TimestampUs
	}

	summary, stages := a.buildPathDiagnosticsLocked()

	return AudioPathStats{
		Mode:               a.pathMode,
		Summary:            summary,
		Stages:             stages,
		VolumePercent:      a.pathVolumePercent,
		IngressCodec:       a.pathIngressCodec,
		IngressRate:        a.pathIngressRate,
		IngressChannels:    a.pathIngressChannels,
		PassthroughPackets: a.passthroughPackets,
		TranscodePackets:   a.transcodePackets,
		ReadyFrames:        len(a.ready),
		ReadyStartUs:       readyStartUs,
		ReadyEndUs:         readyEndUs,
	}
}

func (a *AudioPath) buildPathDiagnosticsLocked() (string, []string) {
	if a.pathMode == "" {
		return "", nil
	}

	inRate := a.pathIngressRate
	if inRate <= 0 {
		inRate = 48000
	}
	inChannels := a.pathIngressChannels
	if inChannels <= 0 {
		inChannels = 2
	}
	inCodec := a.pathIngressCodec
	if inCodec == "" {
		inCodec = "pcm"
	}

	outCh := 1
	if a.codec == domain.CodecOpus && inChannels == 2 {
		outCh = 2
	}

	inLabel := fmt.Sprintf("%s %dHz %dch", strings.ToUpper(inCodec), inRate, inChannels)
	outLabel := fmt.Sprintf("%s %dHz %dch", strings.ToUpper(string(a.codec)), a.codec.SampleRate(), outCh)
	volLabel := volumeStageLabel(a.pathVolumePercent)

	if a.pathMode == "opus_passthrough" {
		stages := []string{
			"ingress " + inLabel,
			"passthrough (no decode/encode, " + volLabel + ")",
			"RTP " + outLabel,
		}
		summary := inLabel + " → passthrough → RTP " + strings.ToUpper(string(a.codec))
		return summary, stages
	}

	var stages []string
	stages = append(stages, "ingress "+inLabel)
	if inCodec == "opus" || a.pathDecodedLocally {
		if a.pathDecodedLocally {
			stages = append(stages, "decode Opus → PCM")
		} else {
			stages = append(stages, "PCM from ingress Opus decode")
		}
	}
	if a.codec != domain.CodecOpus && inChannels > 1 {
		stages = append(stages, "downmix → mono")
	}
	stages = append(stages, volLabel)
	if a.codec == domain.CodecOpus {
		if inRate != 48000 {
			stages = append(stages, fmt.Sprintf("resample %dHz → 48000Hz", inRate))
		}
		chLabel := "mono"
		if outCh == 2 {
			chLabel = "stereo"
		}
		stages = append(stages, "encode OPUS ("+chLabel+")")
	} else {
		if inRate != a.codec.SampleRate() {
			stages = append(stages, fmt.Sprintf("resample %dHz → %dHz", inRate, a.codec.SampleRate()))
		}
		stages = append(stages, "encode "+strings.ToUpper(string(a.codec))+" (mono)")
	}
	stages = append(stages, "RTP "+outLabel)
	summary := inLabel + " → transcode (" + volLabel + ") → RTP " + strings.ToUpper(string(a.codec))
	return summary, stages
}

func volumeStageLabel(vol int) string {
	if vol <= 0 {
		return "volume mute"
	}
	if vol >= 100 {
		return "volume 100% (0 dB)"
	}
	db := float64(vol)/100.0*60.0 - 60.0
	return fmt.Sprintf("volume %d%% (%.1f dB)", vol, db)
}
