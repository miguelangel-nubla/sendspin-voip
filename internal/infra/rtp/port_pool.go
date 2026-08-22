package rtp

import (
	"fmt"
	"net"
	"sync"
	"time"
)

// PortPool manages allocation and recycling of even UDP ports for RTP streams.
// It features thread-safe round-robin distribution, in-use tracking, and
// release cooldown to prevent socket reuse race conditions under rapid churning.
type PortPool struct {
	mu         sync.Mutex
	minPort    int
	maxPort    int
	nextPort   int
	cooldown   time.Duration
	allocated  map[int]bool
	releasedAt map[int]time.Time
}

// NewPortPool creates a port pool bounded to even port numbers in [minPort, maxPort].
func NewPortPool(minPort, maxPort int) *PortPool {
	if minPort <= 0 {
		minPort = 10000
	}
	if minPort%2 != 0 {
		minPort++
	}
	if maxPort <= minPort {
		maxPort = minPort + 10000
	}
	if maxPort%2 != 0 {
		maxPort--
	}

	return &PortPool{
		minPort:    minPort,
		maxPort:    maxPort,
		nextPort:   minPort,
		cooldown:   3 * time.Second,
		allocated:  make(map[int]bool),
		releasedAt: make(map[int]time.Time),
	}
}

// Allocate searches round-robin for an available even UDP port, binds it, and returns the listener.
func (p *PortPool) Allocate() (*net.UDPConn, int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	totalPorts := ((p.maxPort - p.minPort) / 2) + 1
	now := time.Now()

	tryPass := func(ignoreCooldown bool) (*net.UDPConn, int, bool) {
		for i := 0; i < totalPorts; i++ {
			port := p.nextPort
			p.advanceNextPort()

			if p.allocated[port] {
				continue
			}

			if !ignoreCooldown {
				if relTime, ok := p.releasedAt[port]; ok && now.Sub(relTime) < p.cooldown {
					continue
				}
			}

			conn, err := bindUDPPort(port)
			if err == nil {
				p.allocated[port] = true
				delete(p.releasedAt, port)
				return conn, port, true
			}
		}
		return nil, 0, false
	}

	// Pass 1: Prefer ports not currently allocated and outside of cooldown
	if conn, port, ok := tryPass(false); ok {
		return conn, port, nil
	}

	// Pass 2: Fallback to unallocated ports even if still within cooldown
	if conn, port, ok := tryPass(true); ok {
		return conn, port, nil
	}

	return nil, 0, fmt.Errorf("no available RTP UDP ports in range [%d, %d]", p.minPort, p.maxPort)
}

func (p *PortPool) advanceNextPort() {
	p.nextPort += 2
	if p.nextPort > p.maxPort {
		p.nextPort = p.minPort
	}
}

// Release marks a port as released and starts its cooldown period.
func (p *PortPool) Release(port int) {
	if port <= 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.allocated, port)
	p.releasedAt[port] = time.Now()
}

func bindUDPPort(port int) (*net.UDPConn, error) {
	addr := &net.UDPAddr{IP: net.ParseIP("0.0.0.0"), Port: port}
	return net.ListenUDP("udp", addr)
}
