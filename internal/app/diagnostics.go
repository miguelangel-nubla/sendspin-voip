package app

import (
	"cmp"
	"fmt"
	"time"

	"github.com/miguelangel-nubla/sendspin-voip/internal/domain"
)

// AudioPathDebugInfo describes the end-to-end processing pipeline for a stream.
type AudioPathDebugInfo struct {
	Mode               string   `json:"mode"` // idle | buffering | opus_passthrough | transcode | dialing
	Summary            string   `json:"summary"`
	Stages             []string `json:"stages"`
	Passthrough        bool     `json:"passthrough"`
	VolumePercent      int      `json:"volume_percent"`
	Muted              bool     `json:"muted"`
	IngressCodec       string   `json:"ingress_codec,omitempty"`
	IngressFormat      string   `json:"ingress_format,omitempty"`
	EgressCodec        string   `json:"egress_codec,omitempty"`
	EgressFormat       string   `json:"egress_format,omitempty"`
	UpstreamChunks     int      `json:"upstream_chunks,omitempty"`
	ConversionQueue    int      `json:"conversion_queue,omitempty"`
	PassthroughPackets uint64   `json:"passthrough_packets,omitempty"`
	TranscodePackets   uint64   `json:"transcode_packets,omitempty"`
	BufferStartSec     float64  `json:"buffer_start_sec,omitempty"`
	BufferEndSec       float64  `json:"buffer_end_sec,omitempty"`
	ReadyStartSec      float64  `json:"ready_start_sec,omitempty"`
	ReadyEndSec        float64  `json:"ready_end_sec,omitempty"`
	PlayheadSec        float64  `json:"playhead_sec,omitempty"`
}

// ProducerDebugInfo details the upstream audio ingress source.
type ProducerDebugInfo struct {
	Type             string   `json:"type"` // "Sendspin Ingress"
	URL              string   `json:"url"`
	Connected        bool     `json:"connected"`
	Format           string   `json:"format"` // e.g. "PCM 16000Hz 1ch 16bit" or "Opus 48000Hz 2ch"
	Codec            string   `json:"codec"`
	SampleRate       int      `json:"sample_rate"`
	Channels         int      `json:"channels"`
	BitDepth         int      `json:"bit_depth"`
	BitrateKbps      int      `json:"bitrate_kbps,omitempty"`
	OfferedFormats   []string `json:"offered_formats,omitempty"` // e.g. ["PCM 16000Hz 1ch 16bit", "PCM 48000Hz 2ch 16bit"]
	ExposedCodecs    []string `json:"exposed_codecs,omitempty"`  // e.g. ["opus", "pcm"]
	State            string   `json:"state"`
	Track            string   `json:"track,omitempty"`
	Artist           string   `json:"artist,omitempty"`
	Title            string   `json:"title,omitempty"`
	Album            string   `json:"album,omitempty"`
	AlbumArtist      string   `json:"album_artist,omitempty"`
	TrackDurationSec float64  `json:"track_duration_sec,omitempty"`
	TrackProgressSec float64  `json:"track_progress_sec,omitempty"`
	ChunksReceived   uint64   `json:"chunks_received"`
	BytesReceived    uint64   `json:"bytes_received"`
}

// ConsumerDebugInfo details the downstream SIP/RTP egress destination.
type ConsumerDebugInfo struct {
	Type             string   `json:"type"` // "SIP/RTP Egress"
	URL              string   `json:"url"`  // e.g. "sip:8003@asterisk.local.myol.es"
	CallID           string   `json:"call_id,omitempty"`
	State            string   `json:"state"`
	ConfigCodec      string   `json:"config_codec"`
	ActiveCodec      string   `json:"active_codec"`
	DiscoveredCodecs []string `json:"discovered_codecs,omitempty"` // Discovered via SIP OPTIONS probe
	OfferedCodecs    []string `json:"offered_codecs,omitempty"`    // SDP Codecs offered in INVITE
	NegotiatedSDP    string   `json:"negotiated_sdp,omitempty"`
	RTPClockRate     uint32   `json:"rtp_clock_rate,omitempty"`
	PayloadType      uint8    `json:"payload_type,omitempty"`
	Format           string   `json:"format"` // e.g. "G.722 16000Hz 1ch (64 kbps)"
	LocalRTP         string   `json:"local_rtp,omitempty"`
	RemoteRTP        string   `json:"remote_rtp,omitempty"`
	AutoAnswer       string   `json:"auto_answer,omitempty"`
	Priority         int      `json:"priority"`
	LingerActive     bool     `json:"linger_active"`
	PacketsSent      uint64   `json:"packets_sent"`
	BytesSent        uint64   `json:"bytes_sent"`
	BitrateKbps      int      `json:"bitrate_kbps,omitempty"`
	DurationSec      float64  `json:"duration_sec"`
	RemoteJitterMs   float64  `json:"remote_jitter_ms,omitempty"`
	FractionLost     float64  `json:"fraction_lost,omitempty"`
	RoundTripTimeMs  float64  `json:"round_trip_time_ms,omitempty"`
}

