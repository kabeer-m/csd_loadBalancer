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

// bufferBody reads r.Body into memory (up to maxBytes) so the request can be
// replayed against a second backend on failure. Returns false if the body
// exceeds maxBytes, in which case a retry is unsafe.
func bufferBody(r *http.Request, maxBytes int) bool {
	if r.Body == nil || r.ContentLength == 0 {
		return true
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, int64(maxBytes)+1))
	r.Body.Close()
	if err != nil {
		return false
	}
	if len(data) > maxBytes {
		// Body too large to buffer; restore what we read for the single attempt.
		r.Body = io.NopCloser(bytes.NewReader(data))
		return false
	}
	r.Body = io.NopCloser(bytes.NewReader(data))
	r.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(data)), nil
	}
	return true
}

// isIdempotent reports whether the method is safe to retry on a second backend.
// POST is excluded because a successful-but-lost reply could mean the action
// already happened.
func isIdempotent(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodPut, http.MethodDelete:
		return true
	default:
		return false
	}
}

// statusWriter wraps ResponseWriter to capture the status code after the fact.
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

// Hijack passes WebSocket (or other protocol-upgrade) connections through
// to the underlying ResponseWriter.
func (sw *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := sw.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("underlying ResponseWriter does not support hijacking")
	}
	return h.Hijack()
}

// ServeHTTP picks a backend and proxies the request, applying backpressure and
// retrying once on a different backend when safe.
func (lb *LoadBalancer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	lb.Metrics.Total.Add(1)
	start := time.Now()

	if len(lb.Backends) == 0 {
		lb.Metrics.Failed.Add(1)
		http.Error(w, "no backends configured", http.StatusServiceUnavailable)
		return
	}

	if isIdempotent(r.Method) && r.URL.Path != "/feed" && bufferBody(r, lb.MaxBodyBuf) {
		lb.serveWithRetry(w, r, start)
	} else {
		lb.serveOnce(w, r, start)
	}
}

// serveOnce sends the request to one backend and writes directly to the client.
// Used for non-retryable requests (e.g. POST).
func (lb *LoadBalancer) serveOnce(w http.ResponseWriter, r *http.Request, start time.Time) {
	tried := make(map[*backend.Backend]bool, len(lb.Backends))
	var admitted *backend.Backend

	for range lb.Backends {
		b := lb.pickUntried(tried)
		if b == nil {
			break
		}
		tried[b] = true
		if b.TryAdmit() {
			admitted = b
			break
		}
		lb.Metrics.Rejected.Add(1)
	}

	if admitted == nil {
		lb.Metrics.RecordLatency(time.Since(start))
		lb.Metrics.Failed.Add(1)
		http.Error(w, "backend is saturated, try again shortly", http.StatusServiceUnavailable)
		return
	}

	sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
	admitted.Serve(sw, r)
	admitted.Release()

	lb.Metrics.RecordLatency(time.Since(start))
	if sw.status < 500 {
		lb.Metrics.Success.Add(1)
	} else {
		lb.Metrics.Failed.Add(1)
	}
}

// serveWithRetry proxies idempotent requests through an in-memory recorder so
// that a failed first attempt can be discarded and retried on a second backend
// without writing any partial response to the client.
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
			break // success
		}
		if rec.Code != http.StatusBadGateway {
			// Backend returned its own error; retrying elsewhere won't help.
			break
		}
		// 502 means our ErrorHandler fired (connection failure) — safe to retry.
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

// flushRecorded copies a buffered response to the real ResponseWriter.
func flushRecorded(w http.ResponseWriter, rec *httptest.ResponseRecorder) {
	dst := w.Header()
	for k, v := range rec.Header() {
		dst[k] = v
	}
	w.WriteHeader(rec.Code)
	_, _ = io.Copy(w, rec.Body)
}

// pickUntried returns the next backend from the scheduler that hasn't been
// tried yet in this request's loop.
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

