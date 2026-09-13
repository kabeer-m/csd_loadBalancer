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

This uses **round robin**: request 1 goes to backend A, request 2 to
backend B, request 3 to backend C, request 4 back to A, and so on. It's
like dealing cards one at a time to each player in turn.

If a backend is currently marked unhealthy, it gets skipped. If *every*
backend looks unhealthy (maybe the health check was just unlucky), we
still send the request somewhere rather than refuse it outright — one
bad health check shouldn't shut everything down.

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

---

## A quick note on "percentiles"

`p50`, `p95`, and `p99` in `/lb/metrics` answer: "how slow was the
slowest request, for the slowest X% of requests?"

- `p50` = the *typical* response time (half of requests were faster,
  half slower).
- `p95` = 95% of requests were faster than this. The remaining 5% were
  slower — this shows you your "occasional bad experience" number.
- `p99` = same idea, but for the worst 1%.

Average response time can hide a few very slow requests. Percentiles
don't — that's why they're worth tracking separately from the average.

---

## Why it's split into files this way

The old version of this project was a single 400-line file. That made it
hard to find anything: scheduling logic, health checks, and the actual
request handling were all tangled together. Now:

- Want to know how requests are picked? → `internal/lb/lb.go`, ~40 lines.
- Want to know how health checks work? → `internal/lb/health.go`, ~30
  lines.
- Want to know what happens when a request comes in? →
  `internal/lb/handler.go`.

Each file does one job and is short enough to read in a couple of
minutes, which makes debugging much faster: you already know which file
the bug has to be in before you even open an editor.
