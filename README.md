# Go Load Balancer

This is a small load balancer written in Go. A load balancer sits in front
of several backend servers and spreads incoming requests across them, so
no single server gets overwhelmed.

This one also checks that backends are alive, keeps basic stats, and
protects itself from overload. This README explains what each part does
in plain language.

---

## What is a load balancer, quickly?

Imagine 3 waiters (backend servers) and 1 host at the restaurant door
(the load balancer). Every customer (request) walks up to the host, and
the host sends them to whichever waiter should take the next customer.
The host also keeps an eye on which waiters are actually working today
(health checks), and if a waiter is swamped, the host stops sending them
new customers for a bit (backpressure).

That's the whole idea. Everything in this repo is one of those three
jobs: **picking who gets the next request**, **checking who's healthy**,
and **not overloading anyone**.

---

## Project layout

```
lbmod/
├── go.mod
├── cmd/loadbalancer/
│   └── main.go              <- start here
└── internal/
    ├── backend/
    │   └── backend.go       <- one backend server
    ├── metrics/
    │   └── metrics.go       <- counting requests and timing them
    └── lb/
        ├── lb.go            <- picks which backend is next
        ├── health.go        <- checks if backends are alive
        ├── handler.go       <- actually forwards the request
        └── monitoring.go    <- the /lb/... status pages
```

**Rule of thumb:** if you want to change *what* the load balancer does
(e.g. add a new scheduling strategy), you're in `internal/lb/`. If you
want to change *how a backend behaves* (e.g. how many requests it can
take at once), you're in `internal/backend/`. If you want to change *what
gets counted*, you're in `internal/metrics/`.

`cmd/loadbalancer/main.go` doesn't contain any real logic. It only reads
command-line flags and connects the pieces together. Read this file
first to see the big picture, then dive into the specific package you
care about.

---

## How to run it

You need Go installed. From inside the `lbmod` folder:

```bash
go run ./cmd/loadbalancer \
  -listen :5000 \
  -backends http://localhost:3001,http://localhost:3002
```

This starts the load balancer on port `5000`, forwarding requests to two
backend servers running on ports `3001` and `3002`.

Every backend is expected to have a `/health` endpoint that returns any
status code below 500 when it's OK.

### All the flags

| Flag | Default | What it does |
|---|---|---|
| `-listen` | `:5000` | Address the load balancer listens on |
| `-backends` | *(required)* | Comma-separated list of backend URLs |
| `-health-interval` | `1s` | How often to check if backends are alive |
| `-backend-timeout` | `10s` | How long to wait for a backend before giving up |
| `-max-inflight-per-backend` | `500` | Max requests sent to one backend at the same time |
| `-max-retry-body-bytes` | `1MB` | Largest request body we'll save in memory to allow a retry |

---

## The three jobs, one at a time

### 1. Picking who gets the next request (`lb.go`)

This uses **least-in-flight**: every time a new request arrives, the load
balancer looks at how many requests are currently being handled by each
backend, and sends the new one to whichever backend has the fewest. It's
like a supermarket checkout — you join the shortest queue, not the next
one in rotation.

If a backend is currently marked unhealthy, it gets skipped. If *every*
backend looks unhealthy (maybe the health check was just unlucky), we
still send the request somewhere rather than refuse it outright — one
bad health check shouldn't shut everything down.

---

## Scheduling algorithm

The scheduler lives in `internal/lb/lb.go` inside `NextBackend()`.

**Algorithm: Least-In-Flight**

Each backend tracks how many requests are currently admitted to it (its
"in-flight" count). When a new request arrives, `NextBackend` does a
single linear scan over all backends and picks the one with the lowest
in-flight count, skipping any that are marked unhealthy.

```
for each healthy backend:
    if backend.InFlight() < current best:
        best = backend
return best
```

Why this instead of round robin or random?

- **Round robin** hands requests out evenly by turn, but ignores the fact
  that some requests take much longer than others. If backend A is stuck
  on a slow database query, round robin will keep sending it new work
  anyway.

- **Least-in-flight** naturally accounts for speed differences. A fast
  backend finishes requests quickly, so its count stays low and it keeps
  getting more work. A slow or busy backend accumulates a higher count
  and gets fewer new requests until it catches up. No configuration
  needed — it adjusts by itself.

- **Why a linear scan?** With a small number of backends (typically 2–10),
  scanning every backend on every request is faster in practice than
  maintaining a sorted heap or priority queue. There's no lock contention
  on the list itself, and the in-flight count is read from a channel's
  `len()` which is a single memory read.

**Fallback when all backends are unhealthy**

If the health loop has temporarily marked every backend as unhealthy, the
scheduler doesn't refuse the request outright. It falls back to a second
scan over *all* backends (ignoring the alive flag) and still picks the
one with the lowest in-flight count. One bad sweep of health checks
shouldn't bring the whole system down.

### 2. Checking who's healthy (`health.go`)

Every `-health-interval` seconds, the load balancer sends a small request
to each backend's `/health` endpoint. If a backend replies with anything
below status 500, it's marked "alive". If it fails to reply, or replies
with an error, it's marked "unhealthy" and the scheduler in `lb.go` starts
skipping it.

This runs in the background forever, independent of actual user traffic.

### 3. Not overloading anyone (`backend.go` + `handler.go`)

Two safety nets:

- **Backpressure.** Each backend has a limit on how many requests it can
  handle at once (`-max-inflight-per-backend`). Think of it as a "table
  is full" sign — once a backend is at its limit, new requests are
  either sent to a different backend or told to wait, instead of piling
  up endlessly and slowing everything down.

- **Timeouts.** If a backend takes too long to respond
  (`-backend-timeout`), the load balancer gives up on it and reports an
  error, instead of hanging forever. A slow backend should never be able
  to freeze the whole system.

There's also a small **retry** feature: if a request fails because a
backend couldn't be reached at all (not because the backend gave a real
error response), and the request is a "safe to repeat" type (like `GET`),
the load balancer will try it once more on a different backend. Requests
that *change* something on the server (like `POST`) are never retried —
repeating those could accidentally do the same action twice.

---

## Checking on it while it runs

Three built-in pages, in plain JSON (except the first one):

| Endpoint | What it shows |
|---|---|
| `GET /lb/health` | Just confirms the load balancer itself is alive |
| `GET /lb/status` | Each backend's URL, whether it's alive, and how many requests it's currently handling |
| `GET /lb/metrics` | Total requests, successes, failures, and response time percentiles (p50/p95/p99) |

Example:

```bash
curl http://localhost:5000/lb/status
curl http://localhost:5000/lb/metrics
```

