// Package lb ties backends and metrics together: it decides which
// backend gets the next request (scheduling), keeps their health flags
// current (health.go), and does the actual proxying with backpressure
// and safe retries (handler.go). Splitting it this way means you can
// read "how do we pick a backend" without wading through retry logic,
// and vice versa.
package lb

import (
	"sync/atomic"

	"loadbalancer/internal/backend"
	"loadbalancer/internal/metrics"
)

// LoadBalancer holds the backend pool, the round-robin cursor, and the
// shared metrics all requests get recorded into.
type LoadBalancer struct {
	Backends []*backend.Backend
	Metrics  *metrics.Metrics

	// MaxBodyBuf caps how large a request body we'll buffer in memory to
	// allow a retry against a second backend. See handler.go.
	MaxBodyBuf int

	next atomic.Uint64
}

// New builds a LoadBalancer over an already-constructed set of backends.
func New(backends []*backend.Backend, m *metrics.Metrics, maxBodyBuf int) *LoadBalancer {
	return &LoadBalancer{
		Backends:   backends,
		Metrics:    m,
		MaxBodyBuf: maxBodyBuf,
	}
}

// NextBackend implements round-robin scheduling, skipping backends
// currently marked unhealthy. lb.next is an atomic counter shared by every
// goroutine handling a request concurrently: each call to Add(1) hands out
// a distinct, ever-increasing ticket with no lock required, and %n maps
// that ticket into the backend array, cycling 0..n-1 forever.
//
// If every backend looks unhealthy, we still return one instead of nil -
// a single bad health probe shouldn't permanently blackhole a backend
// that a real request might succeed against.
func (lb *LoadBalancer) NextBackend() *backend.Backend {
	n := len(lb.Backends)
	if n == 0 {
		return nil
	}

	for i := 0; i < n; i++ {
		index := lb.next.Add(1) % uint64(n)
		b := lb.Backends[index]
		if b.Alive.Load() {
			return b
		}
	}

	index := lb.next.Add(1) % uint64(n)
	return lb.Backends[index]
}
