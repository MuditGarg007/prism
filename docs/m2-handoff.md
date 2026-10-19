# M2 handoff

As of 2026-10-19, M2 (traffic management) is complete. All 9 steps are done, and `make lint test validate` passes with `-race`. Next is M3 (resilience).

- The repo lives at `/home/mudit/prism`. The module is `github.com/MuditGarg007/prism`, on Go 1.27.
- The last commit is `2b97de5` (M1). All M2 work is uncommitted until the user asks for a commit.
- Run `make lint test validate` to check the current state, and `make fuzz` to run the five 30 s fuzz targets.
- Every `go` command prints `go: warning: ignoring go.mod in $GOPATH /home/mudit/go`. This comes from the local environment and is harmless.
- The source plan is `docs/project-plan.md`, section "M2: Traffic management, Layer 2". M1's state and decisions are in `docs/m1-handoff.md`.

## Decisions

These were accepted at the start of M2. Don't reopen them unless asked. All seven are implemented as written, except one deviation in D7, noted under step 7: a body of unknown length is never read ahead, so a chunked body is retried only on a connect error.

| # | Decision | Choice | Why |
| --- | --- | --- | --- |
| D1 | Balancer shape | A balancer is built with its backends (`upstream.NewBalancer func([]*Backend) Balancer`) and picks with `Pick(upstream.Choice)`. `Choice` carries `ClientIP` and `Tried`. | Weighted RR needs per-backend state and ip_hash needs a prebuilt ring. `Tried` is how retries reach a different backend. |
| D2 | `weighted` | nginx's smooth weighted round robin, under a mutex | Weights 5,1,1 give `aabacaa`, not a burst of five. |
| D3 | `least_conn` | Power of two choices on `(inflight+1)/weight` | O(1), and it avoids every concurrent pick herding onto the same idle backend. Envoy's least_request works the same way. |
| D4 | `ip_hash` | Ketama-style hash ring with virtual nodes, keyed on the full client IP | Adding a backend remaps about 1/N of clients. Hashes are unseeded, so all instances agree. |
| D5 | Rate limit | GCRA per route, keyed by `ClientIP`, in 64 sharded maps with lazy idle-key eviction. Rejections get 429 + `Retry-After`. | One integer per key, an exact token bucket, bounded memory, and no goroutine to manage across reloads. |
| D6 | Concurrency and body limits | A buffered-channel semaphore per route that fails immediately with 503 + `Retry-After: 1`. `max_body` gives 413, up front from `Content-Length` and mid-stream from `http.MaxBytesReader`. | M3 adds a bounded queue on top of the channel with `Acquire(ctx)`. |
| D7 | Retries | nginx style. Only to an **untried** backend; stop when none is left. `connect_error` for any method, but only when zero request-body bytes were sent. `timeout` and 5xx only for idempotent methods. Bodies are prefetched up to 64 KiB for replay. No backoff and no budget until M3. | Retries never duplicate a non-idempotent request. Amplification is bounded by `min(attempts, pool size − 1)`. |

The request pipeline in `proxy.Handler`, matching the project plan:

1. Client IP
2. Path validation (400)
3. Route match (404)
4. Rate limit (429) — **done**
5. Body size check (413) — **done**
6. Concurrency limit (503) — **done**
7. Balancer pick and forward, with retries — **done**

## What is built

### Step 1: balancer refactor (`internal/upstream`, `internal/balancer`)

- `upstream.Balancer` is now `Pick(c Choice) (*Backend, error)`.
- `upstream.Choice`:
  - `ClientIP netip.Addr` is the address worked out from the trusted proxies, never the raw X-Forwarded-For. It is invalid for non-IP peers. This closes the M1 open item "Client IP use".
  - `Tried []*Backend` holds the backends this request already failed on.
  - `Choice.Usable(b)` means available and not tried. Every balancer skips whatever `Usable` rejects.
- `upstream.NewPool(name, u, newBal NewBalancer)` builds the backends first, then the balancer over them.
- `balancer.New(kind)` returns a constructor, so the call sites in `runtime.Build` and the proxy test helper did not change.
- The proxy calls `rt.Pool.Pick(upstream.Choice{ClientIP: an.ClientIP})`.
- The shared `weight(b)` helper in `balancer.go` treats a weight of 0 as 1, because a zero-value Backend is usable. Config already requires weight ≥ 1.

### Step 2: smooth weighted round robin (`balancer/weighted.go`)

