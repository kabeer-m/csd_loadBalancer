package lb

import (
	"log"
	"net/http"
	"sync"
	"time"

	"loadbalancer/internal/backend"
)

const (
	healthFailureThreshold = 3
	healthSuccessThreshold = 2
)

// HealthLoop probes every backend concurrently and uses hysteresis so a
// single transient timeout does not flap a backend in and out of rotation.
func (lb *LoadBalancer) HealthLoop(interval time.Duration) {
	client := &http.Client{Timeout: 1 * time.Second}
	failures := make(map[int]int)
	successes := make(map[int]int)
	first := true

	check := func() {
		type result struct {
			index int
			alive bool
		}

		results := make(chan result, len(lb.Backends))
		var wg sync.WaitGroup

		for i, b := range lb.Backends {
			wg.Add(1)
			go func(i int, b *backend.Backend) {
				defer wg.Done()

				resp, err := client.Get(b.HealthURL())
				alive := err == nil && resp != nil && resp.StatusCode < 500
				if resp != nil {
					resp.Body.Close()
				}

				results <- result{index: i, alive: alive}
			}(i, b)
		}

		wg.Wait()
		close(results)

		for res := range results {
			b := lb.Backends[res.index]
			wasAlive := b.Alive.Load()

			if first {
				b.Alive.Store(res.alive)
				failures[res.index] = 0
				successes[res.index] = 0
			} else if res.alive {
				failures[res.index] = 0
				successes[res.index]++
				if !wasAlive && successes[res.index] >= healthSuccessThreshold {
					b.Alive.Store(true)
					successes[res.index] = 0
				}
			} else {
				successes[res.index] = 0
				failures[res.index]++
				if wasAlive && failures[res.index] >= healthFailureThreshold {
					b.Alive.Store(false)
					failures[res.index] = 0
				}
			}

			nowAlive := b.Alive.Load()
			if nowAlive != wasAlive {
				state := "UNHEALTHY"
				if nowAlive {
					state = "ALIVE"
				}
				log.Printf("[health] backend %s -> %s", b.URL, state)
			}
		}

		first = false
	}

	check()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		check()
	}
}
