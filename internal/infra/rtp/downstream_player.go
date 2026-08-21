package rtp

import (
	"log/slog"
	"sync"
	"time"

	"github.com/miguelangel-nubla/sendspin-voip/internal/domain"
)

// DownstreamPlayer represents Layer 3 of the audio pipeline.
// It is the playout timing policy engine that reads converted 20ms ready frames
// from AudioPath and decides when to hand them to RTPTransport.
//
// In LIVE mode:
//   - Discards stale raw upstream chunks before conversion so CPU is not wasted transcoding stale audio.
//   - Paces packet transmission against Sendspin PlayAt timestamps.
//   - Instantly discards expired ready frames before (Now - 150ms) with zero trickle latency.
//
// In ANNOUNCEMENT mode:
//   - Holds all frames from sample 0 until the SIP call is answered.
//   - Once answered, plays continuously from sample 0 on a strict 20ms cadence.
type DownstreamPlayer struct {
	mu          sync.Mutex
	logger      *slog.Logger
	mode        domain.BufferMode
	answered    bool
	codec       domain.Codec
	audioPath   *AudioPath
	transport   *RTPTransport
	firstPkt    bool
	loopRunning bool
	stopped     bool
	stopChan    chan struct{}
	stopOnce    sync.Once
	wg          sync.WaitGroup
}

// NewDownstreamPlayer creates a new DownstreamPlayer.
func NewDownstreamPlayer(
	logger *slog.Logger,
	mode domain.BufferMode,
	codec domain.Codec,
	audioPath *AudioPath,
	transport *RTPTransport,
) *DownstreamPlayer {
	if logger == nil {
		logger = slog.Default()
	}
	return &DownstreamPlayer{
		logger:    logger,
		mode:      mode,
		codec:     codec,
		audioPath: audioPath,
		transport: transport,
		firstPkt:  true,
		stopChan:  make(chan struct{}),
	}
}

// SetBufferMode updates the playout policy (Live vs Announcement).
func (d *DownstreamPlayer) SetBufferMode(mode domain.BufferMode) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.mode = mode
}

// SetCodec updates the downstream transmission codec.
func (d *DownstreamPlayer) SetCodec(codec domain.Codec) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.codec = codec
	d.firstPkt = true
}

// SetAnswered marks the call as answered and starts the downstream pacing loop.
func (d *DownstreamPlayer) SetAnswered(answered bool) {
	d.mu.Lock()
	if d.stopped || d.answered == answered {
		d.mu.Unlock()
		return
	}
	d.answered = answered
	d.firstPkt = true
	shouldStart := answered && !d.loopRunning
	if shouldStart {
		d.loopRunning = true
	}
	d.mu.Unlock()

	if shouldStart {
		d.wg.Add(1)
		go d.pacingLoop()
	}
}

// StartPlayout starts the 20ms downstream playout loop.
func (d *DownstreamPlayer) StartPlayout() {
	d.SetAnswered(true)
}

// pacingLoop executes every 20ms (50 times per second) to release ready audio frames.
func (d *DownstreamPlayer) pacingLoop() {
	defer d.wg.Done()

	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-d.stopChan:
			return
		case <-ticker.C:
			d.step()
		}
	}
}

// step executes a single 20ms playout tick.
func (d *DownstreamPlayer) step() {
	d.mu.Lock()
	answered := d.answered
	mode := d.mode
	codec := d.codec
	isMarker := d.firstPkt
	d.mu.Unlock()

	if !answered {
		return
	}

	now := time.Now()

	// 1. In live mode, discard stale raw upstream audio and stale ready frames before conversion.
	if mode == domain.BufferModeLive {
		staleCutoff := now.Add(-150 * time.Millisecond)
		d.audioPath.DiscardBefore(staleCutoff)
	}

	// 2. Convert raw upstream chunks into ready 20ms frames (up to 35 frames / 700ms ahead)
	_ = d.audioPath.Fill(35)

	// 3. In live mode, verify scheduled playback time
	if mode == domain.BufferModeLive {
		readyFrame, ok := d.audioPath.PeekReady()
		if !ok {
			return
		}

		// Hold until scheduled playback time (closes gap with Music Assistant clock)
		if !readyFrame.PlayAt.IsZero() && now.Before(readyFrame.PlayAt) {
			return
		}
	}

	// 4. Pop the ready 20ms frame (AudioPath automatically acknowledges consumed upstream chunks)
	frame, ok := d.audioPath.PopReady()
	if !ok {
		return
	}

	// 5. Hand over to Layer 4 RTP Transport
	samplesPerPacket := uint32((codec.RTPClockRate() * 20) / 1000)
	if err := d.transport.SendPacket(frame.Payload, codec.PayloadType(), samplesPerPacket, isMarker); err != nil {
		d.logger.Debug("RTP transmit error", "err", err)
	}

	if isMarker {
		d.mu.Lock()
		d.firstPkt = false
		d.mu.Unlock()
	}
}

// ResetMarker triggers the RTP marker bit on the next outgoing packet.
func (d *DownstreamPlayer) ResetMarker() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.firstPkt = true
}

// Stop terminates the downstream playout loop.
func (d *DownstreamPlayer) Stop() {
	d.stopOnce.Do(func() {
		d.mu.Lock()
		d.stopped = true
		d.mu.Unlock()
		close(d.stopChan)
	})
	d.wg.Wait()
}
