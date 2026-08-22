package app

import (
	"fmt"
	"time"

	"github.com/miguelangel-nubla/sendspin-voip/internal/domain"
)

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

	prodInfo, prodFormat, bitrateIn, trackProgSec := buildProducerInfo(player, ingStats, hasIngress)

	info := StreamDebugInfo{
		ID:        cfg.ID,
		Name:      cfg.Name,
		State:     "idle",
		IsPlaying: isPlaying,
		IsGrouped: isGrouped,
		Volume:    vol,
		Muted:     muted,
		AudioPath: AudioPathDebugInfo{
			Muted:         muted,
			VolumePercent: effectiveVol,
			IngressCodec:  ingStats.Codec,
			IngressFormat: prodFormat,
		},
		Producers: []ProducerDebugInfo{prodInfo},
		Consumers: make([]ConsumerDebugInfo, 0, 1),
	}

	allCodecs := domain.PrioritizeCodecs(cfg.Codec, nil)
	offeredSIPCodecs := make([]string, len(allCodecs))
	for i, c := range allCodecs {
		offeredSIPCodecs[i] = c.SDPDescription()
	}

	autoAnswerDesc := string(cfg.AutoAnswer)
	if cfg.CustomAutoAnswerHeader != "" {
		autoAnswerDesc = fmt.Sprintf("custom (%s)", cfg.CustomAutoAnswerHeader)
	}

	if call.hasCall {
		consumer, audioPath, state := buildActiveCallInfo(cfg, call, discoveredCodecs, offeredSIPCodecs, autoAnswerDesc, prodFormat, bitrateIn, trackProgSec, effectiveVol, muted)
		info.State = state
		info.AudioPath = audioPath
		info.Consumers = append(info.Consumers, consumer)
	} else {
		consumer, audioPath := buildIdleCallInfo(cfg, discoveredCodecs, offeredSIPCodecs, autoAnswerDesc, prodFormat, effectiveVol, muted)
		info.AudioPath = audioPath
		info.Consumers = append(info.Consumers, consumer)
	}

	return info
}

