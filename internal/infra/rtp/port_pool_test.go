package rtp

import (
	"sync"
	"testing"
	"time"
)

func TestPortPool_AllocateAndRelease(t *testing.T) {
	pool := NewPortPool(21000, 21010)

	conn1, port1, err := pool.Allocate()
	if err != nil {
		t.Fatalf("Allocate 1 failed: %v", err)
	}
	defer conn1.Close()

	if port1 < 21000 || port1 > 21010 || port1%2 != 0 {
		t.Fatalf("unexpected port1: %d", port1)
	}

	conn2, port2, err := pool.Allocate()
	if err != nil {
		t.Fatalf("Allocate 2 failed: %v", err)
	}
	defer conn2.Close()

	if port2 == port1 {
		t.Fatalf("expected distinct ports, got %d and %d", port1, port2)
	}

	// Release port1 and verify cooldown preference
	_ = conn1.Close()
	pool.Release(port1)

	// Next allocation should continue round-robin, avoiding recently released port1 while fresh ports exist
	conn3, port3, err := pool.Allocate()
	if err != nil {
		t.Fatalf("Allocate 3 failed: %v", err)
	}
	defer conn3.Close()

	if port3 == port1 {
		t.Errorf("expected round-robin to pick fresh port before cooled-down port %d, got %d", port1, port3)
	}
}

func TestPortPool_ExhaustionAndCooldownFallback(t *testing.T) {
	pool := NewPortPool(21100, 21104) // 3 even ports: 21100, 21102, 21104
	pool.cooldown = 10 * time.Second

	conn1, p1, err := pool.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	conn2, p2, err := pool.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	conn3, p3, err := pool.Allocate()
	if err != nil {
		t.Fatal(err)
	}

	// Pool should be exhausted now
	_, _, err = pool.Allocate()
	if err == nil {
		t.Fatal("expected error on exhausted pool")
	}

	// Close and release p1
	_ = conn1.Close()
	pool.Release(p1)

	// Allocation should succeed by falling back to p1 even though it is within cooldown
	conn4, p4, err := pool.Allocate()
	if err != nil {
		t.Fatalf("expected fallback allocation to succeed: %v", err)
	}
	if p4 != p1 {
		t.Fatalf("expected reallocated port %d, got %d", p1, p4)
	}

	_ = conn2.Close()
	_ = conn3.Close()
	_ = conn4.Close()
	pool.Release(p2)
	pool.Release(p3)
	pool.Release(p4)
}

func TestPortPool_ConcurrentAllocation(t *testing.T) {
	pool := NewPortPool(22000, 22100)
	var wg sync.WaitGroup

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, port, err := pool.Allocate()
			if err != nil {
				t.Errorf("concurrent allocate failed: %v", err)
				return
			}
			time.Sleep(10 * time.Millisecond)
			_ = conn.Close()
			pool.Release(port)
		}()
	}

	wg.Wait()
}
