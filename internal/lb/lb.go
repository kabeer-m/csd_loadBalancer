// Package lb ties backends and metrics together: it decides which
// backend gets the next request (scheduling), keeps their health flags
// current (health.go), and does the actual proxying with backpressure
// and safe retries (handler.go). Splitting it this way means you can
// read "how do we pick a backend" without wading through retry logic,
// and vice versa.
package lb

import (
	"loadbalancer/internal/backend"
	"loadbalancer/internal/metrics"
)

// LoadBalancer holds the backend pool and shared request metrics.
type LoadBalancer struct {
	Backends []*backend.Backend
	Metrics  *metrics.Metrics

	// MaxBodyBuf caps how large a request body we'll buffer in memory to
	// allow a retry against a second backend. See handler.go.
	MaxBodyBuf int
}

// New builds a LoadBalancer over an already-constructed set of backends.
func New(backends []*backend.Backend, m *metrics.Metrics, maxBodyBuf int) *LoadBalancer {
	return &LoadBalancer{
		Backends:   backends,
		Metrics:    m,
		MaxBodyBuf: maxBodyBuf,
	}
}

// NextBackend returns the healthy backend with the fewest currently admitted
// requests. With only a few backends, a linear scan is cheaper and simpler
// than maintaining a separate heap or queue.
func (lb *LoadBalancer) NextBackend() *backend.Backend {
	if len(lb.Backends) == 0 {
		return nil
	}

	var best *backend.Backend
	bestLoad := int(^uint(0) >> 1)

	for _, b := range lb.Backends {
		if !b.Alive.Load() {
			continue
		}
		load := b.InFlight()
		if best == nil || load < bestLoad {
			best = b
			bestLoad = load
		}
	}

	// If health checking has temporarily marked every backend unhealthy,
	// still allow one request through rather than blackholing the whole pool.
	if best != nil {
		return best
	}

	best = lb.Backends[0]
	bestLoad = best.InFlight()
	for _, b := range lb.Backends[1:] {
		if load := b.InFlight(); load < bestLoad {
			best = b
			bestLoad = load
		}
	}
	return best
}