- Each pick adds every usable backend's weight to its current weight, picks the highest, and subtracts the total from the winner. A mutex guards `cur []int`.
- Only usable backends take part, so the share of a backend that is down or already tried spreads evenly over the rest.
- Ties go to the lowest index, as in nginx.
- Tests:
  - exact sequences, including nginx's `aabacaa` example
  - sequences with a backend down
  - exact counts over 50 cycles for six weight sets, with every cycle identical
  - tried backends skipped
  - ErrNoBackend
  - exact totals from 8 goroutines under `-race`
  - zero allocations

### Step 3: least connections, power of two choices (`balancer/leastconn.go`)

- **Fast path.** Draw two distinct random indices. If both backends are usable, return the lighter one. There is no lock and no scan.
- **Slow path.** If either is unusable, reservoir-sample two of the usable backends in a single pass. Because health is read only once, a health change mid-scan cannot leave a stale count. Every usable pair stays equally likely overall.
- **Comparison.** `(inflight+1)/weight`, by cross-multiplying. The `+1` counts the request being placed, so between idle backends the heavier one wins. Ties are broken at random.
- Randomness comes from `math/rand/v2`'s lock-free top-level functions. The `intN` field is swapped in tests for a seeded PCG, so every statistical test is deterministic.
- Tests:
  - a table of comparison cases
  - an even spread with 0, 1 and 2 backends down
  - the most loaded backend is never picked
  - a discrete-event load simulation: a backend with 4× the latency gets about 20% of picks (round robin would give 50%), and weights 3:1 give 75/25
  - edge cases
  - a `-race` test with real Acquire/Release
  - zero allocations

### Step 4: consistent hashing (`balancer/iphash.go`)

- **Ring.** 160 points per unit of weight. The total is capped at 65,536 points, with weights scaled down to fit and their ratio kept.
- **Point keys.** `URL.String() + "-" + v`.
- **Client keys.** All 16 bytes of `ClientIP.As16()`. nginx's ip_hash uses only the IPv4 /24; using the full address spreads clients better.
- **Hash.** FNV-1a 64 followed by the splitmix64 finalizer. FNV alone spreads short, similar keys poorly. Nothing is seeded, so every instance with the same backends agrees.
- **Pick.** Binary search for the client's hash, then walk clockwise to the first usable backend. The walk goes at most one lap and skips consecutive points of the same backend. A backend that is down hands only its own clients to the next owners on the ring.
- A zero `ClientIP` (non-IP peer) hashes as all zeros, so all such clients share one backend.
- `balancer.Implemented` and the runtime's "balancer not implemented" warning are deleted.
- Tests:
  - Stickiness, including agreement with a separately built instance.
  - Spread over 100k seeded IPs. Equal and weighted pools are both covered. With 160 vnodes, 10 equal backends deviate up to about ±17%, so the tolerances are 15–20%. The tests are deterministic, not flaky.
  - **Adding a backend** (2→3, 4→5, 9→10) moves about 1/N of clients, and every one that moves goes to the new backend.
  - **Ejecting a backend** moves only its own clients, spreads them over all the others, and they return on recovery.
  - Retries with `Tried` are sticky.
  - No-IP clients, ErrNoBackend, ring size and scaling, and zero allocations.

### Step 5: rate limiter (`internal/ratelimit`, wired into `proxy`)

- **`ratelimit.New(count, per, burst)` and `Allow(key netip.Addr) (ok, retryAfter)`.**
  - **GCRA.** `T = per/count` (at least 1 ns) and `limit = burst×T` (capped so it cannot overflow). A request is allowed when `max(tat, now) + T − now ≤ limit`, and the key's TAT then moves to that value.
  - A rejected request does not count. The returned wait is exact: 1 ns less is still rejected.
  - The clock is monotonic nanoseconds since `New`, and `now` can be swapped in tests.
- **Store.**
  - 64 shards, each holding a mutex, a `map[[16]byte]int64` and an insert counter.
  - The shards are padded to 64 bytes.
  - `[16]byte` keys mean the maps hold no pointers, so the GC does not scan them.
  - The shard is chosen by a splitmix mix with a per-process random seed, so clients cannot target one shard.
- **Memory bound.**
  - A key whose TAT has passed is the same as one never seen.
  - Every 1024 new keys, a shard sweeps out its idle keys. When fewer than half the keys are live, it rebuilds the map instead, because Go maps never shrink.
  - Each shard holds at most 4096 keys (about 262k keys in total). A full shard drops an arbitrary key between sweeps, which only gives that client a fresh burst. Sweeping on every insert into a full shard would cost a full scan per new client.