// StreamDebugInfo provides comprehensive debug information for a virtual player stream.
type StreamDebugInfo struct {
	ID        string              `json:"id"`
	Name      string              `json:"name"`
	State     string              `json:"state"` // "idle", "dialing", "playing", "lingering", "paused"
	IsPlaying bool                `json:"is_playing"`
	IsGrouped bool                `json:"is_grouped"`
	Volume    int                 `json:"volume"`
	Muted     bool                `json:"muted"`
	AudioPath AudioPathDebugInfo  `json:"audio_path"`
	Producers []ProducerDebugInfo `json:"producers"`
	Consumers []ConsumerDebugInfo `json:"consumers"`
}

// IsActive returns true if the stream is currently streaming, dialing, lingering, or active.
func (s StreamDebugInfo) IsActive() bool {
	return s.IsPlaying || s.State == "active" || s.State == "dialing" || s.State == "lingering" || s.State == "playing"
}

type callDiagnosticsSnapshot struct {
	hasCall            bool
	sessionState       string
	priority           int
	lingerActive       bool
	answered           bool
	startTime          time.Time
	answerTime         time.Time
	streamStartProgSec float64
	callID             string
	rtpStats           RTPStats
}

// buildStreamDebugInfo compiles real-time diagnostics for a virtual player stream.
func buildStreamDebugInfo(
	player *domain.Player,
	ingStats IngressPlayerStats,
	hasIngress bool,
	discoveredCodecs []string,
	call callDiagnosticsSnapshot,
) StreamDebugInfo {
	cfg := player.Config
	vol := player.Volume
	muted := player.IsMuted
	isPlaying := player.IsPlaying
	isGrouped := player.IsGrouped
	effectiveVol := player.EffectiveVolume()

	prodInfo, prodFormat, bitrateIn, trackProgSec := buildProducerInfo(player, ingStats)

	allCodecs := domain.PrioritizeCodecs(cfg.Codec, nil)
	offeredSIPCodecs := make([]string, len(allCodecs))
	for i, c := range allCodecs {
		offeredSIPCodecs[i] = c.SDPDescription()
	}

	autoAnswerDesc := string(cfg.AutoAnswer)
	if cfg.CustomAutoAnswerHeader != "" {
		autoAnswerDesc = fmt.Sprintf("custom (%s)", cfg.CustomAutoAnswerHeader)
	}

	consumer, audioPath, state := buildConsumerAndAudioPathInfo(
		cfg, call, discoveredCodecs, offeredSIPCodecs, autoAnswerDesc,
		prodFormat, bitrateIn, trackProgSec, effectiveVol, muted,
	)

	return StreamDebugInfo{
		ID:        cfg.ID,
		Name:      cfg.Name,
		State:     state,
		IsPlaying: isPlaying,
		IsGrouped: isGrouped,
		Volume:    vol,
		Muted:     muted,
		AudioPath: audioPath,
		Producers: []ProducerDebugInfo{prodInfo},
		Consumers: []ConsumerDebugInfo{consumer},
	}
}

