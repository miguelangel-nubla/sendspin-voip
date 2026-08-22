package app

import (
	"fmt"
	"strings"
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

	effectiveVol := vol
	if muted {
		effectiveVol = 0
	}

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
		},
		Producers: make([]ProducerDebugInfo, 0, 1),
		Consumers: make([]ConsumerDebugInfo, 0, 1),
	}

	// 1. Gather Producer Info (Sendspin Ingress)
	prodState := "disconnected"
	if ingStats.Connected {
		if isPlaying {
			prodState = "streaming"
		} else {
			prodState = "connected"
		}
	}

	var trackStr string
	if ingStats.Metadata.Title != "" {
		if ingStats.Metadata.Artist != "" {
			trackStr = fmt.Sprintf("%s - %s", ingStats.Metadata.Artist, ingStats.Metadata.Title)
		} else {
			trackStr = ingStats.Metadata.Title
		}
	}

	prodFormat := "PCM 48000Hz 2ch 16bit"
	bitrateIn := 1536
	if hasIngress && ingStats.Codec != "" {
		if strings.ToLower(ingStats.Codec) == "opus" {
			prodFormat = fmt.Sprintf("OPUS %dHz %dch", ingStats.SampleRate, ingStats.Channels)
			bitrateIn = 128
		} else {
			prodFormat = fmt.Sprintf("%s %dHz %dch %dbit", strings.ToUpper(ingStats.Codec), ingStats.SampleRate, ingStats.Channels, ingStats.BitDepth)
			bitrateIn = (ingStats.SampleRate * ingStats.Channels * ingStats.BitDepth) / 1000
		}
	} else {
		prodFormat = "OPUS 48000Hz 2ch"
		bitrateIn = 128
	}

	ingressURL := ingStats.ServerAddr
	if ingressURL != "" && !strings.HasPrefix(ingressURL, "ws://") && !strings.HasPrefix(ingressURL, "http://") {
		ingressURL = "ws://" + ingressURL + "/sendspin"
	}

	var trackProgSec float64
	if isPlaying {
		trackProgSec = float64(ingStats.Metadata.ProgressMs) / 1000.0
		if !ingStats.Metadata.ProgressUpdated.IsZero() {
			trackProgSec += time.Since(ingStats.Metadata.ProgressUpdated).Seconds()
			if ingStats.Metadata.Duration > 0 && trackProgSec > ingStats.Metadata.Duration.Seconds() {
				trackProgSec = ingStats.Metadata.Duration.Seconds()
			}
		}
	} else if prodState == "paused" {
		trackProgSec = float64(ingStats.Metadata.ProgressMs) / 1000.0
	}

	info.Producers = append(info.Producers, ProducerDebugInfo{
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
	})

	info.AudioPath.IngressCodec = ingStats.Codec
	info.AudioPath.IngressFormat = prodFormat
	info.AudioPath.VolumePercent = effectiveVol

	// 2. Gather Consumer Info (SIP/RTP Egress)
	allCodecs := domain.PrioritizeCodecs(cfg.Codec, nil)
	var offeredSIPCodecs []string
	for _, c := range allCodecs {
		offeredSIPCodecs = append(offeredSIPCodecs, c.SDPDescription())
	}

	autoAnswerDesc := string(cfg.AutoAnswer)
	if cfg.CustomAutoAnswerHeader != "" {
		autoAnswerDesc = fmt.Sprintf("custom (%s)", cfg.CustomAutoAnswerHeader)
	}

	if call.hasCall {
		activeCodec := cfg.Codec
		if call.rtpStats.Codec != "" {
			activeCodec = call.rtpStats.Codec
		}

		if call.lingerActive {
			info.State = "lingering"
			call.sessionState = "lingering"
		} else if call.sessionState == string(domain.StateActive) {
			info.State = "active"
		} else if call.sessionState == string(domain.StateDialing) {
			info.State = "dialing"
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
		info.AudioPath.EgressCodec = string(activeCodec)
		info.AudioPath.EgressFormat = egressFormat
		info.AudioPath.UpstreamChunks = call.rtpStats.UpstreamChunks
		info.AudioPath.ConversionQueue = call.rtpStats.ConversionQueue
		info.AudioPath.PassthroughPackets = call.rtpStats.PassthroughPackets
		info.AudioPath.TranscodePackets = call.rtpStats.TranscodePackets

		// Calculate buffer timeline positions directly from PlayAt timestamps
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

		info.AudioPath.PlayheadSec = trackProgSec
		info.AudioPath.BufferStartSec = trackProgSec + upStartOffset
		info.AudioPath.BufferEndSec = trackProgSec + upEndOffset
		info.AudioPath.ReadyStartSec = trackProgSec + readyStartOffset
		info.AudioPath.ReadyEndSec = trackProgSec + readyEndOffset

		volDesc := volumeStageForDebug(info.AudioPath.VolumePercent, muted)

		switch {
		case !call.answered:
			info.AudioPath.Mode = "buffering"
			info.AudioPath.Passthrough = false
			info.AudioPath.Summary = fmt.Sprintf("Live audio queue (%d chunks) → Transcode (%s) → RTP %s",
				call.rtpStats.UpstreamChunks, volDesc, activeCodec.DisplayName())
			info.AudioPath.Stages = []string{
				fmt.Sprintf("Stage 1: Upstream Ingestion & Raw Queue (%d chunks)", call.rtpStats.UpstreamChunks),
				fmt.Sprintf("Stage 2: Transcoding & Gain (%s, ready on SIP 200 OK)", volDesc),
				"Stage 3: Downstream RTP Playout (Live sync, waiting for answer)",
			}
		case call.rtpStats.PathMode != "":
			info.AudioPath.Mode = call.rtpStats.PathMode
			info.AudioPath.Passthrough = call.rtpStats.PathMode == "opus_passthrough"
			info.AudioPath.Summary = call.rtpStats.PathSummary
			info.AudioPath.Stages = call.rtpStats.PathStages
			if call.rtpStats.PathVolumePercent > 0 || call.rtpStats.PathMode != "" {
				info.AudioPath.VolumePercent = call.rtpStats.PathVolumePercent
			}
			if call.rtpStats.PathIngressCodec != "" {
				info.AudioPath.IngressCodec = call.rtpStats.PathIngressCodec
				info.AudioPath.IngressFormat = fmt.Sprintf("%s %dHz %dch",
					strings.ToUpper(call.rtpStats.PathIngressCodec), call.rtpStats.PathIngressRate, call.rtpStats.PathIngressChannels)
			}
		case call.answered:
			info.AudioPath.Mode = "transcode"
			info.AudioPath.Summary = prodFormat + " → transcode (" + volDesc + ") → RTP " + activeCodec.DisplayName()
			info.AudioPath.Stages = []string{
				"Stage 1: Upstream Ingress (" + prodFormat + ")",
				fmt.Sprintf("Stage 2: Transcode (%s) → %s", volDesc, activeCodec.DisplayName()),
				"Stage 3: Downstream Playout (Live sync, 20ms pacing)",
			}
		default:
			info.AudioPath.Mode = "dialing"
			info.AudioPath.Summary = "SIP dialing — media path initializing"
			info.AudioPath.Stages = []string{"Stage 1: Upstream Ingress (" + prodFormat + ")", "Stage 2: Codec Negotiation", "Stage 3: RTP Pending"}
		}

		info.Consumers = append(info.Consumers, ConsumerDebugInfo{
			Type:             "SIP/RTP Egress",
			URL:              cfg.SIPTarget,
			CallID:           call.callID,
			State:            call.sessionState,
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
		})
	} else {
		bitrateOut := cfg.Codec.DefaultBitrateKbps()
		egressFormat := cfg.Codec.FormatDescription(1, bitrateOut)
		info.AudioPath.EgressCodec = string(cfg.Codec)
		info.AudioPath.EgressFormat = egressFormat
		info.AudioPath.Summary = "idle — " + prodFormat + " ⇢ (no call) ⇢ " + cfg.Codec.DisplayName()
		info.AudioPath.Stages = []string{
			"Stage 1: Upstream Ingress (" + prodFormat + ")",
			"Stage 2: No active SIP session",
			"Stage 3: Configured Egress " + cfg.Codec.DisplayName(),
		}

		info.Consumers = append(info.Consumers, ConsumerDebugInfo{
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
		})
	}

	return info
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
