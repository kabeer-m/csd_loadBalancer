# Deployment & Experiment Guide

Files in this bundle:

```
loadbalancer/main.go, go.mod   -> the Go load balancer (runs on Sys1)
loadgen/client.go, go.mod      -> the Go load generator (runs on your local PC)
messaging-app-health-patch.md  -> the one patch needed on the messaging app backend
REPORT_TEMPLATE.md             -> fill this in and export to PDF for submission
```

Replace `SYS1` / `SYS2` / `SYS3` / `SYS4` below with your actual assigned
hostnames or IPs throughout.

---

## Step 0 — Prerequisites on every machine

Every Sys1–4 machine needs Go installed to build the LB/backends (Sys2–4
only need Go if you rebuild something; the messaging app itself is Python).

```bash
sudo apt update
sudo apt install golang-go
go version
```

Sys2, Sys3, Sys4 also need the messaging app's Python deps:

```bash
pip install aiohttp cryptography
```

---

## Step 1 — Patch and deploy the messaging app on Sys2, Sys3, Sys4

On **each** of Sys2, Sys3, Sys4:

```bash
git clone https://github.com/rohitraghuwanshi07/Chat-Application-.git
cd Chat-Application-/backend
```

Apply the `/health` patch described in `messaging-app-health-patch.md`
(required — the LB's health checker needs it, or it'll mark every backend
UNHEALTHY and refuse to route to it).

Then run it:

```bash
python3 server.py
```

It listens on `0.0.0.0:3210` by default. Confirm from that same machine:

```bash
curl http://localhost:3210/health   # -> ok
curl http://localhost:3210/         # -> HTML page
```

Repeat on Sys3 and Sys4 (same steps, same port 3210 is fine since they're
different machines).

> Keep these three terminals open/running for the whole assignment —
> if you close them the backend goes down and the LB will mark it
> unhealthy within one health-check interval.

---

## Step 2 — Build and run the load balancer on Sys1

Copy the `loadbalancer/` folder to Sys1 (scp, git, USB, whatever's
convenient), then:

```bash
cd loadbalancer
go build -o lb main.go
./lb -listen :8080 -backends http://SYS2:3210,http://SYS3:3210,http://SYS4:3210
```

You should see log lines like:

```
load balancer starting on :8080 with 3 backend(s):
  - http://SYS2:3210
  - http://SYS3:3210
  - http://SYS4:3210
[health] backend http://SYS2:3210 -> ALIVE
[health] backend http://SYS3:3210 -> ALIVE
[health] backend http://SYS4:3210 -> ALIVE
```

If a backend shows `UNHEALTHY`, double check the `/health` patch was
applied and the messaging app is actually running and reachable
(firewall / port open) from Sys1.

Sanity check from Sys1 or your local PC:

```bash
curl http://SYS1:8080/lb/status
curl http://SYS1:8080/
```

`/lb/status` should list all three backends as `"alive": true`, and `/`
should return the messaging app's HTML page (proxied through the LB).

---

## Step 3 — Build the load generator on your local PC

```bash
cd loadgen
go build -o loadgen client.go
```

---

## Step 4 — Experiment 1: single backend (Sys2 only)

Point the LB at just one backend so you get a clean single-server
baseline. Stop the Step-2 LB (Ctrl+C) and restart it with only Sys2:

```bash
./lb -listen :8080 -backends http://SYS2:3210
```

From your local PC:

```bash
./loadgen -url http://SYS1:8080 -requests 5000 -concurrency 40 \
    -experiment single_backend -out results -csv results/comparison.csv
```

This prints a summary and writes:
- `results/single_backend.json`
- `results/comparison.csv` (created fresh, one row)

---

## Step 5 — Experiment 2: three backends

Stop the LB again and restart it with all three:

```bash
./lb -listen :8080 -backends http://SYS2:3210,http://SYS3:3210,http://SYS4:3210
```

Run the **same** request count and concurrency so the comparison is fair:

```bash
./loadgen -url http://SYS1:8080 -requests 5000 -concurrency 40 \
    -experiment three_backends -out results -csv results/comparison.csv
```

`results/comparison.csv` now has two rows you can drop straight into your
report table.

---

## Step 6 — Screenshots to capture for the report

- `lb.go` log output showing all backends `ALIVE`
- `curl http://SYS1:8080/lb/status` output for both experiments
- The load generator's terminal summary for both runs
- `results/comparison.csv` opened in a spreadsheet/table view

---

## Step 7 — Integration: point the messaging app's public URL at the LB

The assignment requires the messaging app to now be reached **through**
the load balancer, not directly against one backend.

- If your class setup exposes the app via a lab IP/port (as in the
  previous assignment's `Client URL`), that URL should now point at
  **Sys1:8080** (the LB), not directly at Sys2/3/4.
- If you're using ngrok to expose it publicly, tunnel the **load
  balancer's** port instead of a backend's port:

  ```bash
  ngrok http 8080   # on Sys1, not on Sys2/3/4
  ```

Either way: open the resulting URL in two browser tabs and confirm you
can still chat in real time — this proves the WebSocket (`/ws`) traffic
is also flowing correctly through the reverse proxy, not just the
regular HTTP page load.

> Note: because the LB does round-robin per HTTP connection, if your
> group's messaging app is fully stateless per room (no in-memory-only
> state tied to a single backend process), it should behave normally.
> If two browser tabs land on *different* backends and messages don't
> sync between them, that's expected given three independent backend
> processes each with their own SQLite file — flag this in your report
> as an observed limitation rather than something to silently work
> around, since fixing it (shared DB/message bus) is out of scope for
> this assignment.

---

## Step 8 — Fill in and export the report

Use `REPORT_TEMPLATE.md`, fill in your details and the comparison table
from `results/comparison.csv`, add your screenshots, then export to PDF
and submit to Google Classroom.
