// Package metrics tracks request counts and a rolling latency window.
package metrics

import (
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// maxSamples caps the number of latency samples kept in memory.
const maxSamples = 50000

// Metrics stores counters and latency samples for the load balancer.
// Atomic counters are safe to increment directly; the latency slice
// is guarded by mu.
type Metrics struct {
	Total         atomic.Uint64
	Success       atomic.Uint64
	Failed        atomic.Uint64
	BackendErrors atomic.Uint64
	Rejected      atomic.Uint64

	mu        sync.Mutex
	latencies []time.Duration
}

// New returns a zeroed Metrics.
func New() *Metrics {
	return &Metrics{}
}

// RecordLatency appends a latency sample, dropping the oldest once the
// window is full.
func (m *Metrics) RecordLatency(d time.Duration) {
	m.mu.Lock()
	m.latencies = append(m.latencies, d)
	if len(m.latencies) > maxSamples {
		m.latencies = m.latencies[len(m.latencies)-maxSamples:]
	}
	m.mu.Unlock()
}

// percentile returns the value at rank p (0.0–1.0) in a sorted slice.
func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(p * float64(len(sorted)-1))
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// Snapshot is a point-in-time JSON-friendly view of the metrics.
type Snapshot struct {
	Total         uint64  `json:"total"`
	Success       uint64  `json:"success"`
	Failed        uint64  `json:"failed"`
	BackendErrors uint64  `json:"backend_errors"`
	Rejected      uint64  `json:"rejected_saturated"`
	P50Ms         float64 `json:"p50_ms"`
	P95Ms         float64 `json:"p95_ms"`
	P99Ms         float64 `json:"p99_ms"`
}

// Snapshot returns a consistent point-in-time snapshot with latency percentiles.
func (m *Metrics) Snapshot() Snapshot {
	m.mu.Lock()
	latencies := make([]time.Duration, len(m.latencies))
	copy(latencies, m.latencies)
	m.mu.Unlock()

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })

	ms := func(d time.Duration) float64 { return float64(d.Microseconds()) / 1000.0 }

	return Snapshot{
		Total:         m.Total.Load(),
		Success:       m.Success.Load(),
		Failed:        m.Failed.Load(),
		BackendErrors: m.BackendErrors.Load(),
		Rejected:      m.Rejected.Load(),
		P50Ms:         ms(percentile(latencies, 0.50)),
		P95Ms:         ms(percentile(latencies, 0.95)),
		P99Ms:         ms(percentile(latencies, 0.99)),
	}
}

