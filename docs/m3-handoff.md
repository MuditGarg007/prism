# M3 handoff

As of 2026-10-20, M3 (resilience) is complete and wrapped up: all 9 steps are done, the open items that needed no decision were closed on 2026-10-20 (see "Wrap-up"), and `make lint test validate` passes with `-race`. Next is M4 (caching).

- The repo lives at `/home/mudit/prism`. The module is `github.com/MuditGarg007/prism`, on Go 1.27.
- M3 is committed on top of `131a8a7` (M2).
- Run `make lint test validate` to check the current state, `make fuzz` to run the seven 30 s fuzz targets, and `make chaos` (about 90 s) to run both chaos scenarios.
- Every `go` command prints `go: warning: ignoring go.mod in $GOPATH /home/mudit/go`. This comes from the local environment and is harmless.
- The source plan is `docs/project-plan.md`, section "M3: Resilience, Layer 3". Earlier state and decisions are in `docs/m1-handoff.md` and `docs/m2-handoff.md`.

## Decisions

These were accepted at the start of M3 ("start step 1"). Don't reopen them unless asked.

| # | Decision | Choice | Why |
| --- | --- | --- | --- |
| D1 | Availability | `Backend.Available()` = healthy, not ejected, and breaker not open, each an atomic load. Half-open probe slots are taken *after* the pick, by `Backend.Admit()`. A refusal adds the backend to `Tried` and picks again, without counting as a retry. | Balancers stay unchanged and lock-free. Taking a probe slot inside `Usable` would leak slots for backends the balancer looked at but didn't pick. |
| D2 | What a failure is | A connect error, a timeout, or any backend 5xx. A client cancel, a broken or oversized client body, a 4xx, and local rejections (429/413/503 overloaded) are neutral. Reported once per attempt. | A client must not be able to get healthy backends ejected. |
| D3 | Circuit breaker | One per backend. A 10-bucket sliding window. Opens when `total >= min_requests && failures/total >= failure_ratio`. Open for `open_for`, then half-open lets `half_open_probes` (default 3) through, and all of them must succeed. | Per backend, so one bad backend doesn't open the breaker for the whole pool. |
| D4 | Outlier detection | Opt-in `outlier` block. Ejection = min(base × n, max), with n decaying by 1 per `base_ejection` of clean time. An ejection that would exceed `max_ejection_percent` is skipped. Expiry is lazy. | Envoy-style. No timer goroutines, and a pool is never emptied by ejection alone. |
| D5 | Active health | One goroutine per backend, with jitter. Starts healthy. `Runtime.Start()`, stopped by `Close()`, so `-validate` never starts it. | A new pool serves at once, and validating a config sends nothing to backends. |
| D6 | Bounded queue | Route `queue_size` and `queue_timeout`. `Semaphore.Acquire` waits with a bounded waiter count. A shed request gets 503 `overloaded`. | Backpressure: latency can't grow without bound behind a busy route. |
| D7 | Retry budget | Finagle-style per route: retries ≤ `budget_ratio` × requests + `budget_min_per_sec` × window. Full-jitter backoff. The concurrency slot is held during backoff. | Retries can't multiply load during an outage. |
| D8 | Graceful shutdown | `shutdown: {drain_delay, timeout}` and an opt-in `ready_path` on the main listeners. A second signal exits immediately. An empty hijack registry is ready for M6. | Readiness moves to the admin port in M5. |

The request pipeline is now:

0. Ready path, in `server`, ahead of the access log (200, or 503 while draining) — **new in step 8**
1. Client IP
2. Path validation (400)
3. Route match (404)
4. Rate limit (429)
5. Body size check (413)
6. Concurrency limit, waiting in the bounded queue when one is set (503 `overloaded`; 499 if the client leaves while queued) — **queue new in step 6**
7. Pick a backend that is healthy, not ejected and admitted by its breaker (503 `no_backend` if none), then forward. Each attempt's outcome goes to the breaker and the outlier detector. A retry needs the retry budget's approval and waits a full-jitter backoff first. — **budget and backoff new in step 7**
8. Copy the response body. If the backend breaks it off, the attempt's success is overturned into a failure. — **new in the wrap-up**

## What is built

### Step 1: config (`internal/config`)

