package domain

import (
	"cmp"
	"fmt"
	"strings"
	"sync"
)

// ConflictPolicy defines how to handle multiple players attempting to play to the same physical SIP target.
type ConflictPolicy string

const (
	ConflictPolicyPreemptHigher        ConflictPolicy = "preempt_higher"            // Preempt active music if new priority > current
	ConflictPolicyPreemptAnnouncements ConflictPolicy = "preempt_for_announcements" // Backward-compatibility alias for preempt_higher
	ConflictPolicyPreemptAlways        ConflictPolicy = "preempt_always"            // Always preempt if new priority >= current
	ConflictPolicyBusy                 ConflictPolicy = "busy"                      // Reject new playback if target is busy
)

// ParseConflictPolicy normalizes and validates a target conflict policy.
func ParseConflictPolicy(s string) (ConflictPolicy, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "preempt_higher", "preempt_for_announcements", "priority":
		return ConflictPolicyPreemptHigher, nil
	case "preempt_always":
		return ConflictPolicyPreemptAlways, nil
	case "busy":
		return ConflictPolicyBusy, nil
	default:
		return "", fmt.Errorf("invalid target_conflict_policy %q (allowed: preempt_higher, preempt_always, busy)", s)
	}
}

// TargetArbiter manages concurrency and preemption for physical SIP targets.
type TargetArbiter struct {
	mu             sync.Mutex
	policy         ConflictPolicy
	activeSessions map[string]*CallSession // keyed by NormalizeSIPTarget(SIPTarget)
}

// NewTargetArbiter creates a new target arbiter.
func NewTargetArbiter(policy ConflictPolicy) *TargetArbiter {
	return &TargetArbiter{
		policy:         cmp.Or(policy, ConflictPolicyPreemptHigher),
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
		shouldPreempt = newSession.Priority >= current.Priority
	case ConflictPolicyPreemptHigher, ConflictPolicyPreemptAnnouncements:
		shouldPreempt = newSession.Priority > current.Priority
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
