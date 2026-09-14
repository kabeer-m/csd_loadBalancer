package lb

import (
	"loadbalancer/internal/backend"
	"loadbalancer/internal/metrics"
)

// LoadBalancer holds the backend pool and shared request metrics.
type LoadBalancer struct {
	Backends []*backend.Backend
	Metrics  *metrics.Metrics

	// MaxBodyBuf caps how large a request body we'll buffer for retries.
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

// NextBackend returns the healthy backend with the fewest in-flight requests.
// Falls back to the least-loaded backend if all are marked unhealthy.
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
		if load := b.InFlight(); best == nil || load < bestLoad {
			best = b
			bestLoad = load
		}
	}

	if best != nil {
		return best
	}

	// All backends unhealthy: allow one request through rather than blackholing.
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

