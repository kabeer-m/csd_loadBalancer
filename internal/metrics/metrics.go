// Package metrics owns everything about counting what the load balancer
// has done: request counts, and a rolling window of latencies used to
// compute percentiles. Nothing in this package knows what a "backend" or
// a "request" looks like beyond a duration and a pass/fail outcome - that
// separation is what makes it independently testable.
package metrics

import (
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// maxSamples caps how many latency samples we keep in memory. Without a
// cap, a long-running process would grow this slice forever.
const maxSamples = 50000

// Metrics stores counters for everything the LB has done. The atomic
// counters can be incremented directly (they are safe for concurrent use
// on their own); the latency slice needs its own lock because "append and
// possibly truncate" is not a single atomic operation.
type Metrics struct {
	Total         atomic.Uint64
	Success       atomic.Uint64
	Failed        atomic.Uint64
	BackendErrors atomic.Uint64
	Rejected      atomic.Uint64 // requests turned away because every backend was saturated

	mu        sync.Mutex
	latencies []time.Duration
}

// New returns a ready-to-use, zeroed Metrics.
func New() *Metrics {
	return &Metrics{}
}

// RecordLatency appends one latency sample, trimming the oldest samples
// once the window is full (a simple fixed-size ring, implemented as a
// slice that drops its head).
func (m *Metrics) RecordLatency(d time.Duration) {
	m.mu.Lock()
	m.latencies = append(m.latencies, d)
	if len(m.latencies) > maxSamples {
		m.latencies = m.latencies[len(m.latencies)-maxSamples:]
	}
	m.mu.Unlock()
}

// percentile returns the value at rank p (0.0-1.0) in an already-sorted
// slice. This is "nearest-rank" percentile estimation - simple, and
// accurate enough for operational dashboards.
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

// Snapshot is a point-in-time, JSON-friendly view of the metrics, safe to
// hand to an HTTP handler without exposing the internal lock or slice.
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

// Snapshot copies the current latency samples out from under the lock,
// sorts the copy (never the live slice - sorting in place while another
// goroutine appends would be a data race), and computes percentiles.
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