- New top-level `ready_path` (off unless set; must start with `/` and have no `?` or `#`) and `shutdown: { drain_delay: 0s, timeout: 15s }`.
- `circuit_breaker` gains `window` (default 10s, at least 1s because it is split into 10 buckets) and `half_open_probes` (default 3).
- New upstream block `outlier`: `consecutive_failures` 5, `base_ejection` 30s, `max_ejection` 300s, `max_ejection_percent` 50. `max_ejection` must be at least `base_ejection`, and the percent must be 1–100.
- Route `queue_size` and `queue_timeout` (default 1s once `queue_size` is set). A queue without `max_concurrency`, or a `queue_timeout` without `queue_size`, is a config error.
- `retry` gains `budget_min_per_sec` (default 10, at least 1) and `backoff: { base: 25ms, max: 250ms }`, with `max >= base`.
- With zero meaning "use the default", some values can't be set to 0, which is why `budget_min_per_sec` and `max_ejection_percent` have a minimum of 1.
- `shutdown.timeout` is already wired into `main.go`, which replaced its 15 s constant.
- `warnIgnored` takes `""` as the path for top-level options.

### Step 2: sliding window (`resilience/window.go`)

- `NewWindow(span, now)` splits the span into 10 buckets and keeps a running sum, so `Sum` is O(1). Moving forward clears each bucket it passes over; a gap of a full span or more clears them all at once.
- The sum covers the current bucket and the 9 before it, so it sees between 90% and 100% of the span.
- `Counts{Total, Marked}` serves both users. The breaker marks failures. The retry budget will count every attempt in Total and mark the retries, so requests = Total − Marked.
- Elapsed time is `now.Sub(origin)`, which uses the monotonic clock. A time older than the head counts in the head bucket, so a caller that read the clock early can't land in an expired bucket.
- Not safe for concurrent use: the owner already holds a lock around its own state. `Reset()` empties it.
- Tests: sliding expiry at exact bucket edges, a gap longer than the span, a time before the head, reset, and a span too small to split. `FuzzWindow` checks it against a plain list of events and is in `make fuzz`.

### Step 3: circuit breaker (`resilience/breaker.go`, wired into `upstream` and `proxy`)

- **States.** `BreakerClosed`, `BreakerOpen`, `BreakerHalfOpen`, which print as `closed`, `open` and `half_open`.
- **Closed.** Every success or failure goes into the window. Only a failure checks the trip condition, because only a failure can push the ratio up.
- **Open.** `Allow` refuses until `open_for` has passed. The next `Allow` after that moves it to half-open. `State()` keeps reporting open until then, while `Ready()` already says true.
- **Half-open.** At most `half_open_probes` requests are admitted. Once that many have succeeded it closes, and the window is reset so the failures that opened it are gone. The first failure opens it again for a fresh `open_for`. A neutral outcome frees its slot for another request.
- **Tickets.** `Allow` returns a `Ticket` carrying a generation number, which goes up on every state change. `Report` ignores a ticket from an older generation. So a slow request admitted while closed can neither fail nor pass a half-open probe, and a second probe's success can't count after the first probe has already reopened the breaker.
- **Concurrency.** `Ready()` is lock-free (atomic state, `openUntil`, probe count), because balancers call it on every pick. `Allow` while closed is lock-free too: it reads the generation before the state, and state changes write them in the other order, so a race can only produce a stale ticket, which costs one sample. Half-open `Allow` and every `Report` take the mutex.
- **Logging.** The `onChange` callback runs outside the lock. `upstream` logs "circuit breaker state changed" with `upstream`, `backend`, `from` and `to`, at WARN when it opens and INFO otherwise.
- **`resilience.Outcome`** is `Neutral`, `Success` or `Failure`, shared with the outlier detector.
- **`upstream` wiring:**
  - `NewPool(name, u, newBal, log)` takes a logger now and builds a breaker per backend when `circuit_breaker` is set. The callers in runtime and the proxy and upstream tests were updated.
  - `Backend.Admit() (Admission, bool)` asks the breaker, and always admits when there is none. `Backend.Report(Admission, Outcome)` feeds the breaker and the outlier detector.
  - `Backend.Breaker()` exposes the breaker for tests now and the admin API in M5.
- **`proxy` wiring:**
  - `pick(pool, &choice)` loops Pick then Admit. A refusal adds the backend to `Tried`, so the loop ends once the pool runs out (`ErrNoBackend`, 503 `no_backend`). It is used for the first pick and in `retryTarget`.
  - `forward` calls `b.Report(adm, outcome(...))` right after each `RoundTrip`, before deciding on a retry.
  - `outcome` is neutral if the client's context is done or `body.clientFailed()`. Otherwise it is a failure on an error or a status ≥ 500, and a success otherwise. It is decided on the response headers; a body that then breaks off overturns a success (see "Wrap-up").
  - `requestBody` now always wraps a streamed body in `readCounter`, on routes without retries too, and `readCounter` records any read error other than `io.EOF`. A `MaxBytesError` counts as one, so oversized chunked uploads are neutral as well. The old `stream` field is gone.
