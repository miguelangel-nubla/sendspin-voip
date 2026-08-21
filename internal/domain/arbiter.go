package domain

import (
	"fmt"
	"sync"
)

// ConflictPolicy defines how to handle multiple players attempting to play to the same physical SIP target.
type ConflictPolicy string

const (
	ConflictPolicyPreemptAnnouncements ConflictPolicy = "preempt_for_announcements" // Preempt active music for incoming announcements
	ConflictPolicyPreemptAlways        ConflictPolicy = "preempt_always"             // Always preempt if new priority >= current
	ConflictPolicyBusy                 ConflictPolicy = "busy"                       // Reject new playback if target is busy
)

// TargetArbiter manages concurrency and preemption for physical SIP targets.
type TargetArbiter struct {
	mu             sync.Mutex
	policy         ConflictPolicy
	activeSessions map[string]*CallSession // keyed by NormalizeSIPTarget(SIPTarget)
}

// NewTargetArbiter creates a new target arbiter.
func NewTargetArbiter(policy ConflictPolicy) *TargetArbiter {
	if policy == "" {
		policy = ConflictPolicyPreemptAnnouncements
	}
	return &TargetArbiter{
		policy:         policy,
		activeSessions: make(map[string]*CallSession),
	}
}

// RequestTarget attempts to claim a physical SIP target for a new call session.
// If the target is currently busy, it evaluates the preemption policy.
// Returns the session that was preempted (if any), or an error if the request is rejected.
func (a *TargetArbiter) RequestTarget(newSession *CallSession) (*CallSession, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	target := newSession.SIPTarget
	current, exists := a.activeSessions[target]

	if !exists || current.GetState() == StateTerminated {
		a.activeSessions[target] = newSession
		return nil, nil
	}

	// Target is currently busy with an active/dialing session. Evaluate preemption.
	shouldPreempt := false

	switch a.policy {
	case ConflictPolicyPreemptAlways:
		if newSession.Priority >= current.Priority {
			shouldPreempt = true
		}
	case ConflictPolicyPreemptAnnouncements:
		// Preempt if the new stream is configured in announcement mode and current is not, OR if new priority is strictly higher
		if (newSession.EffectiveMod == BufferModeAnnouncement && current.EffectiveMod != BufferModeAnnouncement) ||
			(newSession.Priority > current.Priority) {
			shouldPreempt = true
		}
	case ConflictPolicyBusy:
		shouldPreempt = false
	}

	if shouldPreempt {
		// Replace current active session
		a.activeSessions[target] = newSession
		return current, nil
	}

	return nil, fmt.Errorf("target %s is busy with active session from player %s (policy: %s)",
		target, current.PlayerID, a.policy)
}

// ReleaseTarget removes a session from active targets if it still holds the lock.
func (a *TargetArbiter) ReleaseTarget(session *CallSession) {
	a.mu.Lock()
	defer a.mu.Unlock()

	target := session.SIPTarget
	if current, exists := a.activeSessions[target]; exists && current.ID == session.ID {
		delete(a.activeSessions, target)
	}
}

// GetActiveSession returns the active session for a given target, if any.
func (a *TargetArbiter) GetActiveSession(target string) (*CallSession, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	sess, exists := a.activeSessions[NormalizeSIPTarget(target)]
	if exists && sess.GetState() != StateTerminated {
		return sess, true
	}
	return nil, false
}
