# Prism: Project Plan

Prism is an nginx-style HTTP reverse proxy written in Go.

## The core build decision

Go's standard library already parses HTTP, so there are three options:

1. **Wrap `httputil.ReverseProxy`.** This is fast to build, but the library does most of the interesting work, so a reviewer won't learn much about your skills from it.
2. **Write your own HTTP/1.1 parser on raw TCP.** This teaches a lot, but parsing would take months before you reach any of the features that make the project stand out.
3. **Recommended: use `net/http` for the server and `http.Transport` for the client, and write the forwarding layer yourself.** That means owning hop-by-hop header handling, forwarded headers, streaming, flushing, cancellation, error mapping, retries and WebSocket upgrades.

Read the `httputil.ReverseProxy` source as a reference, but don't depend on it. The load balancers, rate limiter, circuit breaker, LRU cache and request coalescing should also be written by hand, because those are what you'll talk about in interviews. Use libraries only for YAML, Prometheus and OpenTelemetry (and quic-go if HTTP/3 is ever added).

## Repository layout

```
prism/
├── cmd/prism/main.go          # flags, signals, wiring
├── internal/
│   ├── config/                # schema, load, validate, defaults
│   ├── runtime/               # immutable snapshot built from config; atomic swap
│   ├── server/                # listeners, TLS/SNI, graceful shutdown
│   ├── router/                # host + path matching table
│   ├── proxy/                 # forwarding, header rewrite, streaming, error mapping
│   ├── upstream/              # Backend, Pool, per-upstream http.Transport
│   ├── balancer/              # rr, weighted, leastconn, iphash
│   ├── ratelimit/             # token bucket / GCRA, sharded keyed store
│   ├── limits/                # concurrency semaphore, bounded queue, body size
│   ├── resilience/            # retry policy, budget, backoff, breaker, outlier
│   ├── health/                # active probes, passive tracking
│   ├── cache/                 # store, LRU, freshness, revalidation, coalescing
│   ├── observability/         # slog access log, prometheus, otel
│   ├── security/              # basic auth, api keys, ip allow/deny
│   └── admin/                 # admin HTTP API (separate listener)
├── test/
│   ├── e2e/                   # full proxy tests against fake backends
│   ├── faultserver/           # backend that can be slow, flaky, drop mid-body
│   ├── loadgen/               # open-loop load, per-second CSV with amplification
│   └── chaosplot/             # draws a chaos run as an SVG
├── scripts/chaos/             # chaos scenarios (make chaos)
├── bench/                     # load scripts, nginx comparison configs
└── examples/prism.yaml
```

## Request pipeline

Most of the design work is choosing the order of these steps:

```
accept → TLS (SNI cert pick)
  → access log + trace span start + recover
  → IP allow/deny
  → route match (host, then longest path prefix)
  → auth (basic / API key)
  → rate limit (per-IP, per-route)          → 429 + Retry-After
  → request size check                       → 413
  → cache lookup (+ coalescing)              → HIT returns here
  → concurrency limit / bounded queue        → 503 + Retry-After
  → traffic split (canary) / shadow copy
  → balancer pick (skips unhealthy / ejected / breaker-open)
  → forward with deadline, retries, budget
  → cache store (tee body)
  → metrics + log finish
```

The cache lookup comes before the concurrency limit, so hits and requests
waiting on a coalesced fetch never hold a slot. Only the request that goes to
a backend, a miss or a revalidation, takes one. Otherwise cheap hits, and
hundreds of waiters on one cold key, would starve real work.

## Key abstractions

```go
// Immutable per-config snapshot; swapped atomically on reload.
type Runtime struct {
    Router    *router.Table
    Upstreams map[string]*upstream.Pool
}
var current atomic.Pointer[Runtime]

type Backend struct {
    URL      *url.URL
    Weight   int
    inflight atomic.Int64   // for least-conn
    healthy  atomic.Bool    // set by health checker / outlier detector
    breaker  *resilience.Breaker
}

type Balancer interface {
    // Pick must skip unavailable backends; returns ErrNoBackend if none.
    Pick(r *http.Request, bs []*Backend) (*Backend, error)
}
```

Each request loads the `Runtime` snapshot once at the start and keeps it until it finishes. Reloads then can't break in-flight requests, and the hot path needs no lock.

## Example config

