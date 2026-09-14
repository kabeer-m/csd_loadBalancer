// Command loadbalancer runs a reverse-proxy load balancer with
// least-in-flight scheduling, active health checks, backpressure, and
// metrics. All the actual logic lives in the internal/ packages; this
// file's only job is reading configuration and wiring the pieces
// together, which is what makes it worth reading top to bottom in one
// sitting.
//
// Run on Sys1:
//
//	go run ./cmd/loadbalancer -listen :5000 \
//	    -backends http://SYS2:3210,http://SYS3:3210,http://SYS4:3210
//
// Monitoring endpoints:
//
//	GET /lb/health   -> is the LB itself alive
//	GET /lb/status   -> per-backend health/state
//	GET /lb/metrics  -> counters + latency summary
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
	flag.StringVar(&listen, "listen", ":5000", "address for the load balancer to listen on")
	flag.StringVar(&listen, "addr", ":5000", "alias for -listen")
	backendsRaw := flag.String("backends", "", "comma-separated list of backend URLs, e.g. http://SYS2:3210,http://SYS3:3210")
	healthInterval := flag.Duration("health-interval", 1*time.Second, "interval between health checks")
	backendTimeout := flag.Duration("backend-timeout", 10*time.Second, "max time to wait for a backend's response headers")
	maxInFlightPerBackend := flag.Int("max-inflight-per-backend", 100, "max concurrent requests allowed to a single backend")
	maxBodyBuf := flag.Int("max-retry-body-bytes", 1<<20, "largest request body (bytes) the LB will buffer to allow a retry on a different backend")
	flag.Parse()

	if strings.TrimSpace(*backendsRaw) == "" {
		log.Fatal("must supply -backends, e.g. -backends http://SYS2:3210,http://SYS3:3210")
	}

	m := metrics.New()

	// One shared transport for every backend so their connections come
	// from a single pooled dialer/keep-alive set, with a bounded
	// ResponseHeaderTimeout so a slow or stuck backend can never hold a
	// request open forever.
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

	backends := buildBackends(*backendsRaw, transport, *maxInFlightPerBackend, m)
	if len(backends) == 0 {
		log.Fatal("no valid backends parsed from -backends")
	}

	balancer := lb.New(backends, m, *maxBodyBuf)

	log.Printf("load balancer starting on %s (backend timeout: %v, max in-flight/backend: %d) with %d backend(s):",
		listen, *backendTimeout, *maxInFlightPerBackend, len(backends))
	for _, b := range backends {
		log.Printf("  - %s", b.URL)
	}

	go balancer.HealthLoop(*healthInterval)

	mux := http.NewServeMux()
	mux.HandleFunc("/lb/health", balancer.HandleHealth)
	mux.HandleFunc("/lb/status", balancer.HandleStatus)
	mux.HandleFunc("/lb/metrics", balancer.HandleMetrics)
	mux.HandleFunc("/", balancer.ServeHTTP)

	// A bare http.ListenAndServe uses zero-value timeouts, meaning a slow
	// or stalled client connection can be held open forever. Under
	// hundreds or thousands of concurrent clients, that alone can exhaust
	// file descriptors / goroutines and start refusing new connections.
	//
	// WriteTimeout must be large enough to cover a full retry: a safe
	// (idempotent) request can hit -backend-timeout once, then hit it
	// again on a second backend, before the load balancer gives up. If
	// WriteTimeout were shorter than that, the server would sever the
	// connection mid-retry - which looks like a "context canceled" error
	// in the logs and needlessly marks a backend unhealthy that might
	// have been about to succeed.
	writeTimeout := 2*(*backendTimeout) + 5*time.Second
	srv := &http.Server{
		Addr:              listen,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       90 * time.Second,
	}

	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

// buildBackends parses the comma-separated -backends flag and constructs
// one backend.Backend per entry, wiring its errors into the shared
// metrics.
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
