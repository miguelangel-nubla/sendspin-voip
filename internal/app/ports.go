package app

import (
	"context"
	"net"
	"time"

	"github.com/miguelangel-nubla/sendspin-voip/internal/domain"
)

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
	// LocalIP returns the advertised local IP address for SDP.
	LocalIP() string
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
	// DrainAndClose waits for drainDelay then closes the RTP socket.
	DrainAndClose(drainDelay time.Duration) error
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
	// SendPauseToUpstream sends a pause command to Music Assistant for the given player.
	// Called when the remote phone hangs up to stop the upstream stream.
	SendPauseToUpstream(playerID string)
	// StopAll disconnects all virtual players.
	StopAll() error
}

// PlayerEventHandler defines the callbacks triggered by incoming Sendspin player events.
type PlayerEventHandler interface {
	OnStreamStart(playerID string, meta domain.StreamMetadata)
	OnAudioChunk(playerID string, chunk domain.AudioChunk)
	OnStreamEnd(playerID string)
	OnVolumeChange(playerID string, volume int, muted bool)
	OnGroupUpdate(playerID string, isGrouped bool)
}