func buildProducerInfo(player *domain.Player, ingStats IngressPlayerStats) (ProducerDebugInfo, string, int, float64) {
	prodState := "disconnected"
	if ingStats.Connected {
		if player.IsPlaying {
			prodState = "streaming"
		} else {
			prodState = "connected"
		}
	}

	trackStr := ingStats.Metadata.TrackDisplay()
	prodFormat := ingStats.FormatDescription()
	bitrateIn := ingStats.BitrateKbps()
	ingressURL := ingStats.WebSocketURL()
	trackProgSec := ingStats.Metadata.ElapsedSeconds(player.IsPlaying)

	prod := ProducerDebugInfo{
		Type:             "Sendspin Ingress",
		URL:              ingressURL,
		Connected:        ingStats.Connected,
		Format:           prodFormat,
		Codec:            ingStats.Codec,
		SampleRate:       ingStats.SampleRate,
		Channels:         ingStats.Channels,
		BitDepth:         ingStats.BitDepth,
		BitrateKbps:      bitrateIn,
		OfferedFormats:   ingStats.OfferedFormats,
		ExposedCodecs:    ingStats.ExposedCodecs,
		State:            prodState,
		Track:            trackStr,
		Artist:           ingStats.Metadata.Artist,
		Title:            ingStats.Metadata.Title,
		Album:            ingStats.Metadata.Album,
		AlbumArtist:      ingStats.Metadata.AlbumArtist,
		TrackDurationSec: ingStats.Metadata.Duration.Seconds(),
		TrackProgressSec: trackProgSec,
		ChunksReceived:   ingStats.ChunksReceived,
		BytesReceived:    ingStats.BytesReceived,
	}

	return prod, prodFormat, bitrateIn, trackProgSec
}

