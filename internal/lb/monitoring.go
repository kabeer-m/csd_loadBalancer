package lb

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// HandleHealth answers whether the load balancer process itself is up -
// distinct from whether any backend is healthy.
func (lb *LoadBalancer) HandleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, "ok")
}

// backendStatus is the JSON shape for one backend in HandleStatus.
type backendStatus struct {
	URL      string `json:"url"`
	Alive    bool   `json:"alive"`
	InFlight int    `json:"in_flight"`
}

// HandleStatus reports each backend's current health flag and in-flight
// request count.
func (lb *LoadBalancer) HandleStatus(w http.ResponseWriter, r *http.Request) {
	out := struct {
		Backends []backendStatus `json:"backends"`
	}{}
	for _, b := range lb.Backends {
		out.Backends = append(out.Backends, backendStatus{
			URL:      b.URL.String(),
			Alive:    b.Alive.Load(),
			InFlight: b.InFlight(),
		})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

// HandleMetrics reports the counters and latency percentiles collected
// so far, as a JSON snapshot.
func (lb *LoadBalancer) HandleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(lb.Metrics.Snapshot())
}
