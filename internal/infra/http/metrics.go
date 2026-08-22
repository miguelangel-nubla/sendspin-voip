package http

import (
	"fmt"
	"io"
	"runtime"
	"strings"
	"time"

	"github.com/miguelangel-nubla/sendspin-voip/internal/app"
)

// escapeLabelValue escapes backslashes, double quotes, and newlines for Prometheus labels.
func escapeLabelValue(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return s
}

// WritePrometheusMetrics writes Prometheus formatted metrics to w.
func WritePrometheusMetrics(
	w io.Writer,
	version, commit, buildDate string,
	startTime time.Time,
	sipStatus app.SIPStatus,
	streams map[string]app.StreamDebugInfo,
) {
	// Build Info
	fmt.Fprintf(w, "# HELP sendspin_voip_build_info Build and version metadata.\n")
	fmt.Fprintf(w, "# TYPE sendspin_voip_build_info gauge\n")
	fmt.Fprintf(w, "sendspin_voip_build_info{version=\"%s\",commit=\"%s\",build_date=\"%s\"} 1\n\n",
		escapeLabelValue(version), escapeLabelValue(commit), escapeLabelValue(buildDate))

	// Uptime
	uptime := time.Since(startTime).Seconds()
	fmt.Fprintf(w, "# HELP sendspin_voip_uptime_seconds Process uptime in seconds.\n")
	fmt.Fprintf(w, "# TYPE sendspin_voip_uptime_seconds counter\n")
	fmt.Fprintf(w, "sendspin_voip_uptime_seconds %.2f\n\n", uptime)

	// SIP Status
	fmt.Fprintf(w, "# HELP sendspin_voip_sip_registered Registration status with SIP PBX (1 = registered, 0 = unregistered).\n")
	fmt.Fprintf(w, "# TYPE sendspin_voip_sip_registered gauge\n")
	regVal := 0
	if sipStatus.Registered {
		regVal = 1
	}
	fmt.Fprintf(w, "sendspin_voip_sip_registered{server=\"%s\",username=\"%s\",mode=\"%s\"} %d\n\n",
		escapeLabelValue(sipStatus.Server), escapeLabelValue(sipStatus.Username), escapeLabelValue(sipStatus.Mode), regVal)

	// Player Totals
	totalPlayers := len(streams)
	activePlayers := 0
	for _, stream := range streams {
		if stream.IsActive() {
			activePlayers++
		}
	}

	fmt.Fprintf(w, "# HELP sendspin_voip_players_total Total configured virtual Sendspin players.\n")
	fmt.Fprintf(w, "# TYPE sendspin_voip_players_total gauge\n")
	fmt.Fprintf(w, "sendspin_voip_players_total %d\n\n", totalPlayers)

	fmt.Fprintf(w, "# HELP sendspin_voip_players_active Number of active streams currently dialing, playing, or lingering.\n")
	fmt.Fprintf(w, "# TYPE sendspin_voip_players_active gauge\n")
	fmt.Fprintf(w, "sendspin_voip_players_active %d\n\n", activePlayers)

	// Per-Player Metrics
	fmt.Fprintf(w, "# HELP sendspin_voip_player_active Whether the player stream is active (1 = active, 0 = idle).\n")
	fmt.Fprintf(w, "# TYPE sendspin_voip_player_active gauge\n")
	for id, stream := range streams {
		active := 0
		if stream.IsActive() {
			active = 1
		}
		fmt.Fprintf(w, "sendspin_voip_player_active{player_id=\"%s\",name=\"%s\"} %d\n",
			escapeLabelValue(id), escapeLabelValue(stream.Name), active)
	}
	fmt.Fprintln(w)

	fmt.Fprintf(w, "# HELP sendspin_voip_player_volume Player volume level (0-100).\n")
	fmt.Fprintf(w, "# TYPE sendspin_voip_player_volume gauge\n")
	for id, stream := range streams {
		fmt.Fprintf(w, "sendspin_voip_player_volume{player_id=\"%s\"} %d\n", escapeLabelValue(id), stream.Volume)
	}
	fmt.Fprintln(w)

	fmt.Fprintf(w, "# HELP sendspin_voip_player_muted Whether player audio is muted (1 = muted, 0 = unmuted).\n")
	fmt.Fprintf(w, "# TYPE sendspin_voip_player_muted gauge\n")
	for id, stream := range streams {
		muted := 0
		if stream.Muted {
			muted = 1
		}
		fmt.Fprintf(w, "sendspin_voip_player_muted{player_id=\"%s\"} %d\n", escapeLabelValue(id), muted)
	}
	fmt.Fprintln(w)

	fmt.Fprintf(w, "# HELP sendspin_voip_player_packets_sent_total Total RTP packets sent downstream.\n")
	fmt.Fprintf(w, "# TYPE sendspin_voip_player_packets_sent_total counter\n")
	for id, stream := range streams {
		var pkts uint64
		codec := stream.AudioPath.EgressCodec
		for _, c := range stream.Consumers {
			pkts += c.PacketsSent
			if codec == "" {
				codec = c.ActiveCodec
			}
		}
		fmt.Fprintf(w, "sendspin_voip_player_packets_sent_total{player_id=\"%s\",codec=\"%s\"} %d\n",
			escapeLabelValue(id), escapeLabelValue(codec), pkts)
	}
	fmt.Fprintln(w)

	fmt.Fprintf(w, "# HELP sendspin_voip_player_bytes_sent_total Total RTP bytes sent downstream.\n")
	fmt.Fprintf(w, "# TYPE sendspin_voip_player_bytes_sent_total counter\n")
	for id, stream := range streams {
		var bytes uint64
		codec := stream.AudioPath.EgressCodec
		for _, c := range stream.Consumers {
			bytes += c.BytesSent
			if codec == "" {
				codec = c.ActiveCodec
			}
		}
		fmt.Fprintf(w, "sendspin_voip_player_bytes_sent_total{player_id=\"%s\",codec=\"%s\"} %d\n",
			escapeLabelValue(id), escapeLabelValue(codec), bytes)
	}
	fmt.Fprintln(w)

	fmt.Fprintf(w, "# HELP sendspin_voip_player_chunks_received_total Total Sendspin chunks received upstream.\n")
	fmt.Fprintf(w, "# TYPE sendspin_voip_player_chunks_received_total counter\n")
	for id, stream := range streams {
		var chunks uint64
		for _, p := range stream.Producers {
			chunks += p.ChunksReceived
		}
		fmt.Fprintf(w, "sendspin_voip_player_chunks_received_total{player_id=\"%s\"} %d\n",
			escapeLabelValue(id), chunks)
	}
	fmt.Fprintln(w)

	// Runtime metrics
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	fmt.Fprintf(w, "# HELP go_goroutines Number of goroutines that currently exist.\n")
	fmt.Fprintf(w, "# TYPE go_goroutines gauge\n")
	fmt.Fprintf(w, "go_goroutines %d\n\n", runtime.NumGoroutine())

	fmt.Fprintf(w, "# HELP go_memstats_alloc_bytes Number of bytes allocated and still in use.\n")
	fmt.Fprintf(w, "# TYPE go_memstats_alloc_bytes gauge\n")
	fmt.Fprintf(w, "go_memstats_alloc_bytes %d\n\n", m.Alloc)

	fmt.Fprintf(w, "# HELP go_memstats_sys_bytes Number of bytes obtained from system.\n")
	fmt.Fprintf(w, "# TYPE go_memstats_sys_bytes gauge\n")
	fmt.Fprintf(w, "go_memstats_sys_bytes %d\n", m.Sys)
}
