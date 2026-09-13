package lb

import (
	"log"
	"net/http"
	"time"
)

// HealthLoop probes every backend's /health endpoint on a fixed interval
// and updates its Alive flag. It runs forever (call it with `go`), so the
// caller owns the goroutine's lifetime.
func (lb *LoadBalancer) HealthLoop(interval time.Duration) {
	client := &http.Client{Timeout: 2 * time.Second}

	check := func() {
		for _, b := range lb.Backends {
			resp, err := client.Get(b.HealthURL())
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

	check() // run once immediately so we don't route to backends of unknown health

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		check()
	}
}