- `warnIgnored` no longer lists `circuit_breaker` or `outlier`.

### Step 4: outlier detection (`resilience/outlier.go`, wired into `upstream`)

- `NewOutlier(n, cfg)` covers a pool of n members, numbered 0 to n−1. `upstream.NewPool` builds one per pool when `outlier` is set, and each `Backend` stores its member number.
- **Hot path.** `Ejected(i)` is one atomic load while a member is not ejected. An expired ejection is cleared with a CAS, so later calls don't read the clock. `Report(i, Success)` loads the failure counter and writes it only if it is non-zero. Each member is padded to 64 bytes so neighbours' counters don't share a cache line; a test checks the size.
- **Ejecting.** The failure that completes a streak of `consecutive_failures` takes the pool mutex. Inside, it rechecks the streak and that the member isn't already ejected. Failures of requests still in flight when the backend was ejected land here and do nothing.
- **Percent cap.** The other members that are currently ejected are counted. If `(ejected+1)×100 > percent × pool size`, the ejection is skipped. Either way the streak resets, so a member the cap holds in needs a new streak instead of taking the lock on every failure. With the default 50%, a pool of one is never ejected and a pool of two keeps one member.
- **Duration.** n is decayed first, by one for each `base_ejection` of time since the last ejection ended. Then n goes up by one, and the ejection lasts min(base × n, max).
- **Logging.** `Backend.Report` logs "backend ejected after consecutive failures" at WARN with `upstream`, `backend` and `for`. Nothing is logged when an ejection ends; the next request simply sees the backend as available again.
- The breaker and the outlier detector are independent and both see every reported outcome. Either one takes the backend out of `Available`.

### Tests for steps 3 and 4

- `resilience/breaker_test.go`, all on a fake clock:
  - the trip condition: below `min_requests`, exactly at the ratio, under it, neutral outcomes not counted, and a success never trips;
  - failures from an earlier window don't count;
  - the full recovery path, with no admission before `open_for`, the probe cap, closing only after every probe succeeds, the window reset, and the exact sequence of state changes;
  - a failed probe reopens it, and the other probe's late success is ignored;
  - a neutral probe frees its slot;
  - stale tickets from the closed period are ignored in half-open;
  - 8 goroutines under `-race` never get more than `half_open_probes` probes admitted.
