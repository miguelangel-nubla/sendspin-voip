package app

import (
	"context"
	"net"
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
	PathMode            string   `json:"path_mode,omitempty"` // "opus_passthrough" | "transcode" | "idle"
	PathSummary         string   `json:"path_summary,omitempty"`
	PathStages          []string `json:"path_stages,omitempty"`
	PathVolumePercent   int      `json:"path_volume_percent"`
	PathIngressCodec    string   `json:"path_ingress_codec,omitempty"`
	PathIngressRate     int      `json:"path_ingress_rate,omitempty"`
	PathIngressChannels int      `json:"path_ingress_channels,omitempty"`
	PassthroughPackets  uint64   `json:"passthrough_packets"`
	TranscodePackets    uint64   `json:"transcode_packets"`
}

// AudioPathDebugInfo describes the end-to-end processing pipeline for a stream.
type AudioPathDebugInfo struct {
	Mode               string   `json:"mode"` // idle | buffering | opus_passthrough | transcode
	Summary            string   `json:"summary"`
	Stages             []string `json:"stages"`
	Passthrough        bool     `json:"passthrough"`
	VolumePercent      int      `json:"volume_percent"`
	Muted              bool     `json:"muted"`
	IngressCodec       string   `json:"ingress_codec,omitempty"`
	IngressFormat      string   `json:"ingress_format,omitempty"`
	EgressCodec        string   `json:"egress_codec,omitempty"`
	EgressFormat       string   `json:"egress_format,omitempty"`
	BufferMode         string   `json:"buffer_mode,omitempty"`
	PreAnswerBuffered  int      `json:"pre_answer_buffered,omitempty"`
	PassthroughPackets uint64   `json:"passthrough_packets,omitempty"`
	TranscodePackets   uint64   `json:"transcode_packets,omitempty"`
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
	Metadata       domain.StreamMetadata `json:"metadata"`
	ChunksReceived uint64                `json:"chunks_received"`
	BytesReceived  uint64                `json:"bytes_received"`
}

// ProducerDebugInfo details the upstream audio ingress source.
type ProducerDebugInfo struct {
	Type           string   `json:"type"` // "Sendspin Ingress"
	URL            string   `json:"url"`
	Connected      bool     `json:"connected"`
	Format         string   `json:"format"` // e.g. "PCM 16000Hz 1ch 16bit" or "Opus 48000Hz 2ch"
	Codec          string   `json:"codec"`
	SampleRate     int      `json:"sample_rate"`
	Channels       int      `json:"channels"`
	BitDepth       int      `json:"bit_depth"`
	BitrateKbps    int      `json:"bitrate_kbps,omitempty"`
	OfferedFormats []string `json:"offered_formats,omitempty"` // e.g. ["PCM 16000Hz 1ch 16bit", "PCM 48000Hz 2ch 16bit"]
	State          string   `json:"state"`
	Track          string   `json:"track,omitempty"`
	Artist         string   `json:"artist,omitempty"`
	Title          string   `json:"title,omitempty"`
	Album          string   `json:"album,omitempty"`
	AlbumArtist    string   `json:"album_artist,omitempty"`
	ChunksReceived uint64   `json:"chunks_received"`
	BytesReceived  uint64   `json:"bytes_received"`
}

// ConsumerDebugInfo details the downstream SIP/RTP egress destination.
type ConsumerDebugInfo struct {
	Type           string   `json:"type"` // "SIP/RTP Egress"
	URL            string   `json:"url"`  // e.g. "sip:8003@asterisk.local.myol.es"
	CallID         string   `json:"call_id,omitempty"`
	State          string   `json:"state"`
	ConfigCodec    string   `json:"config_codec"`
	ActiveCodec    string   `json:"active_codec"`
	OfferedCodecs  []string `json:"offered_codecs,omitempty"`
	NegotiatedSDP  string   `json:"negotiated_sdp,omitempty"`
	RTPClockRate   uint32   `json:"rtp_clock_rate,omitempty"`
	PayloadType    uint8    `json:"payload_type,omitempty"`
	Format         string   `json:"format"` // e.g. "G.722 16000Hz 1ch (64 kbps)"
	LocalRTP       string   `json:"local_rtp,omitempty"`
	RemoteRTP      string   `json:"remote_rtp,omitempty"`
	AutoAnswer     string   `json:"auto_answer,omitempty"`
	BufferMode     string   `json:"buffer_mode"`
	Priority       int      `json:"priority"`
	BufferedChunks int      `json:"buffered_chunks"`
	LingerActive   bool     `json:"linger_active"`
	PacketsSent    uint64   `json:"packets_sent"`
	BytesSent      uint64   `json:"bytes_sent"`
	BitrateKbps    int      `json:"bitrate_kbps,omitempty"`
	DurationSec    float64  `json:"duration_sec"`
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

// SIPDialog represents an active SIP call dialog.
type SIPDialog interface {
	// RemoteRTPAddr returns the remote IP and RTP port parsed from the SIP 200 OK SDP answer.
	RemoteRTPAddr() *net.UDPAddr
	// RemoteCodec returns the negotiated codec from SDP.
	RemoteCodec() domain.Codec
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
	// PushAudio sends raw PCM samples (int32) into the transcoder and RTP pacer.
	PushAudio(chunk domain.AudioChunk, volumePercent int) error
	// ClearBuffer flushes any buffered PCM samples and queued RTP packets (e.g. on seek).
	ClearBuffer()
	// DrainAndClose waits for drainDelay then closes the RTP socket.
	DrainAndClose(drainDelay time.Duration) error
	// Stats returns packet and byte transmission counters.
	Stats() RTPStats
}

// AudioTranscoderPort defines the interface for resampling, volume adjustment, and codec encoding.
type AudioTranscoderPort interface {
	// Transcode converts incoming PCM samples to the target codec payload.
	Transcode(samples []int32, srcRate int, srcChannels int, dstCodec domain.Codec, volumePercent int) ([]byte, error)
}

// PlayerIngressPort defines the interface for managing Sendspin client connections to Music Assistant.
type PlayerIngressPort interface {
	// RegisterPlayer creates and starts a virtual Sendspin player client.
	RegisterPlayer(player domain.PlayerConfig, handler PlayerEventHandler) error
	// RegisterPlayerWithCodecs creates and starts a virtual Sendspin player with dynamic codec capabilities.
	RegisterPlayerWithCodecs(player domain.PlayerConfig, codecs []domain.Codec, handler PlayerEventHandler) error
	// UnregisterPlayer stops and disconnects a virtual Sendspin player client.
	UnregisterPlayer(playerID string) error
	// SendPauseToUpstream sends a pause command to Music Assistant for the given player.
	// Called when the remote phone hangs up to stop the upstream stream.
	SendPauseToUpstream(playerID string)
	// GetPlayerStats retrieves current ingress stats and metadata for a player.
	GetPlayerStats(playerID string) (IngressPlayerStats, bool)
	// StopAll disconnects all virtual players.
	StopAll() error
}

// PlayerEventHandler defines the callbacks triggered by incoming Sendspin player events.
type PlayerEventHandler interface {
	OnStreamStart(playerID string, meta domain.StreamMetadata)
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