- **Wiring.**
  - `proxy.Route.RateLimit` (`nil` for none) is checked right after the route match.
  - The key is `an.ClientIP`, so an untrusted client cannot get a fresh bucket by changing X-Forwarded-For. A test checks this.
  - `reject(w, an, status, kind, retryAfter)` in `errors.go` sets `Retry-After` in whole seconds, rounded up, and `an.Error`. It writes no log line of its own, because a WARN per rejected request would turn a flood into a log flood. The access log still records each one.
  - The new error kind is `rate_limited`.
  - `runtime.Build` creates one limiter per route, and `rate_limit` is out of `warnIgnored`.
- **Tests.**
  - Table-driven sequences: burst then steady rate, idle refill capped at the burst, a burst of 1, and per-hour rates.
  - Exact Retry-After.
  - A steady rate, and twice the rate (exactly 50% allowed).
  - Independent keys across IPv4, IPv6 and no IP.
  - 16 goroutines get exactly the burst.
  - A 500k unique-client flood stays under the cap.
  - Idle keys are swept.
  - Even shard spread.
  - Zero allocations.
  - `FuzzGCRA` checks every decision and wait against an integer token bucket. It is in `make fuzz` and ran 8.7M executions clean.
  - The proxy tests in `proxy/limits_test.go` check the 429, `Retry-After: 60`, the annotations, that rejected requests never reach the backend, and per-client keys behind a trusted proxy. That file also has the `annotatedRecorder` and `countingBackend` helpers for step 6.

### Step 6: concurrency and body limits (`internal/limits`, wired into `proxy`)

- **`limits.NewSemaphore(n)`** is a buffered `chan struct{}` of size `n`.
  - `TryAcquire` is a `select` with `default`, so it never waits. `Release` panics if nothing was acquired, rather than blocking forever.
  - `InUse` and `Cap` are there for tests now and metrics later.
- **`proxy.Route.MaxBody`** (`int64`, 0 for none) and **`proxy.Route.Concurrency`** (`*limits.Semaphore`, `nil` for no cap). `runtime.Build` fills both, and `max_body` and `max_concurrency` are out of `warnIgnored`.
- **Pipeline.** Rate limit, then body size, then concurrency, so a request refused early never takes a slot. A test checks the order.
- **Body size.**
  - A `Content-Length` over the limit gets 413 before anything is read, so a client that sent `Expect: 100-continue` never gets the go-ahead. A raw-socket test checks this.
  - Only a body of unknown length is wrapped in `http.MaxBytesReader`, because net/http already stops a declared-length body at its length. This saves an allocation on most requests.
  - `classify` maps `*http.MaxBytesError` to 413 `body_too_large`. The transport does hand back the body's read error as its own: 100 runs of the mid-stream test under `-race` never gave a 502.
  - A mid-stream 413 is logged at DEBUG, not WARN. It is the client's fault and as easy to flood with as a rate limit.
  - Every 413 on HTTP/1 sets `Connection: close` (`closeAfter`), because the rest of the body is still unread on the connection. `MaxBytesReader` would do this itself, but it cannot see through the access log's writer. HTTP/2 is left alone: the stream is reset, and `Connection: close` there would shut down every other stream on the connection.
  - A 413 has no `Retry-After`, because waiting will not help.
- **Concurrency.** A full semaphore gets 503 `overloaded` with `Retry-After: 1`. The slot is released by a `defer` around `forward`, so it is held until the whole response body is copied, and it comes back on a failed request and on an abort's panic too.
- **Tests** (`proxy/limits_test.go`):
  - Body sizes at and over the limit, by `Content-Length` and chunked, including 1 MiB chunked against a 1000-byte limit. A declared oversize never reaches the backend.
  - Annotations and no WARN for both kinds of 413.
  - No `100 Continue` for an oversize upload.
  - At the cap: a third request gets 503 while two responses are still streaming their bodies, and the slots come back once they finish.
  - The slot is released after a connect error and after a truncated body.
  - `limits`: the cap, panics, a 16-goroutine `-race` test that the cap is never exceeded, and zero allocations.
- A 413 that closes the connection costs about 500 ms in test cleanup. That is net/http's `rstAvoidanceDelay` before it closes, not a bug.

### Step 7: retries (`internal/resilience`, wired into `proxy`)

- **`resilience.NewRetry(attempts, on)`** builds the policy. The 5xx codes are a two-word bitset.
  - `Retryable(Attempt{Connect, Timeout, Status}, idempotent)`: a connect failure retries for any method. A timeout or a listed 5xx retries only for an idempotent method.
  - A connect *timeout* sets both `Connect` and `Timeout`, so `connect_error` covers it for any method, as Envoy's `connect-failure` does. The backend never saw the request.
  - Any other error, such as a backend hanging up after the connection was made, is not retried. Go's transport still retries a stale pooled connection by itself when nothing was written.
  - `Idempotent(method)`: GET, HEAD, OPTIONS, TRACE, PUT and DELETE.
