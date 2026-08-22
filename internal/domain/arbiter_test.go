package domain

import (
	"testing"
)

func TestTargetArbiter_Preemption(t *testing.T) {
	arbiter := NewTargetArbiter(ConflictPolicyPreemptHigher)

	// 1. First session: Music on Target Ext 101 (Priority 1)
	sess1 := NewCallSession("sess-1", "player-music", "sip:101@192.168.1.50", 1, StreamMetadata{
		Title:     "Song A",
		MediaType: "track",
	})
	sess1.SetState(StateActive)

	preempted, err := arbiter.RequestTarget(sess1)
	if err != nil {
		t.Fatalf("unexpected error claiming target: %v", err)
	}
	if preempted != nil {
		t.Fatalf("expected no preemption for first session")
	}

	// 2. Second session: Higher priority stream on the SAME Target Ext 101 (Priority 10)
	sess2 := NewCallSession("sess-2", "player-tts", "sip:101@192.168.1.50", 10, StreamMetadata{
		Title:     "Doorbell",
		MediaType: "announcement",
	})

	preempted, err = arbiter.RequestTarget(sess2)
	if err != nil {
		t.Fatalf("unexpected error during preemption request: %v", err)
	}
	if preempted == nil || preempted.ID != "sess-1" {
		t.Fatalf("expected sess-1 to be preempted, got %v", preempted)
	}

	// 3. Third session: Low priority stream trying to preempt active high priority session
	sess3 := NewCallSession("sess-3", "player-music-2", "sip:101@192.168.1.50", 1, StreamMetadata{
		Title:     "Song B",
		MediaType: "track",
	})

	_, err = arbiter.RequestTarget(sess3)
	if err == nil {
		t.Fatalf("expected error when low priority tries to claim busy target")
	}

	// 4. Release active session
	arbiter.ReleaseTarget(sess2)

	// 5. Now sess3 can claim target
	preempted, err = arbiter.RequestTarget(sess3)
	if err != nil {
		t.Fatalf("unexpected error claiming released target: %v", err)
	}
	if preempted != nil {
		t.Fatalf("expected no preemption after release")
	}
}
