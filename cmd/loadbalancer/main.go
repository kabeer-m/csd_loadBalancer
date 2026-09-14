// Command loadbalancer runs a reverse-proxy load balancer with least-in-flight
// scheduling, health checks, backpressure, and metrics.
//
// Usage:
//
//	go run ./cmd/loadbalancer -listen :5000 \
//	    -backends http://SYS2:3210,http://SYS3:3210,http://SYS4:3210
//
// Monitoring endpoints:
//
//	GET /lb/health   -> LB liveness
//	GET /lb/status   -> per-backend health and in-flight count
//	GET /lb/metrics  -> counters and latency percentiles
//	*   /            -> proxied to a healthy backend (least-in-flight)
package main

import (
	"flag"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"loadbalancer/internal/backend"
	"loadbalancer/internal/lb"
	"loadbalancer/internal/metrics"
)

func main() {
	var listen string
	flag.StringVar(&listen, "listen", ":5000", "address to listen on")
	backendsRaw := flag.String("backends", "", "comma-separated backend URLs")
	healthInterval := flag.Duration("health-interval", 1*time.Second, "interval between health checks")
	backendTimeout := flag.Duration("backend-timeout", 10*time.Second, "max wait for backend response headers")
	maxInFlight := flag.Int("max-inflight-per-backend", 100, "max concurrent requests per backend")
	maxBodyBuf := flag.Int("max-retry-body-bytes", 1<<20, "max request body bytes to buffer for retries")
	flag.Parse()

	if strings.TrimSpace(*backendsRaw) == "" {
		log.Fatal("must supply -backends, e.g. -backends http://SYS2:3210,http://SYS3:3210")
	}

	m := metrics.New()

	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   2 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxConnsPerHost:       100,
		MaxIdleConns:          300,
		MaxIdleConnsPerHost:   100,
		IdleConnTimeout:       30 * time.Second,
		ResponseHeaderTimeout: *backendTimeout,
		ExpectContinueTimeout: 1 * time.Second,
	}

	backends := buildBackends(*backendsRaw, transport, *maxInFlight, m)
	if len(backends) == 0 {
		log.Fatal("no valid backends parsed from -backends")
	}

	balancer := lb.New(backends, m, *maxBodyBuf)

	log.Printf("starting on %s (timeout: %v, max-inflight: %d) with %d backend(s):",
		listen, *backendTimeout, *maxInFlight, len(backends))
	for _, b := range backends {
		log.Printf("  - %s", b.URL)
	}

	go balancer.HealthLoop(*healthInterval)

	mux := http.NewServeMux()
	mux.HandleFunc("/lb/health", balancer.HandleHealth)
	mux.HandleFunc("/lb/status", balancer.HandleStatus)
	mux.HandleFunc("/lb/metrics", balancer.HandleMetrics)
	mux.HandleFunc("/", balancer.ServeHTTP)

	// WriteTimeout must cover two full backend timeouts (initial attempt + one retry).
	srv := &http.Server{
		Addr:              listen,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      2*(*backendTimeout) + 5*time.Second,
		IdleTimeout:       90 * time.Second,
	}

	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

// buildBackends parses the comma-separated -backends flag into Backend instances.
func buildBackends(raw string, transport http.RoundTripper, maxInFlight int, m *metrics.Metrics) []*backend.Backend {
	var backends []*backend.Backend
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		b, err := backend.New(part, backend.Config{
			Transport:   transport,
			MaxInFlight: maxInFlight,
			OnError: func(bb *backend.Backend, err error) {
				m.BackendErrors.Add(1)
				log.Printf("[proxy-error] backend %s: %v", bb.URL, err)
			},
		})
		if err != nil {
			log.Fatal(err)
		}
		backends = append(backends, b)
	}
	return backends
}