- **Bodies** (`proxy/body.go`, `requestBody`). Only routes with `retry` change; other routes pass the body through as in M1.
  - No body: every attempt gets `http.NoBody`.
  - A declared `Content-Length` of at most 64 KiB (`replayLimit`) is read up front, and every attempt gets its own reader over it. `GetBody` is set too, so the transport can rewind it for its own stale-connection retry.
  - **Deviation from D7:** a body of unknown length is never read ahead, even a short one. A streaming client, such as a gRPC client stream, may wait for a response before sending more, and reading ahead would deadlock it. So chunked bodies, like bodies over 64 KiB, are streamed once through `readCounter`, and a retry is allowed only while the transport has read none of it. In practice only connect failures are retried for them. The cost is that a chunked PUT or DELETE is not retried on a timeout or 5xx. POST never was.
  - `readCounter.Close` does nothing, because the transport closes the body of a failed attempt. net/http closes the client's body when the handler returns. The count is atomic because the transport reads on its own goroutine.
  - The outgoing `ContentLength` stays as the client sent it, so a chunked body stays chunked and request trailers still work.
  - A client body cut short while it is read up front gets 400 `client_body` (logged at DEBUG, connection closed), or 499 if the client left. Without that it would have been a 502 blamed on the backend.
- **Loop** (`proxy.forward` and `retryTarget`):
  - Pick with `Choice{ClientIP}`. After a failed attempt, the policy, the attempt cap (`an.Attempts > retry.attempts` stops it), a canceled client context and `body.replayable()` all have to allow a retry. Then the backend is appended to `Tried`, and the next one is picked.
  - If `Pick` returns `ErrNoBackend`, the loop stops and the last result goes to the client. A 5xx is only closed once the next backend is in hand, so the last attempt's 5xx reaches the client unchanged.
  - `Tried` is allocated only when a retry happens. Escape analysis confirms `requestBody` and `Choice` stay on the stack.
  - Each attempt does its own `Acquire`/`Release`, and the last backend's is deferred, so `least_conn` counts stay right through an abort.
  - A failed attempt that is retried is logged at WARN as "upstream attempt failed, retrying" with its kind. A retried 5xx is not logged; the access log shows the attempts.
- `retry` with `attempts: 0` builds no policy, so the body is not read up front.
- **`retry.budget_ratio` is accepted but does nothing until M3.** It is not in `warnIgnored`, because the default fills it in for every route with `retry`.
- `Annotations.Attempts` counts every backend tried, the first included. The access log writes `attempts=` whenever a backend was picked.
- `warnIgnored` is now down to `health`, `circuit_breaker` and `cache`, which is step 8's target.
- **Tests** (`proxy/retry_test.go`):
  - Connect errors are retried for GET and POST with every body kind: none, small, exactly 64 KiB, 100 KB, and chunked. The failed attempt is logged.
  - A 503 is retried for GET, DELETE and PUT with a replayable body. It is not retried for POST or PATCH, or for a PUT whose streamed body the first backend read.
  - A 5xx not in `on` is not retried.
  - The attempt cap, and running out of backends, both return the last backend's 503 unchanged. No backend is tried twice and every inflight count is back to 0.
  - A header timeout is retried for GET and gives 504 for POST.
  - No retry after the client cancels.
  - A short client body gets 400 `client_body`.
  - A replayed PUT through real servers.
  - `resilience`: the table of conditions, every status 400–699 against the bitset, rejected conditions, `Idempotent`, and zero allocations.
  - Mutation check: dropping the `replayable` check, or treating every method as idempotent, fails 2 and 4 subtests respectively.

### Benchmarks so far

All but the whole-proxy rows have 0 allocations. The parallel figures were measured on 16 cores.

| Benchmark | ns/op |
| --- | --- |
| Round robin, parallel | 30 |
| Smooth weighted, parallel | 80 (mutex contention, acceptable) |
| least_conn P2C, parallel | 3–7 |
| ip_hash, parallel | 7 |
| Rate limit, one hot key: 1 core | 48 |
| Rate limit, one hot key: 16 cores | 94 (serialized on one shard) |
| Rate limit, 64k keys: 1 core | 65 |
| Rate limit, 64k keys: 16 cores | 23 |
| Semaphore TryAcquire + Release: 1 core | 26 |
| Semaphore TryAcquire + Release: 16 cores | 40 (one channel lock) |
| Whole proxy, GET (loopback, net/http dominated) | 57 µs, 147 allocs |
| Whole proxy, 4 KiB PUT on a retry route (body read up front) | 67 µs, 153 allocs |