- `resilience/outlier_test.go`: ejection after the streak (a success resets the streak, a neutral doesn't); expiry at the exact nanosecond; the expired state cleared; failures while ejected don't extend it; the sequence 30s, 60s, then capped at 75s, then one base of clean time forgives one step, then an hour forgives all; the percent cap for pools of 1, 2 and 10 at 50% and 25%, and 100%; the cap releasing once ejections end; the member size.
- `upstream/upstream_test.go` `TestPoolResilience`: no breaker or outlier unless configured; a breaker per backend, not shared; the breaker opening takes only that backend out and logs the change; ejection takes the backend out and logs it, and the pool-wide cap keeps the second backend in.
- `proxy/resilience_test.go`:
  - `TestProxyBreakerOpens`: after 3 backend 500s the breaker is open, and the 4th request gets 503 `no_backend` without reaching the backend.
  - `TestProxyOutlierEjects`: round robin over a 503 backend and a 200 backend. After 2 failures the bad one is ejected, and the next 10 requests all go to the good one.
  - `TestProxyClientFaultsAreNeutral`: a broken chunked body, a chunked body over `max_body`, and a client deadline all leave the breaker closed and the backend available, even with a breaker that opens on one failure and an outlier that ejects on one.
  - Mutation check: making `outcome` never neutral fails all three subtests.

### Step 5: active health checks (`internal/health`)

- `health.New(pool, Config, log)` builds a `Checker` for a pool. `Start` runs one goroutine per backend; `Stop` cancels them and waits, and is safe to call twice or without `Start`.
- **Timing.** The first check comes at a random offset in [0, interval), so the backends of a pool, and proxies started together, don't all check at once. After that, each wait is the interval ±10%. The wait is measured from the end of a check; validation already keeps `timeout` below `interval`.
- **The check.** GET `scheme://host` + `path` (a query is allowed) through the backend's own `Transport`, so it uses the backend's TLS settings and pooled connections. Host is the backend's, and User-Agent is `prism-health-check`. Any 2xx or 3xx passes. Up to 4 KiB of the body is read so the connection can be reused.
- **Thresholds.** A `tracker` flips the state only after `unhealthy_after` failed checks in a row, or `healthy_after` passed ones; any result that agrees with the current state resets the streak. A check that is cut short by `Stop` is ignored.
- **Logging.** "backend marked unhealthy" at WARN, with `checks` and the last `error`, and "backend marked healthy" at INFO. Each failed check is logged at DEBUG.
- **Wiring.** `runtime.Build` makes a checker for every upstream with `health`, and `Runtime.Start()` starts them. `main` calls it only after the listeners are up, and never under `-validate`. `Runtime.Close()` stops them before closing the pools. The e2e helper now calls `rt.Start()` too. `health` is out of `warnIgnored`.
- Health, the breaker and outlier detection are independent. Any one of them takes a backend out of `Available`.

### Step 6: bounded queue (`internal/limits`, wired into `proxy`)

- `NewSemaphore(n, queue)` now takes the queue size; 0 means nothing waits, which is the old behaviour.
- `Acquire(ctx, timeout)` takes a free slot without allocating. Otherwise it joins the queue, unless the queue is full (`ErrQueueFull`), and waits for a slot, the timeout (`ErrQueueTimeout`) or `ctx`. Joining is a CAS loop, not Add-then-undo, so `Waiting()` never reads above the cap; a `-race` test checks both caps. Go hands a freed slot to waiters in the order they started waiting.
- In the proxy, `Route.QueueTimeout` is passed to `Acquire`. A full queue or a timeout gets 503 `overloaded` with `Retry-After: 1`, as before. A client that leaves while queued goes through `fail`, so it is logged as 499 `client_closed` and nothing is written.
- `queue_size` is out of `warnIgnored`, and the example sets `queue_size: 100`.

### Step 7: retry budget and backoff (`resilience/budget.go`, `retry.go`)

- `NewBudget(ratio, minPerSec)` counts over a fixed 10 s `BudgetWindow` (Finagle's default), on the step 2 `Window`. `Request()` adds an unmarked event for each request's first attempt. `TryRetry()` allows a retry if `retries+1 <= ratio × requests + minPerSec × 10`, where requests = Total − Marked, and records it as a marked event. Retries earn no budget of their own. A mutex guards the window.
- `Retry` gains `Backoff{Base, Max}` and `Budget`, set by `runtime.Build` from the config. `Delay(n)` draws from [0, min(Max, Base×2ⁿ)) and can't overflow for any n. A zero `Backoff` means no wait, which keeps the older proxy tests instant.
- **Proxy.** `forward` calls `Budget.Request()` once per request. `retryTarget` asks `TryRetry()` after deciding a retry is allowed and the body can be replayed, and before picking the next backend; a refusal sets `Annotations.BudgetExhausted`, and the access log prints `retry_budget_exhausted=true`. The backoff runs after the failed backend is released and the next one is picked, so the route's concurrency slot is held while waiting. If the client leaves during the backoff, the next backend's admission is reported as neutral, and the request ends as 499 without being sent.
- A budget refused when the pool has no untried backend left still counts the retry. This overcounts slightly, and only when the pool has run out anyway.

### Step 8: graceful shutdown (`server`, `cmd/prism`)

- `server.New(listeners, readyPath, h, log)`. When `ready_path` is set, every listener answers it before routing and outside the access log (probes every second would flood it): 200 `ready` normally, 503 `draining` after `Drain()`, both with `Cache-Control: no-store`.
- `Server.Drain()` sets the ready path to 503 and calls `SetKeepAlivesEnabled(false)` on every `http.Server`, so HTTP/1 responses carry `Connection: close` and clients reconnect elsewhere while the proxy still serves them.
- **Hijack registry.** `TrackHijacked(conn) (untrack func())`. `Shutdown` waits for tracked connections after the HTTP servers are done, polling every 10 ms as `http.Server.Shutdown` does, and at the deadline closes the rest and forgets them, returning "closed N hijacked connections". Nothing registers yet; M6's WebSocket proxy will.
- **`main`.** It listens with `signal.Notify` on a channel with room for two signals. On the first, `shutdown()` calls `Drain()`, waits `drain_delay`, then calls `Server.Shutdown` with `timeout`. A second signal at any point cancels the context, which closes everything at once and returns `errInterrupted`. A shutdown that ends in an error makes the exit code 1. `ready_path` and `drain_delay` are out of `warnIgnored`; only `cache` is left.
- Checked by hand with the real binary: during a 2 s drain delay the ready path answers 503, the process exits 0 after the delay, and a second SIGTERM exits 1 within about a millisecond.

### Step 9: chaos tools and exit tests

- **`test/faultserver`** is a backend for chaos runs. Flags set the latency, an error rate and its status (503 by default), a drop rate (a 200 whose body is cut off halfway) and the body size. `GET /_fault?error_rate=1&latency=50ms` changes the faults while it runs and refuses unknown or malformed parameters. `GET /_stats` returns `{"name","requests"}`, where `/healthz` is not counted, so health checks don't show up as load.
- **`test/loadgen`** sends GETs at a fixed open-loop rate, so a slow proxy does not slow the sender. It writes one CSV row per second: `second,sent,ok,status_5xx,status_4xx,failed,error_pct,p50_ms,p99_ms,backend_reqs,amplification`. With `-stats`, it polls the faultservers' `/_stats` every 100 ms and adds each delta to the second it falls in; `backend_reqs` is the sum, and `amplification` is backend_reqs / sent. A request is counted in the second it was sent. A summary goes to stderr.
- **`test/chaosplot`** draws `load.csv` and `events.tsv` as an SVG with four panels on one time axis: requests per second (sent, ok, reached backends), client error rate, load amplification against a reference line at 1, and p50/p99 latency. Injected faults are dashed lines; Prism's reactions are numbered ticks, listed below the panels. It uses only the standard library, replacing the gnuplot step, which never ran because gnuplot isn't installed here.
- **`scripts/chaos/`**: `common.sh` builds everything into `bin/chaos/<scenario>/`, starts three faultservers (20 ms latency) and Prism on loopback ports, and runs 300 req/s for 40 s. It writes `load.csv`, `events.tsv` (the injected faults plus Prism's breaker, ejection and health log lines, in seconds from the start, via `jq`), `prism.log`, `summary.txt` and `graph.svg`. `RATE`, `DURATION` and `BACKEND_LATENCY` can be overridden.
  - `kill-backend.sh` sends SIGKILL to b3 at 10 s and restarts it at 25 s. Retries on `connect_error`, a breaker with a 1 s window, and health checks; no outlier block, because it would eject the backend within milliseconds and the breaker would never be seen. A run on 2026-10-20: 12,001 requests, **0 errors**, amplification 0.999 overall and 1.010 at worst. The breaker opened 0.4 s after the kill, health marked b3 down at 1.3 s, and after the restart health marked it up at 1.9 s, then half-open probes closed the breaker. Graph: `docs/m3-chaos/kill-backend.svg`.
  - `outage.sh` makes all three backends answer 503 from 10 s to 30 s, with 2 retries on 503, a budget of 0.2 and no breaker or health checks, so only the budget limits the load. A run on 2026-10-20: amplification 1.118 overall. Without the budget it would be 3.0 during the outage. `BUDGET_RATIO=1` shows a looser budget. Graph: `docs/m3-chaos/outage.svg`, which also shows the retry bursts every 10 s (see open items).
  - `make chaos` runs both.
- **`BreakerState.MarshalText`**: the first chaos run showed that JSON logs printed breaker states as numbers, because slog's JSON handler ignores `String()`. They are now names, and a test checks it.
- **`test/e2e/m3_test.go`** builds the real stack from YAML:
  - `TestBackendDiesUnderLoad`: 4 clients in a loop, and one of two backends is closed. No client sees an error, the log shows the dead backend's breaker opening, and the next 20 requests all reach the live one. It has no health checks, because they would mark the backend down before the breaker's window fills.
  - `TestHealthCheckRemovesBackend`: a backend whose `/healthz` fails but which still answers other paths gets no traffic once it is marked down.
  - `TestQueue`: with `max_concurrency: 1, queue_size: 1`, the second request waits and is served, and the third gets 503 with `Retry-After: 1` at once.
  - `TestRetryBudgetCapsLoad`: three backends answering 503 and 2 retries per request. 300 requests reach the backends at most 340 times (300 + 0.1 × 300 + 10), instead of 900, and the access log says the budget ran out.
  - `TestReadyPath`: the ready path answers, and a longer path is routed.
  - `startProxyLogged(t, body, w)` is `startProxyYAML` with the log also captured.

### Benchmarks

New rows, measured on 16 cores. All have 0 allocations.

| Benchmark | ns/op |
| --- | --- |
| Window Add, 1 core | 11 |
| Breaker Allow + Report while closed, parallel on one breaker | 84 (one mutex, in Report; 158 before Allow was made lock-free) |
| Outlier Ejected + Report success, parallel on one member | 0.3 |
| Budget Request, parallel on one route | 124 (one mutex) |
| Semaphore TryAcquire + Release, parallel | 44 (Acquire on a free slot is the same path, 0 allocs) |
| Whole proxy, GET | 54 µs, 147 allocs (unchanged from M2's 57 µs) |

The breaker figure is the worst case, with every core on one backend. Real traffic spreads over backends, and each has its own breaker.

### Wrap-up (2026-10-20)

These closed the open items that needed no decision.

- **Mid-body failures count.** `Breaker.Overturn(ticket)` turns a success already reported into a failure. While closed it adds only a failure to the window, since the request is already in Total, so one broken body is one failure, not a success plus a failure. While half-open it reopens the breaker; a stale ticket is ignored as usual. `Backend.Overturn(adm)` calls it and reports a failure to the outlier detector. `forward` calls it when copying the body fails, the attempt was a success, and the client is still there. Reporting the outcome only once the body ends was rejected: a half-open probe on a long stream, such as SSE, would hold its slot for the whole stream.
- **Client body errors are 400 `client_body`.** When the transport fails because reading the client's body failed (`body.clientFailed()`), `forward` answers through `clientBodyFailed`, which now takes the backend for the log line. Passing `max_body` still goes through `fail` and stays 413.
- **`FuzzBudget`** checks `Budget` against a plain list of requests and retries, and is in `make fuzz`.
- **`TestPickSkipsRefusingBackend`** covers the Admit refusal path in `pick` deterministically, with a balancer that ignores availability, as a balancer racing a breaker would see it.
- **Graphs.** `test/chaosplot` replaced gnuplot (see step 9). The graphs of the 2026-10-20 runs are committed in `docs/m3-chaos/`.
- **Loadgen polls every 100 ms**, so a killed backend loses at most 100 ms of its count, not a second: the kill run now reads 0.999 overall instead of 0.992.
- **The kill demo's missing in-flight errors are explained** by `net/http`'s `shouldRetryRequest`: a GET (replayable) on a reused connection that fails reading from the server is retried by the transport itself, and that retry fails to connect, which Prism retries on `connect_error`. A non-idempotent load would show errors at the kill.

## Next steps

Start M4 (caching) from `docs/project-plan.md`. `cache` is the only option left in `warnIgnored`.

## Open items carried forward

- [ ] **Retries come in bursts during a long outage.** The budget bounds retries over any 10 s, which the outage run confirms (1.118 overall). But once the budget is spent, a retry is allowed again only as an old one leaves the window, so they return as one burst every 10 s, about 2.9× for one second at 300 req/s; `docs/m3-chaos/outage.svg` shows it. Finagle's windowed budget does the same. A token bucket (deposit `ratio` per request plus `min_per_sec` per second, capped) would spread them out evenly, at the cost of the exact per-window bound. That would change D7, so it needs the user's call.
- [ ] **State across reloads (M5).** Breakers, outlier state and health live on `Backend` and `Pool`, so M5 can carry them over for backends whose identity hasn't changed. Today a reload resets them, and a new runtime's backends start healthy. M5 must also call `Start()` on the new runtime and `Close()` on the old one, which stops its health checks.
- [ ] **Readiness on the admin port (M5).** `ready_path` lives on the main listeners until the admin listener exists.
- [ ] **WebSocket (M6).** `cleanRequestHeaders` still strips `Upgrade`, and nothing registers with `TrackHijacked` yet.
- [ ] Still open from M2: the ip_hash spread and the rate-limit key cap.

## Conventions

- Claude writes code directly into the files; the user doesn't type it from chat. Plan each step in chat, implement one step at a time, and run `make lint test` after each.
- Edit in place and never overwrite the user's own work. Match the existing style: short doc comments that explain why, table-driven tests, and fuzz tests against a simple reference.
- Time-based logic takes an injectable `now` (a field set by tests in the same package), and tests run on a fake clock.
- Every hot-path function gets a benchmark with `ReportAllocs`.
- Add each new fuzz target to the Makefile's `fuzz` target.
- Libraries are allowed only for YAML, Prometheus and OpenTelemetry.
- Commit only when the user asks.
