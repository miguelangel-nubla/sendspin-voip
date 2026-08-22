package app

import (
	"cmp"
	"context"
	"net"
	"strings"
	"time"

	"github.com/miguelangel-nubla/sendspin-voip/internal/domain"
)

// SIPStatus represents current SIP connection and registration state.
type SIPStatus struct {
	Mode         string    `json:"mode"`
	Server       string    `json:"server"`
	Username     string    `json:"username"`
	Domain       string    `json:"domain"`
	Transport    string    `json:"transport"`
	LocalIP      string    `json:"local_ip"`
	LocalSIPPort int       `json:"local_sip_port"`
	Registered   bool      `json:"registered"`
	LastRegister time.Time `json:"last_register,omitempty"`
}

// RTPStats represents runtime metrics for an active RTP session.
type RTPStats struct {
	LocalPort   int          `json:"local_port"`
	RemoteAddr  string       `json:"remote_addr"`
	Codec       domain.Codec `json:"codec"`
	PacketsSent uint64       `json:"packets_sent"`
	BytesSent   uint64       `json:"bytes_sent"`

	// Live audio path (updated on each PushAudio)
	PathMode            string    `json:"path_mode,omitempty"` // "opus_passthrough" | "transcode" | "idle"
	PathSummary         string    `json:"path_summary,omitempty"`
	PathStages          []string  `json:"path_stages,omitempty"`
	PathVolumePercent   int       `json:"path_volume_percent"`
	PathIngressCodec    string    `json:"path_ingress_codec,omitempty"`
	PathIngressRate     int       `json:"path_ingress_rate,omitempty"`
	PathIngressChannels int       `json:"path_ingress_channels,omitempty"`
	PassthroughPackets  uint64    `json:"passthrough_packets"`
	TranscodePackets    uint64    `json:"transcode_packets"`
	UpstreamChunks      int       `json:"upstream_chunks"`
	ConversionQueue     int       `json:"conversion_queue"`
	UpstreamPlayAtStart time.Time `json:"upstream_play_at_start,omitempty"`
	UpstreamPlayAtEnd   time.Time `json:"upstream_play_at_end,omitempty"`
	ReadyPlayAtStart    time.Time `json:"ready_play_at_start,omitempty"`
	ReadyPlayAtEnd      time.Time `json:"ready_play_at_end,omitempty"`
	Answered            bool      `json:"answered"`
	RemoteJitterMs      float64   `json:"remote_jitter_ms,omitempty"`
	RemoteFractionLost  float64   `json:"remote_fraction_lost,omitempty"`
	RemoteRTTMs         float64   `json:"remote_rtt_ms,omitempty"`
}

// IngressPlayerStats represents runtime metrics for a Sendspin ingress connection.
type IngressPlayerStats struct {
	ServerAddr     string                `json:"server_addr"`
	Connected      bool                  `json:"connected"`
	Codec          string                `json:"codec"`
	SampleRate     int                   `json:"sample_rate"`
	Channels       int                   `json:"channels"`
	BitDepth       int                   `json:"bit_depth"`
	OfferedFormats []string              `json:"offered_formats,omitempty"`
	ExposedCodecs  []string              `json:"exposed_codecs,omitempty"`
	Metadata       domain.StreamMetadata `json:"metadata"`
	ChunksReceived uint64                `json:"chunks_received"`
	BytesReceived  uint64                `json:"bytes_received"`
}

// FormatDescription returns a formatted representation of the active ingress stream format.
func (s IngressPlayerStats) FormatDescription() string {
	return domain.FormatAudioDescription(cmp.Or(s.Codec, "opus"), s.SampleRate, s.Channels, s.BitDepth)
}

// BitrateKbps calculates the bitrate in kbps for the ingress format.
func (s IngressPlayerStats) BitrateKbps() int {
	return domain.CalculateBitrateKbps(cmp.Or(s.Codec, "opus"), s.SampleRate, s.Channels, s.BitDepth)
}

// WebSocketURL returns the normalized WebSocket endpoint URI for the ingress server.
func (s IngressPlayerStats) WebSocketURL() string {
	addr := s.ServerAddr
	if addr == "" {
		return ""
	}
	if !strings.HasPrefix(addr, "ws://") && !strings.HasPrefix(addr, "http://") {
		addr = "ws://" + addr + "/sendspin"
	}
	return addr
}

// SIPDialog represents an active SIP call dialog.
type SIPDialog interface {
	// RemoteRTPAddr returns the remote IP and RTP port parsed from the SIP 200 OK SDP answer.
	RemoteRTPAddr() *net.UDPAddr
	// RemoteCodec returns the negotiated codec from SDP.
	RemoteCodec() domain.Codec
	// CallID returns the SIP Call-ID of the dialog, for correlation in the dashboard and logs.
	CallID() string
	// Bye terminates the SIP call cleanly.
	Bye(ctx context.Context) error
	// Done returns a channel that closes if the remote party hangs up.
	Done() <-chan struct{}
}