func buildProducerInfo(player *domain.Player, ingStats IngressPlayerStats, hasIngress bool) (ProducerDebugInfo, string, int, float64) {
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

func buildActiveCallInfo(
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
	if call.rtpStats.Codec != "" {
		activeCodec = call.rtpStats.Codec
	}

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

	bitrateOut := 64
	egressCh := 1
	if activeCodec == domain.CodecOpus {
		bitrateOut = bitrateIn
		if bitrateOut <= 0 {
			bitrateOut = 128
		}
		egressCh = 2
		if call.rtpStats.PathIngressChannels == 1 {
			egressCh = 1
		}
	}

	egressFormat := activeCodec.FormatDescription(egressCh, bitrateOut)

	ap := AudioPathDebugInfo{
		Muted:              muted,
		VolumePercent:      effectiveVol,
		EgressCodec:        string(activeCodec),
		EgressFormat:       egressFormat,
		UpstreamChunks:     call.rtpStats.UpstreamChunks,
		ConversionQueue:    call.rtpStats.ConversionQueue,
		PassthroughPackets: call.rtpStats.PassthroughPackets,
		TranscodePackets:   call.rtpStats.TranscodePackets,
		PlayheadSec:        trackProgSec,
	}

	// Buffer timeline calculation
	now := time.Now()
	var upStartOffset, upEndOffset, readyStartOffset, readyEndOffset float64

	if !call.rtpStats.UpstreamPlayAtStart.IsZero() && !call.rtpStats.UpstreamPlayAtEnd.IsZero() {
		upStartOffset = call.rtpStats.UpstreamPlayAtStart.Sub(now).Seconds()
		if upStartOffset < 0 {
			upStartOffset = 0
		}
		upEndOffset = call.rtpStats.UpstreamPlayAtEnd.Sub(now).Seconds() + 0.02
		if upEndOffset < upStartOffset {
			upEndOffset = upStartOffset
		}
	} else if call.rtpStats.UpstreamChunks > 0 {
		upEndOffset = float64(call.rtpStats.UpstreamChunks*20) / 1000.0
	}

	if !call.rtpStats.ReadyPlayAtStart.IsZero() && !call.rtpStats.ReadyPlayAtEnd.IsZero() {
		readyStartOffset = call.rtpStats.ReadyPlayAtStart.Sub(now).Seconds()
		if readyStartOffset < 0 {
			readyStartOffset = 0
		}
		readyEndOffset = call.rtpStats.ReadyPlayAtEnd.Sub(now).Seconds() + 0.02
		if readyEndOffset < readyStartOffset {
			readyEndOffset = readyStartOffset
		}
	} else if call.rtpStats.ConversionQueue > 0 {
		readyEndOffset = float64(call.rtpStats.ConversionQueue*20) / 1000.0
	}

	ap.BufferStartSec = trackProgSec + upStartOffset
	ap.BufferEndSec = trackProgSec + upEndOffset
	ap.ReadyStartSec = trackProgSec + readyStartOffset
	ap.ReadyEndSec = trackProgSec + readyEndOffset

	volDesc := volumeStageForDebug(ap.VolumePercent, muted)

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

	consumer := ConsumerDebugInfo{
		Type:             "SIP/RTP Egress",
		URL:              cfg.SIPTarget,
		CallID:           call.callID,
		State:            state,
		ConfigCodec:      string(cfg.Codec),
		ActiveCodec:      string(activeCodec),
		DiscoveredCodecs: discoveredCodecs,
		OfferedCodecs:    offeredSIPCodecs,
		NegotiatedSDP:    activeCodec.SDPDescription(),
		RTPClockRate:     activeCodec.RTPClockRate(),
		PayloadType:      activeCodec.PayloadType(),
		Format:           egressFormat,
		LocalRTP:         localRTPStr,
		RemoteRTP:        call.rtpStats.RemoteAddr,
		AutoAnswer:       autoAnswerDesc,
		Priority:         call.priority,
		LingerActive:     call.lingerActive,
		PacketsSent:      call.rtpStats.PacketsSent,
		BytesSent:        call.rtpStats.BytesSent,
		BitrateKbps:      bitrateOut,
		DurationSec:      durationSec,
	}

	return consumer, ap, state
}

func buildIdleCallInfo(
	cfg domain.PlayerConfig,
	discoveredCodecs, offeredSIPCodecs []string,
	autoAnswerDesc, prodFormat string,
	effectiveVol int,
	muted bool,
) (ConsumerDebugInfo, AudioPathDebugInfo) {
	bitrateOut := cfg.Codec.DefaultBitrateKbps()
	egressFormat := cfg.Codec.FormatDescription(1, bitrateOut)

	ap := AudioPathDebugInfo{
		Muted:         muted,
		VolumePercent: effectiveVol,
		EgressCodec:   string(cfg.Codec),
		EgressFormat:  egressFormat,
		Summary:       "idle — " + prodFormat + " ⇢ (no call) ⇢ " + cfg.Codec.DisplayName(),
		Stages: []string{
			"Stage 1: Upstream Ingress (" + prodFormat + ")",
			"Stage 2: No active SIP session",
			"Stage 3: Configured Egress " + cfg.Codec.DisplayName(),
		},
	}

	consumer := ConsumerDebugInfo{
		Type:             "SIP/RTP Egress",
		URL:              cfg.SIPTarget,
		State:            "idle",
		ConfigCodec:      string(cfg.Codec),
		ActiveCodec:      string(cfg.Codec),
		DiscoveredCodecs: discoveredCodecs,
		OfferedCodecs:    offeredSIPCodecs,
		NegotiatedSDP:    cfg.Codec.SDPDescription(),
		RTPClockRate:     cfg.Codec.RTPClockRate(),
		PayloadType:      cfg.Codec.PayloadType(),
		Format:           egressFormat,
		AutoAnswer:       autoAnswerDesc,
		Priority:         cfg.Priority,
		BitrateKbps:      bitrateOut,
	}

	return consumer, ap
}

func volumeStageForDebug(volumePercent int, muted bool) string {
	if muted || volumePercent <= 0 {
		return "volume mute"
	}
	if volumePercent >= 100 {
		return "volume 100% (0 dB)"
	}
	db := (float64(volumePercent)/100.0)*60.0 - 60.0
	return fmt.Sprintf("volume %d%% (%.0f dB)", volumePercent, db)
}
