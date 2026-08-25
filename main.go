// main.go
// -------
// A Go load balancer: reverse proxy + round-robin scheduling + health
// checks + metrics, following the design from the "Building a Load
// Balancer in Go" slides.
//
// Run on Sys1:
//
//	go run ./loadbalancer -listen :5000 \
//	    -backends http://SYS2:3210,http://SYS3:3210,http://SYS4:3210
//
// Monitoring endpoints:
//
//	GET /lb/health   -> is the LB itself alive
//	GET /lb/status   -> per-backend health/state
//	GET /lb/metrics  -> counters + latency summary
//	*   /            -> proxied to a healthy backend (round robin)
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---------------------------------------------------------------------
// Backend
// ---------------------------------------------------------------------

// Backend represents one downstream server the LB can forward traffic to.
type Backend struct {
	URL      *url.URL
	Alive    atomic.Bool
	InFlight atomic.Int64
	proxy    *httputil.ReverseProxy
}

// ---------------------------------------------------------------------
// Metrics
// ---------------------------------------------------------------------

// Metrics stores counters for everything the LB has done.
type Metrics struct {
	Total         atomic.Uint64
	Success       atomic.Uint64
	Failed        atomic.Uint64
	BackendErrors atomic.Uint64

	LatencyMu sync.Mutex
	Latencies []time.Duration
}

func (m *Metrics) recordLatency(d time.Duration) {
	m.LatencyMu.Lock()
	m.Latencies = append(m.Latencies, d)
	// Keep the slice from growing without bound during long-running use.
	if len(m.Latencies) > 200000 {
		m.Latencies = m.Latencies[len(m.Latencies)-200000:]
	}
	m.LatencyMu.Unlock()
}

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

// ---------------------------------------------------------------------
// LoadBalancer
// ---------------------------------------------------------------------

type LoadBalancer struct {
	backends []*Backend
	next     atomic.Uint64
	metrics  Metrics
}

// nextBackend implements round-robin scheduling, skipping unhealthy
// backends. It scans at most len(backends) times before giving up.
func (lb *LoadBalancer) nextBackend() *Backend {
	n := len(lb.backends)
	if n == 0 {
		return nil
	}
	for i := 0; i < n; i++ {
		index := lb.next.Add(1) % uint64(n)
		b := lb.backends[index]
		if b.Alive.Load() {
			return b
		}
	}
	return nil
}

// healthLoop periodically probes GET <backend>/health for every backend.
func (lb *LoadBalancer) healthLoop(interval time.Duration) {
	client := &http.Client{Timeout: 2 * time.Second}
	check := func() {
		for _, b := range lb.backends {
			resp, err := client.Get(strings.TrimRight(b.URL.String(), "/") + "/health")
			alive := err == nil && resp != nil && resp.StatusCode < 500
			if resp != nil {
				resp.Body.Close()
			}
			wasAlive := b.Alive.Load()
			b.Alive.Store(alive)
			if wasAlive != alive {
				state := "UNHEALTHY"
				if alive {
					state = "ALIVE"
				}
				log.Printf("[health] backend %s -> %s", b.URL, state)
			}
		}
	}
	check() // run once immediately so we don't route to unknown backends
	for range time.Tick(interval) {
		check()
	}
}

// ServeHTTP picks a healthy backend and proxies the request to it,
// recording metrics along the way.
func (lb *LoadBalancer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	lb.metrics.Total.Add(1)
	start := time.Now()

	b := lb.nextBackend()
	if b == nil {
		lb.metrics.Failed.Add(1)
		http.Error(w, "no healthy backends available", http.StatusServiceUnavailable)
		return
	}

	b.InFlight.Add(1)
	defer b.InFlight.Add(-1)

	// sw wraps the ResponseWriter so we can tell if the proxy actually
	// succeeded (status < 500) once ServeHTTP returns.
	sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
	b.proxy.ServeHTTP(sw, r)

	elapsed := time.Since(start)
	lb.metrics.recordLatency(elapsed)

	if sw.status >= 500 {
		lb.metrics.Failed.Add(1)
	} else {
		lb.metrics.Success.Add(1)
	}
}

// statusWriter captures the HTTP status code written by the reverse proxy.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (sw *statusWriter) WriteHeader(code int) {
	sw.status = code
	sw.ResponseWriter.WriteHeader(code)
}

