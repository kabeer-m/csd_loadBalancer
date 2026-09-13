package lb

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"loadbalancer/internal/backend"
)

// bufferBody reads r.Body into memory (up to maxBytes) so the same
// request can be replayed against a second backend if the first attempt
// fails at the connection level. An http.Request's Body can only be read
// once, so without this, any retry would send an empty body. Returns
// false if the body was too large to safely buffer - in which case the
// caller should not attempt a retry for this request.
func bufferBody(r *http.Request, maxBytes int) (ok bool) {
	if r.Body == nil || r.ContentLength == 0 {
		return true
	}
	limited := io.LimitReader(r.Body, int64(maxBytes)+1)
	data, err := io.ReadAll(limited)
	r.Body.Close()
	if err != nil {
		return false
	}
	if len(data) > maxBytes {
		// Too large to buffer for a retry. Restore what we already read
		// so the single attempt we do make still gets the full body.
		r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(data), r.Body))
		return false
	}
	r.Body = io.NopCloser(bytes.NewReader(data))
	r.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(data)), nil
	}
	return true
}

// isIdempotent reports whether a method is safe to retry against a second
// backend. POST is deliberately excluded: if the first attempt actually
// reached the backend and only the reply was lost, retrying it elsewhere
// could store the same submission twice.
func isIdempotent(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodPut, http.MethodDelete:
		return true
	default:
		return false
	}
}

// statusWriter wraps a ResponseWriter so we can observe the status code a
// backend's response used, since http.ResponseWriter doesn't expose it
// after the fact.
type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (sw *statusWriter) WriteHeader(code int) {
	if sw.wroteHeader {
		return
	}
	sw.status = code
	sw.wroteHeader = true
	sw.ResponseWriter.WriteHeader(code)
}

func (sw *statusWriter) Write(b []byte) (int, error) {
	if !sw.wroteHeader {
		sw.WriteHeader(http.StatusOK)
	}
	return sw.ResponseWriter.Write(b)
}

// Hijack lets a WebSocket (or other protocol-upgrading) connection reach
// the raw TCP socket through our wrapper, if the underlying writer
// supports it.
func (sw *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := sw.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("underlying ResponseWriter does not support hijacking")
	}
	return hijacker.Hijack()
}

// ServeHTTP picks a backend and proxies the request to it, applying
// backpressure (fail fast instead of queuing indefinitely) and retrying
// once against a different backend when it's safe to do so.
func (lb *LoadBalancer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	lb.Metrics.Total.Add(1)
	start := time.Now()

	if len(lb.Backends) == 0 {
		lb.Metrics.Failed.Add(1)
		http.Error(w, "no backends configured", http.StatusServiceUnavailable)
		return
	}

	canRetry := isIdempotent(r.Method) && bufferBody(r, lb.MaxBodyBuf)
	attempts := 1
	if canRetry {
		attempts = 2
	}

	tried := make(map[*backend.Backend]bool, attempts)
	var lastStatus int

	for attempt := 0; attempt < attempts; attempt++ {
		b := lb.pickUntried(tried)
		if b == nil {
			continue
		}

		if !b.TryAdmit() {
			// This backend is already at its concurrency cap. Fail this
			// attempt fast (instead of queuing behind it) so we can try
			// another backend or return promptly.
			lb.Metrics.Rejected.Add(1)
			lastStatus = http.StatusServiceUnavailable
			continue
		}

		tried[b] = true
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		b.Serve(sw, r)
		b.Release()
		lastStatus = sw.status

		if sw.status < 500 {
			lb.Metrics.Success.Add(1)
			lb.Metrics.RecordLatency(time.Since(start))
			return
		}

		// Only retry on a genuine connection-level failure (marked 502 by
		// the backend package's ErrorHandler). A backend that is up but
		// returning its own 5xx is an application bug, not a routing
		// problem, and retrying it elsewhere wouldn't help.
		if sw.status != http.StatusBadGateway || !canRetry {
			break
		}
	}

	lb.Metrics.RecordLatency(time.Since(start))
	lb.Metrics.Failed.Add(1)
	if lastStatus == 0 || lastStatus == http.StatusServiceUnavailable {
		http.Error(w, "no healthy backend available", http.StatusServiceUnavailable)
	}
	// Otherwise the proxy's ErrorHandler already wrote a response
	// (e.g. 502) - nothing further to write.
}

// pickUntried calls NextBackend a bounded number of times to avoid
// picking a backend we've already tried in this request's retry loop.
func (lb *LoadBalancer) pickUntried(tried map[*backend.Backend]bool) *backend.Backend {
	n := len(lb.Backends)
	b := lb.NextBackend()
	for i := 0; i < n && (b == nil || tried[b]); i++ {
		b = lb.NextBackend()
	}
	if b == nil || tried[b] {
		return nil
	}
	return b
}
