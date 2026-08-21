package domain

import (
	"context"
	"sync"
	"time"
)

// SessionState represents the lifecycle state of a SIP call session.
type SessionState string

const (
	StateIdle        SessionState = "idle"
	StateDialing     SessionState = "dialing"
	StateActive      SessionState = "active"
	StateTerminating SessionState = "terminating"
	StateTerminated  SessionState = "terminated"
)

// CallSession represents an active SIP dialog and RTP stream for a physical SIP phone target.
type CallSession struct {
	mu           sync.RWMutex
	ID           string
	PlayerID     string
	SIPTarget    string
	Priority     int
	State        SessionState
	Metadata     StreamMetadata
	StartTime    time.Time
	AnswerTime   time.Time
	CancelFunc   context.CancelFunc
	Ctx          context.Context
	DrainDelay   time.Duration
	EffectiveMod BufferMode
}

// NewCallSession creates a new call session.
func NewCallSession(
	id string,
	playerID string,
	sipTarget string,
	priority int,
	meta StreamMetadata,
	mode BufferMode,
	drainDelay time.Duration,
) *CallSession {
	ctx, cancel := context.WithCancel(context.Background())
	return &CallSession{
		ID:           id,
		PlayerID:     playerID,
		SIPTarget:    sipTarget,
		Priority:     priority,
		State:        StateIdle,
		Metadata:     meta,
		StartTime:    time.Now(),
		CancelFunc:   cancel,
		Ctx:          ctx,
		DrainDelay:   drainDelay,
		EffectiveMod: mode,
	}
}

// SetState updates the session state thread-safely.
func (s *CallSession) SetState(state SessionState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.State = state
	if state == StateActive && s.AnswerTime.IsZero() {
		s.AnswerTime = time.Now()
	}
}

// GetState returns the current state.
func (s *CallSession) GetState() SessionState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.State
}

// IsActive returns true if the session is currently connected and streaming.
func (s *CallSession) IsActive() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.State == StateActive
}

// Close cancels the session context.
func (s *CallSession) Close() {
	if s.CancelFunc != nil {
		s.CancelFunc()
	}
	s.SetState(StateTerminated)
}
