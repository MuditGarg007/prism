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
  → concurrency limit / bounded queue        → 503 + Retry-After
  → cache lookup (+ coalescing)              → HIT returns here
  → traffic split (canary) / shadow copy
  → balancer pick (skips unhealthy / ejected / breaker-open)
  → forward with deadline, retries, budget
  → cache store (tee body)
  → metrics + log finish
```

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
- Purge goes through the admin API. Add a bypass header, metrics for hit/miss/stale/revalidated, and an `X-Cache` response header.

**Exit:** a coalescing test where 1,000 concurrent requests for a cold key produce exactly 1 upstream hit.

### M5: Operations, first part of Layer 5 (1.5 weeks)

- Prometheus metrics: request counters per route, upstream and status; latency histograms per route and upstream; gauges for inflight, queue depth, breaker state and healthy backends.
- Structured JSON access logs with a request ID.
- An admin API on a separate port: `GET /routes`, `/upstreams` (live health, inflight, breaker state), `/config`, and `POST /cache/purge`.
- Hot reload on SIGHUP: parse → validate → build a new `Runtime` → atomic swap. Keep backend state (health, breakers, connection pools) for backends whose identity hasn't changed. Old requests finish on the old snapshot.

**Exit:** a reload under load test with zero failed requests (run the load generator while reloading in a loop).

### M6: Advanced routing and security, rest of Layer 5 (1–1.5 weeks)

- Canary: a weighted split per route, plus a header or cookie override to force a version.
- Shadow traffic: send an async copy with the body buffered up to a cap and its own concurrency limit, and discard the response. Shadowing must never add latency to the primary request; test that explicitly.
- Basic auth (bcrypt plus constant-time compare), API keys stored as hashes, and IP allow/deny with `netip.Prefix`.
- WebSocket: detect `Upgrade`, hijack, run a bidirectional copy, and apply idle timeouts.
- HTTP/2 comes for free on TLS listeners; h2c is optional. OpenTelemetry tracing (W3C `traceparent` propagation) is a bonus.

### M7: Benchmarks and write-up (1 week)

- Use k6 or vegeta to compare a direct backend, Prism and nginx on the same machine. Report p50/p99 added latency, requests per second, and allocations per request (`go test -bench -benchmem`, pprof).
- Profile the hot path and fix the top 2–3 allocation sources, such as header copying and buffer pooling.
- Write a README with an architecture diagram, design decisions and their tradeoffs, and a "what's not supported" section.

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
