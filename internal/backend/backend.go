// Package backend represents a single downstream server the load balancer
// can forward traffic to. It owns the reverse proxy plumbing (so callers
// never touch httputil directly) and a concurrency limiter, so the rest
// of the codebase just sees "a thing I can ask to admit or serve a
// request" without caring how that's implemented underneath.
package backend

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
)

// Backend is one downstream target.
type Backend struct {
	URL   *url.URL
	Alive atomic.Bool

	proxy *httputil.ReverseProxy
	admit chan struct{} // counting semaphore: one slot per in-flight request
}

// Config groups everything New needs to build a Backend, so the
// constructor signature doesn't grow every time we add a knob.
type Config struct {
	// Transport is shared across all backends so their connections come
	// from one pooled http.Transport instead of each backend paying for
	// its own dial/keep-alive pool.
	Transport http.RoundTripper

	// MaxInFlight bounds how many requests may be sent to this backend at
	// once. Once full, TryAdmit returns false so the caller can fail fast
	// or try a different backend, instead of queuing indefinitely.
	MaxInFlight int

	// OnError is called whenever a request to this backend fails at the
	// connection level (refused, reset, timed out) - as opposed to the
	// backend replying with its own HTTP error status. The backend
	// package doesn't know about metrics; it just reports the fact.
	OnError func(b *Backend, err error)
}

// New parses rawURL and builds a Backend with its own reverse proxy
// wired up: connection pooling via the shared Transport, a corrected
// Host header (see below), and error handling that marks the backend
// unhealthy on a connection failure.
func New(rawURL string, cfg Config) (*Backend, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("invalid backend URL %q: %w", rawURL, err)
	}

	if cfg.MaxInFlight <= 0 {
		cfg.MaxInFlight = 1
	}

	b := &Backend{
		URL:   u,
		admit: make(chan struct{}, cfg.MaxInFlight),
	}
	b.Alive.Store(true) // optimistic; the health loop corrects this quickly

	proxy := httputil.NewSingleHostReverseProxy(u)
	proxy.Transport = cfg.Transport

	// NewSingleHostReverseProxy rewrites the scheme/host/path used to make
	// the *connection*, but leaves the Host *header* on the wire as
	// whatever the original client sent. Any backend that validates or
	// routes on Host (or sits behind its own vhost setup) would otherwise
	// reject or mis-route every single request. Fixing this here means
	// every caller of this package gets it for free.
	originalDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		originalDirector(req)
		req.Host = u.Host
	}

	proxy.ErrorHandler = func(rw http.ResponseWriter, req *http.Request, err error) {
		b.Alive.Store(false)
		if cfg.OnError != nil {
			cfg.OnError(b, err)
		}
		http.Error(rw, "backend unavailable", http.StatusBadGateway)
	}

	b.proxy = proxy
	return b, nil
}

// TryAdmit reserves one concurrency slot without blocking. It returns
// false immediately if the backend is already at MaxInFlight, which is
// what lets the load balancer fail fast instead of letting requests pile
// up behind an overloaded backend.
func (b *Backend) TryAdmit() bool {
	select {
	case b.admit <- struct{}{}:
		return true
	default:
		return false
	}
}

// Release frees one concurrency slot. Must be called exactly once for
// every successful TryAdmit.
func (b *Backend) Release() {
	<-b.admit
}

// InFlight reports how many requests are currently admitted - useful for
// a /status endpoint, not used for scheduling decisions here.
func (b *Backend) InFlight() int {
	return len(b.admit)
}

// Serve forwards the request through this backend's reverse proxy.
func (b *Backend) Serve(w http.ResponseWriter, r *http.Request) {
	b.proxy.ServeHTTP(w, r)
}

// HealthURL returns the address the health checker should probe.
func (b *Backend) HealthURL() string {
	return strings.TrimRight(b.URL.String(), "/") + "/health"
}