// ---------------------------------------------------------------------
// Monitoring endpoints
// ---------------------------------------------------------------------

func (lb *LoadBalancer) handleLBHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, "ok")
}

func (lb *LoadBalancer) handleStatus(w http.ResponseWriter, r *http.Request) {
	type backendStatus struct {
		URL      string `json:"url"`
		Alive    bool   `json:"alive"`
		InFlight int64  `json:"in_flight"`
	}
	out := struct {
		Backends []backendStatus `json:"backends"`
	}{}
	for _, b := range lb.backends {
		out.Backends = append(out.Backends, backendStatus{
			URL:      b.URL.String(),
			Alive:    b.Alive.Load(),
			InFlight: b.InFlight.Load(),
		})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func (lb *LoadBalancer) handleMetrics(w http.ResponseWriter, r *http.Request) {
	lb.metrics.LatencyMu.Lock()
	latencies := make([]time.Duration, len(lb.metrics.Latencies))
	copy(latencies, lb.metrics.Latencies)
	lb.metrics.LatencyMu.Unlock()

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })

	ms := func(d time.Duration) float64 { return float64(d.Microseconds()) / 1000.0 }

	out := struct {
		Total         uint64  `json:"total"`
		Success       uint64  `json:"success"`
		Failed        uint64  `json:"failed"`
		BackendErrors uint64  `json:"backend_errors"`
		P50Ms         float64 `json:"p50_ms"`
		P95Ms         float64 `json:"p95_ms"`
		P99Ms         float64 `json:"p99_ms"`
	}{
		Total:         lb.metrics.Total.Load(),
		Success:       lb.metrics.Success.Load(),
		Failed:        lb.metrics.Failed.Load(),
		BackendErrors: lb.metrics.BackendErrors.Load(),
		P50Ms:         ms(percentile(latencies, 0.50)),
		P95Ms:         ms(percentile(latencies, 0.95)),
		P99Ms:         ms(percentile(latencies, 0.99)),
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

// ---------------------------------------------------------------------
// main
// ---------------------------------------------------------------------

func main() {
	listen := flag.String("listen", ":5000", "address for the load balancer to listen on")
	backendsRaw := flag.String("backends", "", "comma-separated list of backend URLs, e.g. http://SYS2:4213,http://SYS3:4213")
	healthInterval := flag.Duration("health-interval", 1*time.Second, "interval between health checks")
	backendTimeout := flag.Duration("backend-timeout", 800*time.Millisecond, "backend request timeout")
	flag.Parse()

	if strings.TrimSpace(*backendsRaw) == "" {
		log.Fatal("must supply -backends, e.g. -backends http://SYS2:4213,http://SYS3:4213")
	}

	lb := &LoadBalancer{}

	parts := strings.Split(*backendsRaw, ",")
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		u, err := url.Parse(part)
		if err != nil {
			log.Fatalf("invalid backend URL %q: %v", part, err)
		}

		b := &Backend{URL: u}
		b.Alive.Store(true) // optimistic; healthLoop's first pass corrects this quickly

		proxy := httputil.NewSingleHostReverseProxy(u)
		proxy.Transport = &http.Transport{
			DialContext: (&net.Dialer{
				Timeout: *backendTimeout,
			}).DialContext,
			ResponseHeaderTimeout: *backendTimeout,
		}
		bb := b // capture for closure
		proxy.ErrorHandler = func(rw http.ResponseWriter, req *http.Request, err error) {
			bb.Alive.Store(false)
			lb.metrics.BackendErrors.Add(1)
			log.Printf("[proxy-error] backend %s: %v", bb.URL, err)
			http.Error(rw, "backend unavailable", http.StatusBadGateway)
		}
		b.proxy = proxy

		lb.backends = append(lb.backends, b)
	}

	log.Printf("load balancer starting on %s with %d backend(s):", *listen, len(lb.backends))
	for _, b := range lb.backends {
		log.Printf("  - %s", b.URL)
	}

	go lb.healthLoop(*healthInterval)

	mux := http.NewServeMux()
	mux.HandleFunc("/lb/health", lb.handleLBHealth)
	mux.HandleFunc("/lb/status", lb.handleStatus)
	mux.HandleFunc("/lb/metrics", lb.handleMetrics)
	mux.HandleFunc("/", lb.ServeHTTP)

	if err := http.ListenAndServe(*listen, mux); err != nil {
		log.Fatal(err)
	}
}
