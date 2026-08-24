# Load Balancer Lab Report

**Student Name:** _____________________
**Roll Number:** _____________________

## Assigned Systems

| System | Role            | IP / Hostname | Port |
|--------|-----------------|----------------|------|
| Sys1   | Load Balancer   |                | 8080 |
| Sys2   | Backend-1       |                | 3210 |
| Sys3   | Backend-2       |                | 3210 |
| Sys4   | Backend-3       |                | 3210 |

---

## Architecture

```
Local PC (Load Generator) --HTTP--> Sys1 (Load Balancer) --> Sys2 / Sys3 / Sys4
                                                              (Messaging App backend)
```

The load balancer performs round-robin scheduling across healthy
backends, health-checks each backend's `/health` endpoint every second,
and reverse-proxies both regular HTTP requests and WebSocket traffic to
the selected backend.

---

## Load Balancer Code

_(Paste `loadbalancer/main.go` here, or attach it as an appendix.)_

---

## Comparison Table

_(Fill in from `results/comparison.csv`)_

| Experiment       | Requests | Concurrency | Successful | Failed | Throughput (RPS) | Dropout % | p50 (ms) | p95 (ms) | p99 (ms) |
|------------------|----------|-------------|------------|--------|-------------------|-----------|----------|----------|----------|
| Single Backend   |          |             |            |        |                   |           |          |          |          |
| Three Backends   |          |             |            |        |                   |           |          |          |          |

### Observations

- _(e.g. Did throughput increase with three backends? By how much?)_
- _(e.g. Did dropout/failure rate decrease?)_
- _(e.g. How did p95/p99 tail latency change?)_
- _(Any anomalies — e.g. a backend flapping unhealthy, uneven distribution — and your explanation)_

---

## Integration with Messaging App

- Confirm the messaging app's public/client URL now routes through the
  load balancer (Sys1:8080) rather than directly to a single backend.
- Note any limitations observed (e.g. per-backend SQLite state meaning
  two tabs landing on different backends won't see each other's history).

**Working URL (through load balancer):** _____________________

---

## Screenshots

1. Load balancer startup log showing all backends `ALIVE`
2. `/lb/status` output for both experiments
3. Load generator terminal output for both experiments
4. `comparison.csv` / comparison table view
5. Messaging app working end-to-end through the load balancer (two tabs chatting)
