// Package backend represents a single downstream server the load balancer
// can forward traffic to.
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

// Config groups the options for New.
type Config struct {
	// Transport is shared across all backends so they use one connection pool.
	Transport http.RoundTripper

	// MaxInFlight bounds concurrent requests to this backend.
	// TryAdmit returns false when the limit is reached.
	MaxInFlight int

	// OnError is called on connection-level failures (refused, reset, timeout).
	OnError func(b *Backend, err error)
}

// New parses rawURL and returns a Backend with a reverse proxy configured.
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
	b.Alive.Store(true) // optimistic; health loop corrects this quickly

	proxy := httputil.NewSingleHostReverseProxy(u)
	proxy.Transport = cfg.Transport

	// Fix the Host header so backends that validate it see their own host,
	// not the client's original host.
	orig := proxy.Director
	proxy.Director = func(req *http.Request) {
		orig(req)
		req.Host = u.Host
	}

	proxy.ErrorHandler = func(rw http.ResponseWriter, req *http.Request, err error) {
		if cfg.OnError != nil {
			cfg.OnError(b, err)
		}
		http.Error(rw, "backend unavailable", http.StatusBadGateway)
	}

	b.proxy = proxy
	return b, nil
}

// TryAdmit reserves one concurrency slot without blocking.
// Returns false if the backend is already at MaxInFlight.
func (b *Backend) TryAdmit() bool {
	select {
	case b.admit <- struct{}{}:
		return true
	default:
		return false
	}
}

// Release frees one concurrency slot. Must be called once per successful TryAdmit.
func (b *Backend) Release() {
	<-b.admit
}

// InFlight returns the number of currently admitted requests.
func (b *Backend) InFlight() int {
	return len(b.admit)
}

// Serve forwards the request through this backend's reverse proxy.
func (b *Backend) Serve(w http.ResponseWriter, r *http.Request) {
	b.proxy.ServeHTTP(w, r)
}

// HealthURL returns the URL the health checker should probe.
func (b *Backend) HealthURL() string {
	return strings.TrimRight(b.URL.String(), "/") + "/health"
}

