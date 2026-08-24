// client.go
// ---------
// Concurrent load generator. Fires N requests at -concurrency workers
// against -url, then reports throughput, dropout percentage, and
// latency percentiles (p50/p95/p99), following the design in the
// "Building a Load Balancer in Go" slides.
//
// Run on your local PC:
//
//	go run ./loadgen -url http://SYS1:8080 -requests 5000 -concurrency 40 \
//	    -experiment baseline -out results -csv results/comparison.csv
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

type result struct {
	Experiment       string  `json:"experiment"`
	Requests         int     `json:"requests"`
	Concurrency      int     `json:"concurrency"`
	Successful       uint64  `json:"successful"`
	Failed           uint64  `json:"failed"`
	ThroughputRPS    float64 `json:"throughput_rps"`
	DropoutPercent   float64 `json:"dropout_percent"`
	P50Ms            float64 `json:"p50_ms"`
	P95Ms            float64 `json:"p95_ms"`
	P99Ms            float64 `json:"p99_ms"`
	ElapsedSeconds   float64 `json:"elapsed_seconds"`
}

func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(p * float64(len(sorted)-1))
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func main() {
	targetURL := flag.String("url", "", "target URL to load (e.g. http://SYS1:8080)")
	requests := flag.Int("requests", 1000, "total number of requests to send")
	concurrency := flag.Int("concurrency", 20, "number of concurrent workers")
	timeout := flag.Duration("timeout", 5*time.Second, "per-request client timeout")
	experiment := flag.String("experiment", "experiment", "label for this experiment run")
	outDir := flag.String("out", "results", "directory to write the per-experiment JSON result to")
	csvPath := flag.String("csv", "", "optional path to a cumulative CSV comparison file (appended to)")
	flag.Parse()

	if *targetURL == "" {
		log.Fatal("must supply -url, e.g. -url http://SYS1:8080")
	}

	client := &http.Client{Timeout: *timeout}

	var success atomic.Uint64
	var failed atomic.Uint64

	var latMu sync.Mutex
	latencies := make([]time.Duration, 0, *requests)

	jobs := make(chan int, *requests)
	for i := 0; i < *requests; i++ {
		jobs <- i
	}
	close(jobs)

	var wg sync.WaitGroup
	start := time.Now()

	for w := 0; w < *concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range jobs {
				reqStart := time.Now()
				resp, err := client.Get(*targetURL)
				elapsed := time.Since(reqStart)

				if err != nil {
					failed.Add(1)
					continue
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()

				if resp.StatusCode >= 500 {
					failed.Add(1)
				} else {
					success.Add(1)
					latMu.Lock()
					latencies = append(latencies, elapsed)
					latMu.Unlock()
				}
			}
		}()
	}

	wg.Wait()
	elapsed := time.Since(start)

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	ms := func(d time.Duration) float64 { return float64(d.Microseconds()) / 1000.0 }

	succ := success.Load()
	fail := failed.Load()
	total := succ + fail

	res := result{
		Experiment:     *experiment,
		Requests:       *requests,
		Concurrency:    *concurrency,
		Successful:     succ,
		Failed:         fail,
		ThroughputRPS:  float64(succ) / elapsed.Seconds(),
		DropoutPercent: 0,
		P50Ms:          ms(percentile(latencies, 0.50)),
		P95Ms:          ms(percentile(latencies, 0.95)),
		P99Ms:          ms(percentile(latencies, 0.99)),
		ElapsedSeconds: elapsed.Seconds(),
	}
	if total > 0 {
		res.DropoutPercent = float64(fail) / float64(total) * 100
	}

	// Print a human-readable summary.
	fmt.Printf("\n=== %s ===\n", res.Experiment)
	fmt.Printf("Requests:     %d (concurrency %d)\n", res.Requests, res.Concurrency)
	fmt.Printf("Successful:   %d\n", res.Successful)
	fmt.Printf("Failed:       %d\n", res.Failed)
	fmt.Printf("Elapsed:      %.2fs\n", res.ElapsedSeconds)
	fmt.Printf("Throughput:   %.1f RPS\n", res.ThroughputRPS)
	fmt.Printf("Dropout:      %.2f%%\n", res.DropoutPercent)
	fmt.Printf("p50:          %.1f ms\n", res.P50Ms)
	fmt.Printf("p95:          %.1f ms\n", res.P95Ms)
	fmt.Printf("p99:          %.1f ms\n", res.P99Ms)

	// Write per-experiment JSON.
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		log.Fatalf("failed to create out dir: %v", err)
	}
	jsonPath := filepath.Join(*outDir, res.Experiment+".json")
	f, err := os.Create(jsonPath)
	if err != nil {
		log.Fatalf("failed to create result file: %v", err)
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(res); err != nil {
		log.Fatalf("failed to write result file: %v", err)
	}
	f.Close()
	fmt.Printf("\nWrote %s\n", jsonPath)

	// Append to cumulative CSV comparison file, if requested.
	if *csvPath != "" {
		writeHeader := false
		if _, err := os.Stat(*csvPath); os.IsNotExist(err) {
			writeHeader = true
		}
		cf, err := os.OpenFile(*csvPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			log.Fatalf("failed to open CSV file: %v", err)
		}
		defer cf.Close()
		if writeHeader {
			fmt.Fprintln(cf, "experiment,requests,concurrency,successful,failed,throughput_rps,dropout_percent,p50_ms,p95_ms,p99_ms")
		}
		fmt.Fprintf(cf, "%s,%d,%d,%d,%d,%.2f,%.2f,%.2f,%.2f,%.2f\n",
			res.Experiment, res.Requests, res.Concurrency, res.Successful, res.Failed,
			res.ThroughputRPS, res.DropoutPercent, res.P50Ms, res.P95Ms, res.P99Ms)
		fmt.Printf("Appended to %s\n", *csvPath)
	}
}