```yaml
listeners:
  - addr: ":443"
    tls: { certs: [{ cert: certs/a.pem, key: certs/a.key }] }

upstreams:
  api:
    balancer: least_conn          # round_robin | weighted | least_conn | ip_hash
    backends:
      - { url: "http://10.0.0.1:8080", weight: 3 }
      - { url: "https://10.0.0.2:8443", weight: 1, tls: { server_name: api.internal } }
    health: { path: /healthz, interval: 5s, timeout: 1s, unhealthy_after: 3, healthy_after: 2 }
    circuit_breaker: { failure_ratio: 0.5, min_requests: 20, open_for: 30s }
    timeouts: { connect: 2s, response_header: 10s }

routes:
  - match: { host: "api.example.com", path_prefix: "/v1/" }
    upstream: api
    strip_prefix: true
    headers: { request_set: { X-Env: prod }, response_remove: [Server] }
    rate_limit: { per_ip: "100/s", burst: 50 }
    max_body: 10MB
    max_concurrency: 500
    retry: { attempts: 2, on: [connect_error, 502, 503], budget_ratio: 0.2 }
    cache: { enabled: true, max_object: 1MB }

cache: { max_size: 64MB }      # one store shared by every route
admin: { listen: "127.0.0.1:9901" }
```

## Milestones

Each milestone ends with an exit criterion. Don't start the next milestone until the current one passes `go test -race`.

### M0: Skeleton (2–3 days)

- `go mod init`, the layout above, config loading and validation with clear errors, and `slog` logging.
- A Makefile with test, lint and bench targets.
- GitHub Actions running `go vet`, staticcheck and `-race`.

**Exit:** `prism -config examples/prism.yaml -validate` works.

### M1: Core proxy, Layer 1 (1.5–2 weeks)

Get this right before building anything else.

- Forward with `http.NewRequestWithContext(r.Context(), ...)` so a client disconnect cancels the upstream request.
- Strip hop-by-hop headers: `Connection`, `Keep-Alive`, `Proxy-Connection`, `TE`, `Trailer`, `Transfer-Encoding`, `Upgrade`, plus every header named inside `Connection`.
- Forwarded headers: add a `trusted_proxies` CIDR list. Only keep incoming `X-Forwarded-For` values when the direct peer is trusted; otherwise replace them. Set `X-Forwarded-Proto` and `X-Forwarded-Host`, and optionally RFC 7239 `Forwarded`. Getting this wrong lets clients spoof their IP, so test it.
- Streaming: copy bodies with a pooled buffer. Flush per write for `text/event-stream` and unknown-length responses (use `http.ResponseController`). Never buffer whole bodies.
- Use one tuned `http.Transport` per upstream (`MaxIdleConnsPerHost`, `IdleConnTimeout`, `ResponseHeaderTimeout`, `TLSClientConfig`).
- Map errors: dial failure → 502, timeout → 504, client gone → log as 499 and write nothing.
- Host routing (exact plus `*.example.com` wildcard) and longest-prefix path matching, compiled into a table.
- TLS termination with `GetCertificate` for SNI, plus HTTPS upstreams with a custom CA and ServerName.

**Exit:** e2e tests cover streaming a 1 GB body with flat memory, SSE passthrough, cancellation that reaches the backend, spoofed XFF being rejected, and keep-alive reuse (count upstream connections).

### M2: Traffic management, Layer 2 (1.5 weeks)

- Round-robin uses an atomic counter. Weighted uses nginx's **smooth weighted round-robin**, which avoids bursts to the heaviest backend.
- Least-connections uses the `inflight` counter. Mention **power-of-two-choices** as an optimization.
- For IP hash, use **consistent hashing**: either a ring with virtual nodes or rendezvous hashing. Adding a backend should then remap only about 1/N of clients, and a test should show it.
- Rate limiting: a token bucket or GCRA per key in a sharded map (`[64]struct{mu; map}`) with idle-key eviction, so memory stays bounded under a flood of unique IPs.
- Concurrency limits use a semaphore. Request size limits check `Content-Length` up front and wrap the body in `http.MaxBytesReader`.
- Retries: only when the request is idempotent or failed before any bytes were sent, and only if the body is replayable (buffered under a small cap, or `GetBody`). Retry on a *different* backend.

**Exit:** table-driven tests for the distribution of every balancer, and a benchmark of the rate limiter under contention.

### M3: Resilience, Layer 3 (2 weeks)

Build these together.

