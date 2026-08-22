package rtp

import (
	"sync"
	"time"

	"github.com/miguelangel-nubla/sendspin-voip/internal/domain"
)

const (
	// defaultUpstreamBufferCapacity is the safety ceiling for raw chunks held in upstream (~120s of audio at 20ms).
	defaultUpstreamBufferCapacity = 6000
)

// UpstreamPlayer represents Layer 1 of the audio pipeline.
// It acts as the raw timeline receiver for Sendspin audio chunks, completely
// decoupled from DSP/transcoding, SIP call states, and downstream playout modes.
//
// It retains raw chunks with a read cursor until they are actually played downstream.
// This allows volume or codec changes to rewind the read cursor and re-convert unplayed audio
// with zero audio skipping/loss.
type UpstreamPlayer struct {
	mu          sync.Mutex
	chunks      []domain.AudioChunk
	readIdx     int
	maxCapacity int
}

// NewUpstreamPlayer creates a new UpstreamPlayer with the specified capacity limit.
// If maxCapacity <= 0, defaultUpstreamBufferCapacity (6000 chunks = 120s) is used.
func NewUpstreamPlayer(maxCapacity int) *UpstreamPlayer {
	if maxCapacity <= 0 {
		maxCapacity = defaultUpstreamBufferCapacity
	}
	return &UpstreamPlayer{
		chunks:      make([]domain.AudioChunk, 0, 64),
		maxCapacity: maxCapacity,
	}
}

// Push appends an incoming raw audio chunk into the raw timeline in chronological order.
// Returns true if a seek jump / timeline discontinuity was detected and the audio pipeline needs clearing.
func (u *UpstreamPlayer) Push(chunk domain.AudioChunk) bool {
	u.mu.Lock()
	defer u.mu.Unlock()

	n := len(u.chunks)
	if n == 0 {
		u.chunks = append(u.chunks, chunk)
		return false
	}

	last := u.chunks[n-1]
	// Detect seek discontinuity: if incoming chunk jumps forward by > 1.5s or jumps backward
	if !chunk.PlayAt.IsZero() && !last.PlayAt.IsZero() {
		gap := chunk.PlayAt.Sub(last.PlayAt)
		if gap < -500*time.Millisecond || gap > 1500*time.Millisecond {
			// Discontinuity / seek detected!
			return true
		}
	}

	if len(u.chunks) >= u.maxCapacity {
		// Tail-drop on capacity overflow to preserve stream start
		return false
	}

	if isChunkAfter(chunk, last) {
		u.chunks = append(u.chunks, chunk)
		return false
	}

	// Insert in chronological order by PlayAt timeline
	idx := n
	for i := n - 1; i >= 0; i-- {
		if isChunkAfter(chunk, u.chunks[i]) {
			idx = i + 1
			break
		}
		if i == 0 {
			idx = 0
		}
	}
	u.chunks = append(u.chunks[:idx], append([]domain.AudioChunk{chunk}, u.chunks[idx:]...)...)
	if idx < u.readIdx {
		u.readIdx++
	}
	return false
}

func isChunkAfter(a, b domain.AudioChunk) bool {
	if !a.PlayAt.IsZero() && !b.PlayAt.IsZero() {
		return !a.PlayAt.Before(b.PlayAt)
	}
	return a.Timestamp >= b.Timestamp
}

// PeekNext returns the next unread raw chunk for conversion without consuming it.
func (u *UpstreamPlayer) PeekNext() (domain.AudioChunk, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()

	if u.readIdx >= len(u.chunks) {
		return domain.AudioChunk{}, false
	}
	return u.chunks[u.readIdx], true
}

// AdvanceRead marks the current unread chunk as processed by the conversion layer.
func (u *UpstreamPlayer) AdvanceRead() {
	u.mu.Lock()
	defer u.mu.Unlock()

	if u.readIdx < len(u.chunks) {
		u.readIdx++
	}
}

// RewindRead resets the conversion cursor back to the oldest unplayed chunk.
// Called on volume or codec changes to re-convert pending audio without dropping samples.
func (u *UpstreamPlayer) RewindRead() {
	u.mu.Lock()
	defer u.mu.Unlock()

	u.readIdx = 0
}

// AcknowledgePlayed removes n chunks from the head of the buffer that have completed playout.
func (u *UpstreamPlayer) AcknowledgePlayed(n int) {
	u.mu.Lock()
	defer u.mu.Unlock()

	if n <= 0 {
		return
	}
	if n > len(u.chunks) {
		n = len(u.chunks)
	}
	u.chunks = u.chunks[n:]
	u.readIdx -= n
	if u.readIdx < 0 {
		u.readIdx = 0
	}
}

// Pop retrieves and removes the oldest raw chunk from the timeline.
func (u *UpstreamPlayer) Pop() (domain.AudioChunk, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()

	if len(u.chunks) == 0 {
		return domain.AudioChunk{}, false
	}
	chunk := u.chunks[0]
	u.chunks = u.chunks[1:]
	if u.readIdx > 0 {
		u.readIdx--
	}
	return chunk, true
}

// Peek returns the oldest raw chunk without removing it.
func (u *UpstreamPlayer) Peek() (domain.AudioChunk, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()

	if len(u.chunks) == 0 {
		return domain.AudioChunk{}, false
	}
	return u.chunks[0], true
}

// DiscardBefore removes raw chunks whose scheduled PlayAt is older than cutoff.
// This allows live mode to instantly drop obsolete audio before conversion without wasting CPU.
func (u *UpstreamPlayer) DiscardBefore(cutoff time.Time) int {
	u.mu.Lock()
	defer u.mu.Unlock()

	dropped := 0
	for len(u.chunks) > 0 {
		c := u.chunks[0]
		if !c.PlayAt.IsZero() && c.PlayAt.Before(cutoff) {
			u.chunks = u.chunks[1:]
			dropped++
		} else {
			break
		}
	}
	u.readIdx -= dropped
	if u.readIdx < 0 {
		u.readIdx = 0
	}
	return dropped
}

// Len returns the count of buffered raw chunks (unplayed).
func (u *UpstreamPlayer) Len() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.chunks)
}

// UnreadLen returns the count of raw chunks that have not yet been converted.
func (u *UpstreamPlayer) UnreadLen() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.readIdx >= len(u.chunks) {
		return 0
	}
	return len(u.chunks) - u.readIdx
}

// Clear flushes all raw chunks from the timeline (called on seek or stream stop).
func (u *UpstreamPlayer) Clear() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.chunks = nil
	u.readIdx = 0
}

// Timestamps returns the timestamps in microseconds of the oldest and newest unplayed raw chunks.
func (u *UpstreamPlayer) Timestamps() (firstUs int64, lastUs int64, count int) {
	u.mu.Lock()
	defer u.mu.Unlock()

	n := len(u.chunks)
	if n == 0 {
		return 0, 0, 0
	}
	return u.chunks[0].Timestamp, u.chunks[n-1].Timestamp, n
}

// PlayAtBounds returns the wall-clock PlayAt timestamps of the oldest and newest unplayed raw chunks.
func (u *UpstreamPlayer) PlayAtBounds() (oldest, newest time.Time, count int) {
	u.mu.Lock()
	defer u.mu.Unlock()

	n := len(u.chunks)
	if n == 0 {
		return time.Time{}, time.Time{}, 0
	}
	return u.chunks[0].PlayAt, u.chunks[n-1].PlayAt, n
}