### Step 8: runtime and config cleanup

- `warnIgnored` is down to `health` and `circuit_breaker` (M3) and `cache` (M4). `make validate` prints exactly those three warnings for the example.
- `examples/prism.yaml` comments now say what each M2 option does (429, 413, 503 + `Retry-After: 1`, which retries are allowed) and which options are still ignored and until when. `budget_ratio` is marked as having no effect until M3.
- No new fuzz targets were needed beyond `FuzzGCRA`, which is already in `make fuzz`. The Makefile runs all five.

### Step 9: e2e tests (`test/e2e/m2_test.go`)

- `startProxyYAML(t, body)` builds the real stack from any config body (everything but the listeners). `startProxy` now calls it, so the M1 tests did not change.
- Helpers: `send` makes one request with a 5 s deadline and an optional X-Forwarded-For, and `deadURL` returns a loopback port with nothing listening.
- Tests:
  - `TestRateLimit`: `1/m` with a burst of 2 gives 200, 200, then 429 with `Retry-After: 60`, and the backend sees only 2 requests. Another client behind the trusted proxy still gets 200.
  - `TestMaxBody`: with `max_body: 1KB`, 1024 bytes pass and 1025 get 413, both declared and chunked. A declared oversize never reaches the backend.
  - `TestConcurrencyCap`: with `max_concurrency: 1` and one request held at the backend, the next gets 503 with `Retry-After: 1`. Once the first finishes, the slot comes back.
  - `TestRetrySkipsDeadBackend`: round robin over a refused port and a live backend, with `on: [connect_error]`. GET, POST and chunked POST all get 200, and the live backend echoes each body intact. Round robin's counter also advances on the retry pick, so every request tries the dead backend first. The log shows 12 retries for 12 requests.
  - `TestRetryIdempotentOnly`: two backends that both return 503, with `on: [503]`. A POST reaches one backend and a GET reaches both, and the client gets the backend's own 503 body either way.
  - `TestIPHashSticky`: `ip_hash` over 3 backends behind a trusted proxy. Each of 30 client IPs gets the same backend on all 5 of its requests, and more than one backend is used.
- 20 runs under `-race` passed with no flakes. The only slow case is the declared 413, which takes about 500 ms (`rstAvoidanceDelay`, see step 6).

## Open items carried forward

- [x] **Client IP use (from M1).** The rate limiter and ip_hash both key on `Choice.ClientIP` / `an.ClientIP`, never on the raw header.
- [ ] **WebSocket (M6).** `cleanRequestHeaders` still strips `Upgrade`.
- [ ] **State across reloads (M5).** Limiter buckets, smooth WRR positions and P2C counters are rebuilt on every `runtime.Build`, so a reload resets them. M5 should carry over limiters for unchanged routes and balancer state for unchanged pools.
- [ ] **ip_hash spread.** 160 vnodes gives about ±17% spread across 10 backends. Raise `pointsPerWeight` if that matters, at the cost of ring memory and cache misses.
- [ ] **Rate limit key cap.** `shardCap` (4096 per shard) is a constant. Make it configurable if a deployment needs more than about 262k concurrently limited clients per route.

## Exit criteria

M2 is done when these hold:

1. Table-driven distribution tests for every balancer. **Done** in steps 2–4, plus the round-robin tests from M1.
2. A rate-limiter benchmark under contention. **Done** in step 5 (`BenchmarkAllow`).
3. The step 9 e2e tests pass under `go test -race`. **Done.**

All three hold, so M2 is complete.

## Conventions

- Claude writes code directly into the files; the user doesn't type it from chat. Plan each step in chat, implement one step at a time, and run `make lint test` after each.
- Edit in place and never overwrite the user's own work. Match the existing style: short doc comments that explain why, table-driven tests, and fuzz tests against a simple reference.
- Statistical tests use seeded randomness, so they are deterministic.
- Every hot-path function gets a benchmark with `ReportAllocs`. Where it should be allocation-free, add a `testing.AllocsPerRun` check.
- Add each new fuzz target to the Makefile's `fuzz` target.
- Libraries are allowed only for YAML, Prometheus and OpenTelemetry. Write everything else by hand, and don't depend on `httputil.ReverseProxy`.
- Commit only when the user asks.