- Active health checks run one goroutine per backend with jitter, with healthy/unhealthy thresholds to prevent flapping.
- Passive checks and outlier detection: count consecutive 5xx and connect errors per backend, then eject the backend for `base × n` time. Cap ejection at a maximum percentage of the pool so every backend is never ejected at once.
- Circuit breaker: closed → open → half-open state machine over a sliding window, with limited probes while half-open.
- Bounded queue: when the concurrency limit is hit, wait in a bounded queue with a timeout, then shed load with 503 + `Retry-After`. This provides backpressure.
- Retry budget: retries may not exceed X% of recent requests plus a small per-second floor. Use exponential backoff with full jitter.
- Graceful shutdown: on SIGTERM, flip readiness, stop accepting, call `Server.Shutdown`, drain within a deadline, and track hijacked connections (WebSockets) separately.

**Exit:** chaos demo scripts using `faultserver`, such as killing one backend mid-load. Show that the error rate recovers, the breaker opens, and retries don't amplify load (graph it).

### M4: Caching, Layer 4 (2 weeks)

- Cache only GET/HEAD and 200/301/404 responses. Skip requests with `Authorization` unless the response is `public` or has `s-maxage`. Skip responses with `Set-Cookie`, `no-store` or `private`.
- The key is method + scheme + host + path + query, plus the values of the headers named in `Vary` (`Vary: *` means don't cache).
- Freshness comes from `s-maxage` > `max-age` > `Expires`. Heuristic freshness stays off, and that is documented.
- Revalidate with `If-None-Match`/`If-Modified-Since`, and merge headers from a 304 into the stored entry.
- A byte-bounded LRU (a map plus a doubly linked list, written by hand). Tee the body into the cache while streaming it to the client, and abandon caching if it exceeds `max_object`.
- A hand-written singleflight for coalescing: one upstream fetch, N waiters.
- Purge goes through the admin API, so the admin listener (`admin: { listen }`, loopback by default) arrives here with `POST /cache/purge` and `GET /cache/stats`; M5 adds the rest of its endpoints. Add a bypass header, metrics for hit/miss/stale/revalidated, and an `X-Cache` response header.

**Exit:** a coalescing test where 1,000 concurrent requests for a cold key produce exactly 1 upstream hit.

**Status:** done on 2026-10-20. The decisions and details are in `docs/m4-handoff.md`. As built:

- Every cached response carries `X-Cache`: `HIT`, `MISS`, `STALE`, `REVALIDATED` or `BYPASS`. The access log has the same value as `cache=`, and `GET /cache/stats` on the admin listener counts each one, along with the store's entries, bytes and evictions and the requests waiting on a fetch.
- Coalescing follows nginx's `proxy_cache_lock`: one request fetches a cold or stale key and the rest wait up to `lock_timeout`, then fetch for themselves without storing. A response that can't be stored releases the waiters at once and marks the key **hit-for-pass** for 10 s, as Varnish does, so requests for an uncacheable URL never queue behind each other.
- **Stale-if-error:** when the backend fails (a connect error, a timeout or any 5xx), an entry up to the route's `stale_if_error` past its freshness is served as `STALE`. A response's own `stale-if-error` can narrow that window but never widen it. `must-revalidate`, `no-cache` and `s-maxage` forbid it. Requests waiting on a failed fetch serve the stale entry too, rather than each trying the backend. `stale-while-revalidate` is parsed but not acted on yet.
- `POST /cache/purge` with `{"host", "path", "prefix"}` deletes every variant, method and scheme of a URL, or of every URL under a path prefix. The host is matched without case or default port, and the path must start with `/`.
- The admin listener has no authentication. Prism warns at startup when `admin.listen` is not a loopback address.

### M5: Operations, first part of Layer 5 (1.5 weeks)

- Prometheus metrics: request counters per route, upstream and status; latency histograms per route and upstream; gauges for inflight, queue depth, breaker state and healthy backends.
- Structured JSON access logs with a request ID.
- An admin API on a separate port: `GET /routes`, `/upstreams` (live health, inflight, breaker state), `/config`, and `POST /cache/purge`.
- Hot reload on SIGHUP: parse → validate → build a new `Runtime` → atomic swap. Keep backend state (health, breakers, connection pools) for backends whose identity hasn't changed. Old requests finish on the old snapshot.

**Exit:** a reload under load test with zero failed requests (run the load generator while reloading in a loop).

**Status:** done on 2026-10-20. The decisions and details are in `docs/m5-handoff.md`. As built:

- Metrics come from `prometheus/client_golang` on the admin listener's `GET /metrics`: requests by route, upstream and status class, request and per-attempt backend latency histograms, retries, budget refusals and proxy errors by kind, plus scrape-time gauges for backend inflight, health, ejection and breaker state, available backends, route concurrency and queue depth, the cache, and reload results. Handles are resolved when a config is built, so recording a request takes no label lookup and no allocation.
- Every request carries `X-Request-Id`, kept from a trusted peer when well formed and generated otherwise. It goes to the backend and the client, and comes first in the access log.
- The admin API adds `GET /routes`, `GET /upstreams`, `GET /config` (the effective config, with a redaction hook for M6 secrets) and `POST /reload`.
- SIGHUP and `POST /reload` reload the config: load, validate, re-read certificates, build, swap. Reloads are serialized, and a failure changes nothing. Backends keep their health, inflight count, breaker and pooled connections, routes their rate limiter, semaphore and retry budget, and the cache its entries, each while its settings are unchanged. Listener, admin, shutdown and ready path changes need a restart and are warned about. Series of removed routes and backends are deleted.
- The exit test is `TestReloadUnderLoad` (40 reloads through `POST /reload` under 8 busy workers, zero failures, connections reused, breaker and health state kept), and `make reload` runs `scripts/reload-under-load.sh` against the real binary with a `kill -HUP` every 0.25 s.

### M6: Advanced routing and security, rest of Layer 5 (1–1.5 weeks)

- Canary: a weighted split per route, plus a header or cookie override to force a version.
- Shadow traffic: send an async copy with the body buffered up to a cap and its own concurrency limit, and discard the response. Shadowing must never add latency to the primary request; test that explicitly.
- Basic auth (bcrypt plus constant-time compare), API keys stored as hashes, and IP allow/deny with `netip.Prefix`.
- WebSocket: detect `Upgrade`, hijack, run a bidirectional copy, and apply idle timeouts.
- HTTP/2 comes for free on TLS listeners; h2c is optional. OpenTelemetry tracing (W3C `traceparent` propagation) is a bonus.

**Status:** done on 2026-10-20. The decisions and details are in `docs/m6-handoff.md`. As built:

- IP allow/deny lists at the top level (before routing) and per route, on the client IP after `trusted_proxies`. Refusals get 403.
- Per-route auth: Basic with bcrypt hashes (a dummy hash for unknown users, verified passwords remembered for 5 minutes, bcrypt work capped at GOMAXPROCS), API keys looked up by SHA-256 digest, or either one. Checked credentials are stripped unless forwarded, and an optional identity header tells the backend who was let in. `GET /config` shows user and key names but no hashes. Build warns about a password route without a rate limit.
- Canary: a sticky split by client IP in 10,000 buckets, so raising the percent only adds clients, with `always`/`never` header and cookie overrides. Canary requests skip the cache and have their own metric series.
- Shadow: one copy per request that reaches a backend, sent on its own goroutine under its own timeout and concurrency cap, with the body copied from memory or captured as it streams. `TestProxyShadowAddsNoLatency` and the e2e `TestShadowAddsNoLatency` check that it never slows the real request.
- WebSockets, opt-in per route: the upgrade is forwarded, the connection hijacked and tracked so a shutdown waits for it and closes it at the deadline, bytes copied both ways under one idle timer, and the route's concurrency slot held for the connection's life.
- HTTP/2 on TLS listeners, tested for streaming, trailers, streamed uploads and cancellation, and `h2c: true` for prior-knowledge HTTP/2 on plain listeners. Backends stay on HTTP/1.1.
- Hand-written W3C `traceparent` propagation (no OpenTelemetry SDK): Prism joins or starts a trace, sends its own span as the parent of every backend request, and logs `trace_id`, `span_id` and `parent_id`.
- The exit tests are in `test/e2e/m6_test.go`: the canary split and overrides, shadow latency, the auth matrix, WebSocket echo, idle cutoff and shutdown, and HTTP/2 over TLS and h2c with trailers and trace propagation.

### M7: Benchmarks and write-up (1 week)

- Use k6 or vegeta to compare a direct backend, Prism and nginx on the same machine. Report p50/p99 added latency, requests per second, and allocations per request (`go test -bench -benchmem`, pprof).
- Profile the hot path and fix the top 2–3 allocation sources, such as header copying and buffer pooling.
- Write a README with an architecture diagram, design decisions and their tradeoffs, and a "what's not supported" section.

**Status:** done on 2026-10-21. The decisions and details are in `docs/m7-handoff.md`. As built:

- `make bench-compare` (`bench/run.sh`) compares a direct backend, nginx 1.28 in Docker and Prism under vegeta, with CPU pinning, fresh processes and a warm-up per scenario, and the median of three runs at 5,000 and 20,000 req/s plus closed loop. `bench/report` writes `results.md` and a latency-by-percentile SVG. The checked-in results are in `docs/bench/`.
- Results: Prism forwards about 70% of nginx's requests per second at about 2.4 times its CPU per request, adds 84 µs at the median at 5,000 req/s, and matches nginx on cache hits. Its maximum latency (13–28 ms) is well above nginx's.
- `BenchmarkPipeline` measures Prism's own cost per request without sockets, and `admin: {pprof: true}` serves the profiler for the real binary. Three allocation fixes from the profiles (empty date headers, the forwarding headers sharing one array, the traceparent formatted once) cut allocations per request from 26 to 21 (bare), 27 to 22 (full) and 13 to 9 (cache hit), per `benchstat`.
- The README has the architecture diagrams, design decisions and tradeoffs, the benchmark table, the chaos graphs, the test strategy and a "what's not supported" section.

### M8: Hardening, closing the gaps (1–1.5 weeks)

M8 closes the gaps that M4–M7 left open: the security holes, the cache's known limits, the health-check behaviour across reloads, and Prism's latency tail. It is planned from the open items in `docs/m4-handoff.md` to `docs/m7-handoff.md`.

**Decisions taken before M8.** The user took the recommended choice on each open decision (2026-10-22):

- **Dependencies are confirmed:** `golang.org/x/crypto` (bcrypt), and vegeta and benchstat as `go tool`s. No other new libraries.
- **The access log stays unbuffered by default.** A `-log-buffer <size>` flag (default 0, off) turns on a buffered writer that flushes when it is full, every second and at shutdown. This is opt-in, as nginx's `buffer=` is: with it on, a crash can lose up to a second of log lines, so whoever turns it on has accepted that.
- **The WebSocket idle default stays at 5 minutes.** nginx's 60 s closes quiet sockets that send no application pings, and `websocket.idle_timeout` already changes it per route.
- **`traceparent` is still accepted from any client.** It decides nothing in Prism, and joining traces that start in a browser or an app is the point of propagating it. Only `X-Request-Id` stays limited to trusted peers.

**Scope:**

- **Failed-auth limiter.** Every route with `auth` gets a per-client limit on failed credential checks: `auth.failure_limit`, a rate that defaults to `10/m` with a burst of 10, and that `off` disables. A request from a client over the limit gets 429 with `Retry-After` before any bcrypt work, so a password-guessing flood costs no CPU. Only failures count against the limit. IPv6 clients are keyed by their /64, so rotating addresses inside one allocation doesn't reset the count. A new `ratelimit.Limiter.Peek` checks the limit without spending from it. Failures and refusals get their own metrics, and Build no longer warns about a Basic auth route without a `rate_limit`.
- **Admin authentication.** `admin.auth` takes the same `basic` and `api_keys` blocks as a route's auth, with the same authenticator and failed-auth limiter. A non-loopback `admin.listen` without `admin.auth` becomes a validation error (it was a warning), since `/config`, `/reload`, `/cache/purge` and pprof should never be open to the network. Prometheus scrapes with a bearer key.
- **Readiness on the admin port.** The admin listener serves `GET /ready` (200 while serving, 503 once shutdown begins, the same state as `ready_path`) and `GET /live` (always 200). Both skip auth, because probes carry no credentials and the answers reveal nothing.
- **Bounded cache drain.** Once the leader's client leaves, the leader reads the rest of the backend body for at most the route's `cache.lock_timeout`. It then gives up and stores nothing, which frees its goroutine and concurrency slot when a backend stalls mid-body.
- **`stale-while-revalidate` (RFC 5861).** A new route setting, `cache.stale_while_revalidate`, mirrors `stale_if_error`: the route's value is the window, and a response's own directive can narrow it but not widen it. Within the window, a stale entry is served at once as `X-Cache: UPDATING`, and one background request per key, led through the existing `Flight`, revalidates it. The background request has a detached context, the route's concurrency slot and the upstream's timeouts. `must-revalidate`, `no-cache` and `s-maxage` forbid it, as they do stale-if-error. A new `updating` counter goes to `/cache/stats` and to the metrics.
- **Health state survives reloads.** The check streak and the time of the last check move into each backend's shared state, which reloads already carry over. A new checker continues the streak and schedules its first check one interval after the last one, instead of at a fresh random offset. A reload storm then no longer delays marking a failing backend down.
- **ip_hash spread.** Raise `pointsPerWeight` (within the `maxPoints` cap) until the distribution test shows about ±5% across 10 backends, down from about ±17%, and check the lookup benchmark. This remaps clients once, at the upgrade, and the upgrade notes say so. Add an e2e test of canary with `ip_hash` on both upstreams.
- **Rate-limit key cap.** The cap stays, since it is what bounds memory under a flood. Evictions forced by a full shard are counted, in `prism_ratelimit_evictions_total`, so an operator can see when the cap is being hit.
- **Latency tail.** Find the cause of Prism's 12–28 ms maximum latency. Under the bench load, take an execution trace from `/debug/pprof/trace` and run with `GODEBUG=gctrace=1`. If GC is confirmed, try cheap fixes (fewer allocations on the path the trace points at, or a `GOGC`/`GOMEMLIMIT` recommendation) and re-run the bench. The result goes into `docs/bench/` and the README, whether or not a fix lands. The same profile settles the per-request metric cost item from M5.
- **`go mod tidy`.** It breaks on the stray `/home/mudit/go/go.mod`. Moving that file is a change to the user's environment, so it waits for their OK. After that, run tidy so direct dependencies are marked properly.

**Steps:**

1. `-log-buffer` flag and the dependency housekeeping
2. Failed-auth limiter (`ratelimit.Peek`, `security`, config, metrics)
3. Admin auth and the non-loopback validation error
4. Admin `/ready` and `/live`
5. Bounded cache drain
6. `stale-while-revalidate`
7. Health state across reloads
8. ip_hash spread, the canary with ip_hash e2e test, and rate-limit eviction metrics
9. Latency tail investigation and bench re-run (AC power, `performance` governor, containers stopped, as in M7)
10. Exit tests, README "what's not supported", this status, `docs/m8-handoff.md`, and `go mod tidy` once the user allows it

**Out of scope.** These stay documented in the README rather than built: WebSockets over HTTP/2 (RFC 8441), a WebSocket close frame at shutdown (Prism does not parse frames), one span per attempt, the shadow's `body_skipped` trade-off, the canary hash ignoring the route (deliberate), coalesced SIGHUPs, and the "Later, if ever" list below.

**Exit:** e2e tests show the following:

- A burst of wrong passwords gets 429 after the limit, and bcrypt work stays flat.
- The admin API refuses requests without credentials, and a non-loopback admin listener without auth fails `-validate`.
- `/ready` flips to 503 at shutdown.
- A stale entry within `stale_while_revalidate` is served at once, while exactly one background request reaches the backend.
- A leader whose client left gives up on a stalled body after `lock_timeout`.
- Under a reload every 0.25 s, a failing backend is still marked down after `unhealthy_after` checks.
- The ip_hash spread is within ±5%.

The README's "what's not supported" section lists only the out-of-scope items above, and the cause of the latency tail is written down with evidence.

### Later, if ever

A disk cache, HTTP/3 via quic-go, adaptive concurrency limits, and ACME/autocert.

## Testing strategy

- **Unit tests:** table-driven tests for routing, header rewriting, balancers, freshness math and breaker state transitions.
- **Fault server:** a small binary with configurable latency, error rate, closing mid-body, never responding and slow reads. Most resilience tests depend on it.
- **e2e tests:** start a real Prism with `httptest` backends and assert on the backend side (which headers arrived, how many connections, whether cancellation happened).
- **Fuzzing:** run `go test -fuzz` on the config parser, `Cache-Control` parser and forwarded-header parser.
- **Run everything with `-race`.** Concurrency bugs are the main risk in this project.

## Scope advice

The full feature list is about 3 months of part-time work. For something showable sooner, M0–M3 plus metrics and the admin API from M5 already make a strong project. The caching layer is the next most impressive addition. HTTP/3 and the disk cache add little on a resume compared to their cost.

## Resume angle

Benchmarks with numbers and failure demos with graphs are what make this stand out. Target bullets like:

- "Built an HTTP reverse proxy in Go with 4 load-balancing strategies (smooth weighted RR, P2C least-conn, consistent hashing), adding X µs p99 overhead at Y k req/s."
- "Implemented circuit breakers, outlier ejection and retry budgets; under a single-backend failure, the error rate recovered within Z s with no retry amplification."
- "Designed an RFC 9111-aware response cache with request coalescing, reducing upstream load by N% on a Zipf workload."
