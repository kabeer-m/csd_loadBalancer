package lb

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
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

	if !canRetry {
		// Nothing will be retried, so we can write straight to the real
		// ResponseWriter. This keeps the fast path cheap and preserves
		// Hijack support (e.g. WebSocket upgrades) for the common case.
		lb.serveOnce(w, r, start)
		return
	}
	lb.serveWithRetry(w, r, start)
}

// serveOnce sends the request to exactly one backend and writes its
// response directly to the real client. Used whenever a retry would be
// unsafe (e.g. POST) - there's nothing to "undo" if it fails, so there's
// no need to buffer.
func (lb *LoadBalancer) serveOnce(w http.ResponseWriter, r *http.Request, start time.Time) {
	b := lb.pickUntried(nil)
	if b == nil {
		lb.Metrics.RecordLatency(time.Since(start))
		lb.Metrics.Failed.Add(1)
		http.Error(w, "no healthy backend available", http.StatusServiceUnavailable)
		return
	}
	if !b.TryAdmit() {
		lb.Metrics.Rejected.Add(1)
		lb.Metrics.RecordLatency(time.Since(start))
		lb.Metrics.Failed.Add(1)
		http.Error(w, "backend is saturated, try again shortly", http.StatusServiceUnavailable)
		return
	}

	sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
	b.Serve(sw, r)
	b.Release()

	lb.Metrics.RecordLatency(time.Since(start))
	if sw.status < 500 {
		lb.Metrics.Success.Add(1)
	} else {
		lb.Metrics.Failed.Add(1)
	}
}

// serveWithRetry is used only for methods that are safe to repeat. An
// HTTP response can't be taken back once it has been written to the real
// client connection - so each attempt here is proxied into an in-memory
// recorder first. Only the attempt we decide to keep is copied to the
// real ResponseWriter, and it's copied exactly once, no matter how many
// backends we had to try. This is what a naive retry loop gets wrong: it
// wraps the *same* real ResponseWriter on every attempt, so a failed
// first attempt can write a real error response to the client before a
// second attempt gets a chance - producing two responses on one
// connection and corrupting whatever request the client sends next on
// it (visible as "http: superfluous response.WriteHeader call" in the
// server log).
func (lb *LoadBalancer) serveWithRetry(w http.ResponseWriter, r *http.Request, start time.Time) {
	tried := make(map[*backend.Backend]bool, 2)
	var last *httptest.ResponseRecorder
	var sawRejection bool

	for attempt := 0; attempt < 2; attempt++ {
		b := lb.pickUntried(tried)
		if b == nil {
			continue
		}

		if !b.TryAdmit() {
			lb.Metrics.Rejected.Add(1)
			sawRejection = true
			continue
		}
		tried[b] = true

		rec := httptest.NewRecorder()
		b.Serve(rec, r)
		b.Release()
		last = rec
		sawRejection = false

		if rec.Code < 500 {
			break // success - stop, we'll flush this one
		}
		if rec.Code != http.StatusBadGateway {
			// A live backend returned its own real error. That's an
			// application problem, not a routing one - retrying
			// elsewhere won't fix it, so stop and report this attempt.
			break
		}
		// rec.Code == 502: a connection-level failure from our own
		// ErrorHandler. Safe to discard and try the other backend.
	}

	lb.Metrics.RecordLatency(time.Since(start))

	if last == nil {
		lb.Metrics.Failed.Add(1)
		if sawRejection {
			http.Error(w, "backend is saturated, try again shortly", http.StatusServiceUnavailable)
		} else {
			http.Error(w, "no healthy backend available", http.StatusServiceUnavailable)
		}
		return
	}

	flushRecorded(w, last)
	if last.Code < 500 {
		lb.Metrics.Success.Add(1)
	} else {
		lb.Metrics.Failed.Add(1)
	}
}

// flushRecorded copies a buffered response to the real ResponseWriter:
// headers, then status line, then body - in that order, exactly once.
func flushRecorded(w http.ResponseWriter, rec *httptest.ResponseRecorder) {
	dst := w.Header()
	for k, v := range rec.Header() {
		dst[k] = v
	}
	w.WriteHeader(rec.Code)
	_, _ = io.Copy(w, rec.Body)
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