// SIPCallerPort defines the interface for making SIP calls and handling auto-answer.
type SIPCallerPort interface {
	// Start starts the SIP user agent listening on configured UDP/TCP port.
	Start(ctx context.Context) error
	// Stop stops the SIP stack and closes connections.
	Stop() error
	// Dial initiates a SIP INVITE with the appropriate auto-answer headers and local SDP.
	Dial(ctx context.Context, player domain.PlayerConfig, localRTPPort int) (SIPDialog, error)
	// ProbeTarget queries the remote SIP target capabilities via OPTIONS request.
	ProbeTarget(ctx context.Context, targetURI string) ([]domain.Codec, error)
	// LocalIP returns the advertised local IP address for SDP.
	LocalIP() string
	// RegistrationStatus returns current SIP registration and connectivity info.
	RegistrationStatus() SIPStatus
}

// RTPStreamerPort defines the interface for creating and managing RTP audio streaming sessions.
type RTPStreamerPort interface {
	// CreateSession binds a local UDP port for RTP and returns an RTP session handler.
	CreateSession(codec domain.Codec) (RTPSession, error)
}

// RTPSession represents an active RTP transmission pipeline.
type RTPSession interface {
	// LocalPort returns the bound local UDP port.
	LocalPort() int
	// StartTransmission sets the destination address and starts packet pacing.
	StartTransmission(remoteAddr *net.UDPAddr) error
	// SetCodec updates the transmission codec if negotiated dynamically via SDP answer.
	SetCodec(codec domain.Codec)
	// SetAnswered sets whether the SIP call has been answered.
	SetAnswered(answered bool)
	// SetVolume updates the gain level and flushes converted frames while preserving raw upstream audio.
	SetVolume(volumePercent int)
	// PushAudio sends raw audio chunks from Sendspin into the Upstream stage.
	PushAudio(chunk domain.AudioChunk, volumePercent int) error
	// ClearBuffer flushes all stages on seek / stream clear.
	ClearBuffer()
	// SetDTMFHandler sets a callback for incoming RFC 2833 / RFC 4733 DTMF digits.
	SetDTMFHandler(handler func(digit string))
	// DrainAndClose waits for drainDelay then closes the RTP socket.
	DrainAndClose(drainDelay time.Duration) error
	// Stats returns packet and byte transmission counters.
	Stats() RTPStats
}

// AudioTranscoderPort defines the interface for resampling, volume adjustment, and codec encoding.
type AudioTranscoderPort interface {
	// Transcode converts incoming PCM samples to the target codec payload.
	Transcode(samples []int32, srcRate int, srcChannels int, dstCodec domain.Codec, volumePercent int) ([]byte, error)
	// DecodeOpusToPCM decodes native Opus data to 48kHz PCM samples.
	DecodeOpusToPCM(opusData []byte, channels int) ([]int32, error)
}

// PlayerIngressPort defines the interface for managing Sendspin client connections to Music Assistant.
type PlayerIngressPort interface {
	// RegisterPlayer creates and starts a virtual Sendspin player client.
	RegisterPlayer(player domain.PlayerConfig, handler PlayerEventHandler) error
	// RegisterPlayerWithCodecs creates and starts a virtual Sendspin player with dynamic codec capabilities.
	RegisterPlayerWithCodecs(player domain.PlayerConfig, codecs []domain.Codec, handler PlayerEventHandler) error
	// UnregisterPlayer stops and disconnects a virtual Sendspin player client.
	UnregisterPlayer(playerID string) error
	// SendStopToUpstream sends a stop command to Music Assistant for the given player.
	SendStopToUpstream(playerID string)
	// SendNextToUpstream sends a next track command to Music Assistant.
	SendNextToUpstream(playerID string)
	// SendPlayPauseToUpstream sends a play/pause toggle command to Music Assistant.
	SendPlayPauseToUpstream(playerID string)
	// SendVolumeToUpstream sends a volume change command to Music Assistant.
	SendVolumeToUpstream(playerID string, volume int)
	// SendMuteToUpstream sends a mute change command to Music Assistant.
	SendMuteToUpstream(playerID string, muted bool)
	// GetPlayerStats retrieves current ingress stats and metadata for a player.
	GetPlayerStats(playerID string) (IngressPlayerStats, bool)
	// StopAll disconnects all virtual players.
	StopAll() error
}

// PlayerEventHandler defines the callbacks triggered by incoming Sendspin player events.
type PlayerEventHandler interface {
	OnStreamStart(playerID string, meta domain.StreamMetadata)
	OnMetadata(playerID string, meta domain.StreamMetadata)
	OnStreamClear(playerID string)
	OnPlaybackState(playerID string, state string)
	OnAudioChunk(playerID string, chunk domain.AudioChunk)
	OnStreamEnd(playerID string)
	OnVolumeChange(playerID string, volume int)
	OnMuteChange(playerID string, muted bool)
	OnGroupUpdate(playerID string, isGrouped bool)
}

// PlayerStateRecord holds persistent player preferences (such as volume and mute).
type PlayerStateRecord struct {
	Volume int  `json:"volume"`
	Muted  bool `json:"muted"`
}

// StateStorePort defines persistence for player state across restarts.
type StateStorePort interface {
	GetPlayerState(playerID string) (PlayerStateRecord, bool)
	SetPlayerState(playerID string, state PlayerStateRecord) error
}