func buildConsumerAndAudioPathInfo(
	cfg domain.PlayerConfig,
	call callDiagnosticsSnapshot,
	discoveredCodecs, offeredSIPCodecs []string,
	autoAnswerDesc, prodFormat string,
	bitrateIn int,
	trackProgSec float64,
	effectiveVol int,
	muted bool,
) (ConsumerDebugInfo, AudioPathDebugInfo, string) {
	activeCodec := cfg.Codec
	if call.hasCall && call.rtpStats.Codec != "" {
		activeCodec = call.rtpStats.Codec
	}

	bitrateOut := 64
	egressCh := 1
	if activeCodec == domain.CodecOpus {
		bitrateOut = cmp.Or(bitrateIn, 128)
		egressCh = 2
		if call.rtpStats.PathIngressChannels == 1 {
			egressCh = 1
		}
	} else if !call.hasCall {
		bitrateOut = cfg.Codec.DefaultBitrateKbps()
	}

	egressFormat := activeCodec.FormatDescription(egressCh, bitrateOut)

	ap := AudioPathDebugInfo{
		Muted:         muted,
		VolumePercent: effectiveVol,
		IngressFormat: prodFormat,
		EgressCodec:   string(activeCodec),
		EgressFormat:  egressFormat,
		PlayheadSec:   trackProgSec,
	}

	consumer := ConsumerDebugInfo{
		Type:             "SIP/RTP Egress",
		URL:              cfg.SIPTarget,
		ConfigCodec:      string(cfg.Codec),
		ActiveCodec:      string(activeCodec),
		DiscoveredCodecs: discoveredCodecs,
		OfferedCodecs:    offeredSIPCodecs,
		NegotiatedSDP:    activeCodec.SDPDescription(),
		RTPClockRate:     activeCodec.RTPClockRate(),
		PayloadType:      activeCodec.PayloadType(),
		Format:           egressFormat,
		AutoAnswer:       autoAnswerDesc,
		Priority:         cfg.Priority,
		BitrateKbps:      bitrateOut,
	}

	if !call.hasCall {
		state := "idle"
		consumer.State = state
		ap.Summary = "idle — " + prodFormat + " ⇢ (no call) ⇢ " + cfg.Codec.DisplayName()
		ap.Stages = []string{
			"Stage 1: Upstream Ingress (" + prodFormat + ")",
			"Stage 2: No active SIP session",
			"Stage 3: Configured Egress " + cfg.Codec.DisplayName(),
		}
		return consumer, ap, state
	}

	// Active call configuration
	state := "idle"
	if call.lingerActive {
		state = "lingering"
	} else if call.sessionState == string(domain.StateActive) {
		state = "active"
	} else if call.sessionState == string(domain.StateDialing) {
		state = "dialing"
	}

	var durationSec float64
	if !call.answerTime.IsZero() {
		durationSec = time.Since(call.answerTime).Seconds()
	} else if !call.startTime.IsZero() {
		durationSec = time.Since(call.startTime).Seconds()
	}

	var localRTPStr string
	if call.rtpStats.LocalPort > 0 {
		localRTPStr = fmt.Sprintf("0.0.0.0:%d", call.rtpStats.LocalPort)
	}

	consumer.CallID = call.callID
	consumer.State = state
	consumer.Priority = call.priority
	consumer.LocalRTP = localRTPStr
	consumer.RemoteRTP = call.rtpStats.RemoteAddr
	consumer.LingerActive = call.lingerActive
	consumer.PacketsSent = call.rtpStats.PacketsSent
	consumer.BytesSent = call.rtpStats.BytesSent
	consumer.DurationSec = durationSec
	consumer.RemoteJitterMs = call.rtpStats.RemoteJitterMs
	consumer.FractionLost = call.rtpStats.RemoteFractionLost
	consumer.RoundTripTimeMs = call.rtpStats.RemoteRTTMs

	ap.UpstreamChunks = call.rtpStats.UpstreamChunks
	ap.ConversionQueue = call.rtpStats.ConversionQueue
	ap.PassthroughPackets = call.rtpStats.PassthroughPackets
	ap.TranscodePackets = call.rtpStats.TranscodePackets

	now := time.Now()
	upStartOffset, upEndOffset := calcTimelineOffsets(now, call.rtpStats.UpstreamPlayAtStart, call.rtpStats.UpstreamPlayAtEnd, call.rtpStats.UpstreamChunks)
	readyStartOffset, readyEndOffset := calcTimelineOffsets(now, call.rtpStats.ReadyPlayAtStart, call.rtpStats.ReadyPlayAtEnd, call.rtpStats.ConversionQueue)

	ap.BufferStartSec = trackProgSec + upStartOffset
	ap.BufferEndSec = trackProgSec + upEndOffset
	ap.ReadyStartSec = trackProgSec + readyStartOffset
	ap.ReadyEndSec = trackProgSec + readyEndOffset

	volDesc := domain.FormatVolumeGain(ap.VolumePercent, muted)

	switch {
	case !call.answered:
		ap.Mode = "buffering"
		ap.Passthrough = false
		ap.Summary = fmt.Sprintf("Live audio queue (%d chunks) → Transcode (%s) → RTP %s",
			call.rtpStats.UpstreamChunks, volDesc, activeCodec.DisplayName())
		ap.Stages = []string{
			fmt.Sprintf("Stage 1: Upstream Ingestion & Raw Queue (%d chunks)", call.rtpStats.UpstreamChunks),
			fmt.Sprintf("Stage 2: Transcoding & Gain (%s, ready on SIP 200 OK)", volDesc),
			"Stage 3: Downstream RTP Playout (Live sync, waiting for answer)",
		}
	case call.rtpStats.PathMode != "":
		ap.Mode = call.rtpStats.PathMode
		ap.Passthrough = call.rtpStats.PathMode == "opus_passthrough"
		ap.Summary = call.rtpStats.PathSummary
		ap.Stages = call.rtpStats.PathStages
		if call.rtpStats.PathVolumePercent > 0 || call.rtpStats.PathMode != "" {
			ap.VolumePercent = call.rtpStats.PathVolumePercent
		}
		if call.rtpStats.PathIngressCodec != "" {
			ap.IngressCodec = call.rtpStats.PathIngressCodec
			ap.IngressFormat = domain.FormatAudioDescription(call.rtpStats.PathIngressCodec, call.rtpStats.PathIngressRate, call.rtpStats.PathIngressChannels, 16)
		}
	case call.answered:
		ap.Mode = "transcode"
		ap.Summary = prodFormat + " → transcode (" + volDesc + ") → RTP " + activeCodec.DisplayName()
		ap.Stages = []string{
			"Stage 1: Upstream Ingress (" + prodFormat + ")",
			fmt.Sprintf("Stage 2: Transcode (%s) → %s", volDesc, activeCodec.DisplayName()),
			"Stage 3: Downstream Playout (Live sync, 20ms pacing)",
		}
	default:
		ap.Mode = "dialing"
		ap.Summary = "SIP dialing — media path initializing"
		ap.Stages = []string{"Stage 1: Upstream Ingress (" + prodFormat + ")", "Stage 2: Codec Negotiation", "Stage 3: RTP Pending"}
	}

	return consumer, ap, state
}

func calcTimelineOffsets(now time.Time, start, end time.Time, chunkCount int) (float64, float64) {
	if !start.IsZero() && !end.IsZero() {
		startOffset := start.Sub(now).Seconds()
		if startOffset < 0 {
			startOffset = 0
		}
		endOffset := end.Sub(now).Seconds() + 0.02
		if endOffset < startOffset {
			endOffset = startOffset
		}
		return startOffset, endOffset
	}
	if chunkCount > 0 {
		return 0, float64(chunkCount*20) / 1000.0
	}
	return 0, 0
}
