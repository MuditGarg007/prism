# M5 handoff

As of 2026-10-20, M5 (operations) is complete: all 9 steps are done, and `make lint test validate` passes with `-race`. The exit test, `TestReloadUnderLoad`, passes, and so does `make reload` (`scripts/reload-under-load.sh`: 6,001 requests at 300 req/s with 64 SIGHUP reloads, zero errors). Nothing from M5 is committed yet; commit only when the user asks. Next is M6.

- The repo lives at `/home/mudit/prism`. The module is `github.com/MuditGarg007/prism`, on Go 1.27.
- M5 builds on `b2416f9` (M4). The working tree also holds a one-line, uncommitted edit to `docs/m4-handoff.md` that records the M4 commit. The user hasn't said yet whether it goes in its own commit or with M5.
- Run `make lint test validate` to check the current state. `make fuzz` is unchanged (eight targets). `make reload` runs the reload script (about 25 s).
- Every `go` command prints `go: warning: ignoring go.mod in $GOPATH /home/mudit/go`. The same stray `/home/mudit/go/go.mod` makes `go mod tidy` fail with "go.mod file not found", so `go.mod` lists every requirement as `// indirect`, as it already did for yaml. Add dependencies with `go get <pkg>@<version>`, including the test-only packages (`prometheus/testutil` needed its own `go get`).
- The source plan is `docs/project-plan.md`, section "M5: Operations, first part of Layer 5". Earlier state and decisions are in `docs/m1-handoff.md` through `docs/m4-handoff.md`.

## Decisions

These were accepted at the start of M5 ("start with step 1"). The open points took the recommended defaults: the `code` label is the status class, and TLS certificate reload is part of M5. Don't reopen them unless asked.

| # | Decision | Choice | Why |
| --- | --- | --- | --- |
| D1 | Metrics library | `prometheus/client_golang` v1.25.0. Admin `GET /metrics` serves one registry that also has the Go and process collectors. Without an `admin` block, metrics are still recorded but not served. | The project plan allows a library for Prometheus. Envoy also serves metrics on its admin port. |
| D2 | Hot-path cost | Handles are resolved once per route and backend in `runtime.Build`. The `code` label is the status class (`1xx`…`5xx`, plus `other` for statuses outside 100–599, which a backend can send), so recording a request indexes an array instead of hashing labels behind a lock. The access log keeps the exact status. | Exact codes through `WithLabelValues` cost about 100 ns and a shared RWMutex per request. |
| D3 | Metric set | `prism_requests_total{route,upstream,code}`, `prism_request_duration_seconds{route,upstream}`, `prism_upstream_response_seconds{upstream,backend}`, `prism_retries_total{route}`, `prism_retry_budget_exhausted_total{route}`, `prism_proxy_errors_total{route,kind}`, plus reload metrics in step 7. Buckets run from 1 ms to 30 s. Unmatched requests use `route=""`, `upstream=""`. | It covers RED per route and per backend, and makes the M3 resilience visible. |
| D4 | Gauges | A collector reads `cur.Load()` at scrape time and reports backend inflight, healthy, ejected and breaker state, available backends per upstream, route concurrency and queue depth, and the cache's counters and sizes. | Costs nothing per request, and follows reloads with no bookkeeping. |
| D5 | Request ID | `X-Request-Id`. It is kept only from a trusted peer, as a single value of at most 128 characters of `[A-Za-z0-9._-]`. Otherwise Prism generates 32 hex characters. It is sent upstream and back to the client, logged as `request_id`, and never stored in the cache. | Without the trusted-peer check, a client could choose the ID its requests are logged under. |
| D6 | Admin endpoints | `GET /routes`, `GET /upstreams`, `GET /config` (effective config as JSON, sizes and durations as strings, with a redaction hook for M6 secrets), `GET /metrics`, `POST /reload`. | `POST /reload` does the same as SIGHUP and makes testing easy. |
| D7 | What a reload keeps | `Backend` is split into a per-runtime part (URL, weight, outlier index, log, latency handle) and a shared state (inflight, unhealthy), keyed by (upstream, URL). The following are each carried over when their config is equal: the transport (TLS, timeouts, `max_idle_conns_per_host`), the breaker, and the pool's outlier detector (that one also needs the same backend list). Per route, keyed by route name: the rate limiter, semaphore and retry budget. The cache store, flight and stats are always carried over, and a new `Store.Resize` handles a changed `max_size`. Health checkers restart from the carried flag. Removing `health` marks the backends healthy. | Without this, inflight drifts for least_conn, every reload resets rate budgets and briefly doubles concurrency caps, and the cache goes cold. |
| D8 | Reload sequence | Reloads are serialized. Each one loads, validates and runs `Build(cfg, log, Options{Prev, Metrics})`, then swaps. It then stops the old runtime's checkers and starts the new one's. Finally `old.Retire(new)` closes idle connections only on transports the new runtime didn't carry over. A failure is logged and counted, and the old runtime keeps serving. Changes to listener addresses, admin or shutdown settings are warned about ("needs restart") and ignored. Certificate files are re-read and swapped through an `atomic.Pointer`. | In-flight requests finish on their own snapshot. Rotating certificates is the usual reason for a SIGHUP. |
| D9 | Stale series | On reload, delete the metric series of routes and backends that no longer exist. | Otherwise they stay frozen forever. |
| D10 | Exit test | `TestReloadUnderLoad` (in process): workers send requests while configs alternate in a loop (weight change, an added backend, a header change). It asserts zero failures, connection reuse, and that breaker and health state survive. `scripts/reload-under-load.sh` does the same with the real binary, loadgen and a `kill -HUP` loop. | This is the project plan's exit criterion, run both in CI and as a demo. |

The 9 steps are:

1. Request ID (D5)
2. Metrics core: the dependency, `observability.Metrics`, pre-resolved handles, recording in the access log, attempt timing
3. Scrape collector and `GET /metrics` (D4)
4. Admin `/routes`, `/upstreams`, `/config` (D6)
5. Backend state split, plus carrying over upstream state in `Build` (D7, upstream part)
6. Carrying over route and cache state, including `Store.Resize`
7. Reloader: SIGHUP, `POST /reload`, cert reload, reload metrics, series cleanup (D8, D9)
8. Exit test and script (D10)
9. Docs: example YAML comments, this handoff, M5 status in `project-plan.md`

## What is built

### Step 1: request ID (`proxy/requestid.go`)

- `proxy.RequestIDHeader = "X-Request-Id"`. `Handler.requestID(hdr, peer)` keeps an incoming ID only when the peer is valid and trusted and the header has exactly one value that passes `validRequestID`. Otherwise `newRequestID()` returns 16 bytes from `crypto/rand` as 32 lowercase hex digits (64 ns, 1 allocation for the string).
- `ServeHTTP` sets `an.RequestID` and `w.Header()["X-Request-Id"]` first, so 400, 404 and every rejection carry it. `r.Header` is not modified. The ID reaches the backend through `outgoing(r, rt, b, rb, id)`, which sets it after the forwarding headers and before the route's request rules, so `request_set`/`request_remove` can still override it.
- `fetch` deletes `X-Request-Id` from every backend response (next to `removeHopByHop`). The client therefore always gets this request's ID, and a cache entry never holds an ID that would be sent with another request's response. Waiters and hits each send their own.
- `logError` and `notStored` now take `*Annotations` instead of `*Route` (`an.Route` is always set where they're called) and log `request_id`, so a warning can be matched to its access log line. The access log puts `request_id` first.
- `Annotations` gained `RequestID` at the end of the struct (to avoid realigning the fields above it).
- Tests: `TestValidRequestID` (11 cases), `TestNewRequestID` (format, 1,000 distinct values), `TestProxyRequestID` (none / untrusted / trusted / malformed / two values, plus a backend that echoes a different ID), `TestProxyRequestIDOnRejection` (404, 400 and 502 with the ID in the error log), `TestCacheRequestID` (a hit gets its own ID even though the backend echoed the miss's), and `TestAccessLogProxied` checks `request_id=edge-42` from a trusted peer. `TestProxyClientCanceled` now ignores the request ID in the header map, which is set but never sent. `BenchmarkNewRequestID`.

### Step 2: metrics core (`observability/metrics.go`)

- `observability.NewMetrics()` creates a `prometheus.Registry` (exported as `Metrics.Registry`) with the six D3 vectors plus `collectors.NewGoCollector()` and `NewProcessCollector`. It outlives any one runtime: `main` makes one and passes it to every `Build`.
- `Metrics.Route(name, upstream) *RouteMetrics` resolves the route's series: 6 request counters (one per class in `codeClasses`, index `status/100`, with 0 for `other`), the duration observer, the retry and budget counters, and an errors vec curried with the route. All 6 classes are created up front, so `rate()` works before the first request. Calling it again for the same labels (as a reload will) returns handles to the same series. `Metrics.Unmatched()` is the route `""`.
- `RouteMetrics.Observe(observability.Request{Status, Duration, Attempts, BudgetExhausted, ErrorKind})` is nil-safe. Retries add `Attempts-1`. The only label lookup is `errors.WithLabelValues(kind)`, and only for failures (kinds are the fixed `errorKind` strings). It takes 105 ns/op in parallel on one hot route with 0 allocations; most of that is atomics contending on one cache line.
- `Metrics.Backend(upstream, backend)` returns a `prometheus.Observer`. `upstream.Backend` gained `Latency upstream.Observer`, a one-method interface defined there so `upstream` doesn't import Prometheus. `runtime.Build` sets it after `NewPool`. `fetch` times each `RoundTrip` and observes only attempts that got response headers. A connect error that fails in 1 ms would otherwise drag the latency distribution down.
- Wiring:
  - `runtime.Build(cfg, log, runtime.Options{Metrics})` is the new signature (`Options` will gain `Prev` in step 5). All callers pass `runtime.Options{}` except `main` and the e2e `launch`.
  - Build gives each `proxy.Route` its `Metrics` handle.
  - `proxy.NewHandler(table, fwd, unmatched, log)` takes the unmatched handle.
  - `ServeHTTP` sets `an.Metrics` to the unmatched handle first and to `rt.Metrics` once a route matches. The `server` package therefore never needs the registry.
- `server.logRequest` calls `an.Metrics.Observe(...)` before the log-level check, so metrics are recorded even with `-log-level warn`. A panicking handler is still recorded, because it runs in the access log's deferred function.
- Tests: `TestRouteMetricsObserve` (class mapping including 499 → 4xx and 700 → other, retries, budget, error kinds, a second handle for the same route adding to the same series, unmatched, histogram buckets), `TestBackendLatency`, `TestNilRouteMetrics`, `TestMetricsLint` (`testutil.GatherAndLint`, prism_ metrics only), `TestObserveAllocs` (0), `BenchmarkObserve`, and `server.TestAccessLogMetrics` (a proxied request, an unmatched request and the backend's latency through the real server).

### Step 3: scrape collector and `GET /metrics` (`admin/metrics.go`)

- `admin.collector{cur}` is an unchecked collector: `Describe` sends nothing, because the series change with every reload. `Collect` loads the current runtime once and emits const metrics:
  - `prism_backend_inflight`, `prism_backend_healthy`, `prism_backend_ejected` for every backend.
  - `prism_backend_breaker_state` (0 closed, 1 half-open, 2 open, so `> 0` means "refusing some traffic") only for backends that have a breaker.
  - `prism_upstream_available_backends` (healthy, not ejected, breaker not open). The plan said "healthy backends"; available is what the balancer actually sees, and per-backend `healthy` is there too.
  - `prism_route_concurrency_in_use`, `_limit` and `prism_route_queue_depth`, only for routes with `max_concurrency`.
  - With a cache: `prism_cache_requests_total{result}` (the X-Cache values), `prism_cache_entries`, `_bytes`, `_max_bytes`, `prism_cache_evictions_total`, `prism_cache_waiting`.
- New accessors: `Backend.Healthy()`, `Backend.Ejected()`, and `Runtime.Routes []*proxy.Route` (in config order; step 4's `/routes` uses it too). Route names are unique because `router.Compile` refuses a duplicate host plus prefix.
- `admin.New(cur, m, log)`: with `m` set, it registers the collector on `m.Registry` and serves `GET /metrics` through `promhttp.HandlerFor` with `ContinueOnError`, logging errors at warn. With `m == nil` there is no `/metrics` (404). It must be called once per registry, as `main` does.
- The cache counters come from `cache.Stats`, which step 6 carries across reloads, so they never reset while the process runs.
- Tests: `TestMetrics` (every gauge kind, an unhealthy backend lowering the available count, cache MISS/HIT, no limit gauges on an unlimited route, the request metrics and `go_goroutines` in the same scrape, the lint check), `TestMetricsFollowReload` (a removed backend's series disappear after `cur.Swap`), `TestNoMetrics`, `TestBreakerValue`, and the e2e `TestRequestIDAndMetrics` in `test/e2e/m5_test.go` (IDs on both sides of a miss and a fresh one for the hit, then `/metrics` over the real admin listener: requests, upstream latency count, cache HIT, backend healthy).

### Step 4: admin `/routes`, `/upstreams`, `/config` (`admin/state.go`, `admin/config.go`)

- Every body is a JSON object, so fields can be added later without breaking clients. All three read `cur.Load()` per request, like the cache endpoints.
- `GET /routes` returns `admin.Routes{routes: []RouteInfo}` in config order: `name`, `host` (`""` for any host), `path_prefix`, `upstream`, `cache`, and `concurrency {in_use, limit, queued}` only for routes with `max_concurrency`. The host comes from `rt.Config.Routes[i]`, which relies on `Build` keeping `rt.Routes` in config order. Every other route setting is in `/config`.
- `GET /upstreams` returns `admin.Upstreams{upstreams: []UpstreamInfo}` sorted by name: `balancer` (after defaults), `available`, and per backend `url` (`Backend.String()`, so the password is masked), `weight`, `inflight`, `healthy`, `ejected`, `breaker` (`closed`, `half_open` or `open`, the `BreakerState` names; absent without a circuit breaker) and `available`.
- `GET /config` writes the effective config (defaults filled in) through `encodeConfig`, a small reflection walker rather than `encoding/json`, which would use Go field names and nanoseconds:
  - Keys are the yaml tag names, in struct order. Map keys are sorted. Nil pointers are `null`, and nil slices are `[]`.
  - `time.Duration` uses `String()` (`"1m0s"`). Any `encoding.TextMarshaler` is written as its text. `config.ByteSize` and `config.Rate` gained `MarshalText` (`"64MB"`, `"1500B"`, `"100/s"`, all readable by their parsers), and `config.Prefix` already had one through the embedded `netip.Prefix`.
  - **Redaction hook for M6:** a `redact:"secret"` struct tag replaces a non-empty value with `"[redacted]"` (an empty one is shown, so a reader can tell none is set), and `redact:"url"` masks a URL's password. `config.Backend.URL` carries `redact:"url"` now. M6 tags its passwords and API key hashes with `redact:"secret"`.
- Tests: `TestRoutes`, `TestUpstreams` (least_conn and round_robin, an open breaker after one failure with `min_requests: 1`, an unhealthy backend, inflight, a masked password), `TestStateFollowsReload`, `TestConfig` (27 paths through the decoded body, the password absent, field order, sorted map keys), `TestEncodeConfigRedact` (secret, empty secret, a secret slice, url, an unparseable url, `yaml:"-"`, an untagged field, an unexported field), `config.TestMarshalText` (round trips through the parsers), and new rows in `TestRouting` for 405s.

### Step 5: backend state split and upstream carry-over (`upstream/upstream.go`, `upstream/transport.go`)

- `upstream.Backend` now points to a shared `state` (inflight and the inverted unhealthy flag). Every runtime's `Backend` for the same upstream and raw config URL shares it, so least_conn sees one count, and requests still running on the old runtime release into it. URL, weight, transport, breaker, outlier index, logger, `Latency` and the transport settings stay per `Backend`.
- **A zero `Backend` is no longer usable** (its state is nil). `upstream.NewBackend(url, weight)` makes one for tests and balancers; the balancer and upstream tests use it now.
- `NewPool(name, u, newBal, log, prev *Pool)` takes the same-named pool of the runtime being replaced (nil for none; every test passes nil). The pool keeps the `*config.Upstream` it was built from, and its outlier detector, for the next reload. Matched by raw `url` string (validation already rejects duplicates in one upstream), a backend:
  - always keeps its state;
  - keeps its `*http.Transport`, and with it the pooled connections, when its `transportConfig` is equal. That is a comparable struct of the connect, response-header and idle timeouts, `max_idle_conns_per_host`, the scheme, `server_name`, the `ca_file` path, **and the CA file's contents**, so rotating a CA in place takes effect on reload. `newTransport(tc)` and `clientTLSConfig(tc)` now build from it;
  - keeps its breaker when `circuit_breaker` is equal. The breaker's state-change log closes over a logger (`logBreaker(log)`), not the `Backend`, because a carried breaker outlives the `Backend` that built it.
- The pool keeps its outlier detector only when the `outlier` config **and the whole URL list, in order,** are equal, since it tracks members by index. Adding, removing or reordering a backend resets ejections.
- Without `health`, every backend is marked healthy, so removing health checks brings a down backend back. (Since step 7 this happens in `Pool.Commit` at the end of `Build`, not in `NewPool`.)
- `health.Checker` starts each tracker from `b.Healthy()`. A backend that was down before a reload needs `healthy_after` passing checks to come back, rather than flipping on the first one (before this, the tracker would never have marked it healthy at all).
- `runtime.Options.Prev`; `Build` passes `prev.Pools[name]` to `NewPool`. `Runtime.Retire(next)` stops the checkers and closes idle connections only on transports that `next` doesn't use (`Pool.CloseExcept`/`AddTransports`). `Close()` is `Retire(nil)`. A failed `Build` cleans up with `Retire(prev)` in a deferred function, and a failed `NewPool` closes only the transports it created, so a bad reload never closes connections the serving runtime is using. Retire is here rather than in step 7 because the failure path needs it; the reloader just calls `old.Retire(new)` after the swap.
- Tests: `upstream.TestPoolCarryOver` (9 config changes, each checking which of transport, breaker, outlier and health carried over, with inflight released through the old pool), `TestPoolCarryOverCA` (the same path with a rewritten file gets a new transport), `health.TestCheckerStartsFromState`, `runtime.TestReloadKeepsConnections` (counts backend connections: a carried transport survives `Retire`, a changed `idle_conn` dials again, and a build that fails after carrying a transport leaves it open; mutation-checked), and `TestReloadKeepsBackendState`.

### Step 6: route and cache carry-over (`runtime/runtime.go`, `cache/store.go`)

- `Build` indexes the previous routes by name (host plus path prefix, unique through `router.Compile`), pairing each with its config through the shared index of `prev.Routes` and `prev.Config.Routes`. A route keeps:
  - its rate limiter when `rate_limit` is equal (rate and burst);
  - its semaphore when `max_concurrency` and `queue_size` are both equal. A changed cap gets a fresh semaphore, so until old requests finish the route can briefly run old plus new; D7 accepted that;
  - its retry budget when `budget_ratio` and `budget_min_per_sec` are equal. The `Retry` itself is rebuilt every time (attempts, `on` and backoff can change freely); only `Retry.Budget` is shared.
- A renamed route (new host or prefix) starts fresh.
- The cache's store, flight and stats are always carried over while any route caches, so a reload keeps it warm and fetches in progress keep their waiters. A changed `max_size` calls the new `Store.Resize`, which evicts least recently used entries until the store fits (counted as evictions). The resize runs only at the very end of a successful `Build`, because the store is shared: an earlier resize followed by a failure would shrink the cache the old runtime is still serving. The new runtime gets its own copy of `runtime.Cache`, so the old one's `MaxSize` is unchanged. With no caching route left, the new runtime has no cache, and the old store goes away with the old runtime.
- Tests: `cache.TestStoreResize` (LRU order, the Vary index and the eviction count when shrinking, then room again after growing), `runtime.TestReloadKeepsRouteState` (7 changes, each checking limiter, semaphore, budget and cache identity), `TestReloadShrinksCache` (5 cached 100 KiB responses, a reload to 250KB keeps the 2 most recent, and dropping the last caching route drops the cache), and `TestFailedReloadKeepsCacheSize` (mutation-checked against resizing early).

### Step 7: the reloader (`runtime/reloader.go`, `server/server.go`, `admin/admin.go`, `cmd/prism/main.go`)

- `runtime.NewReloader(path, cur, metrics, certs, log)`. `Reload()` holds a mutex for the whole reload, because two Builds from the same `Prev` would carry the same state into two runtimes. The sequence: `config.Load`, `keepRestartOnly`, load the certificates (without using them yet), `Build(cfg, log, Options{Prev: cur.Load(), Metrics})`, and only then swap the certificates, `cur.Swap(next)`, `next.Start()`, `old.Retire(next)` and `metrics.Prune(old.series(), next.series())`. Any error before the swaps returns with nothing changed. It is logged at error ("reload failed, the previous config keeps serving") and counted. A success logs "config reloaded" with the counts and how long it took.
- `keepRestartOnly` copies the old `listeners`, `admin`, `shutdown` and `ready_path` into the new config when they differ, with one warning each (`setting=...`), so `GET /config` shows what is actually running. Listeners count as unchanged when only their certificate pairs differ (address, TLS on or off, and timeouts are compared), so new cert paths take effect.
- `runtime.CertLoader` is `func([]config.Listener) (swap func(), err error)`, and `server.Server.ReloadCerts` is one. Each TLS listener's `certStore` now sits behind an `atomic.Pointer` that `GetCertificate` loads per handshake, so rotation needs no restart. `ReloadCerts` loads every listener whose index, address and TLS setting still match `New`'s, all or nothing, and the returned swap stores them. A listener that moved is skipped; `keepRestartOnly` has already put the old one back.
- `admin.New(cur, m, reload, log)`: with `reload` set, `POST /reload` calls it and answers `{"reloaded": true}`, or 500 with `{"error": "reload failed: ..."}`. Without it there is no such route (404).
- `main` makes the Reloader right after `server.New` (with `srv.ReloadCerts`) and passes `reloader.Reload` to the admin API. It then runs a loop in which SIGHUP calls `Reload`, and SIGINT/SIGTERM or a listener error break out to the shutdown. The deferred close is now `cur.Load().Close()`, so it closes whichever runtime is current. A reload runs on the main goroutine, so a SIGTERM during one waits for it to finish.
- Reload metrics (D3), named after Prometheus's own: `prism_config_reloads_total{result="success"|"failure"}` (both created up front), `prism_config_last_reload_successful` and `prism_config_last_reload_success_timestamp_seconds`. `Metrics.ConfigLoaded(now)` sets the last two at startup, and `Metrics.Reloaded(err, now)` records each reload.
- Series cleanup (D9) is `Metrics.Prune(old, next observability.Series)`:
  - a `(route, upstream)` pair that is gone loses its request and duration series;
  - a route name that is gone also loses its retry, budget and error series;
  - an `(upstream, backend)` pair that is gone loses its latency histogram.

  A request still finishing on the old runtime after the prune records into a deleted series and is lost, which is accepted. A route that comes back later starts from zero, which `rate()` reads as a counter reset.
- **The open item from step 5 is fixed.** `NewPool` no longer marks backends healthy when the config has no `health`. The new `Pool.Commit()` does, and `Build` calls it for every pool at the very end, next to the cache resize, so a failed reload never touches shared health state.
- Tests:
  - `runtime.TestReloader`: a swap that keeps and warns about every restart-only setting, loads the certificates for the old listeners and swaps them once, and sets the reload gauges. Then four failures (an unreadable file, an invalid config, a certificate error and a build error), each leaving the runtime, the certificates and the failure counter as they should be.
  - `TestReloaderPrunesMetrics`, and `TestReloaderConcurrent` (8 racing reloads under `-race`).
  - `server.TestReloadCerts`: rotated files are served only after the swap, a bad file changes nothing, and a moved listener is skipped.
  - `admin.TestReloadEndpoint`, `observability.TestReloadMetrics` and `TestPrune`.
  - `upstream.TestPoolCarryOver` now checks that `NewPool` leaves health alone until `Commit`.

### Step 8: the exit test and script (`test/e2e/m5_test.go`, `scripts/reload-under-load.sh`)

- The e2e `launch` now writes the YAML to a file in a temp dir, loads it with `config.Load`, and wires a Reloader and the admin API exactly as `main` does. It returns an `*instance` (the `*server.Server` embedded, plus the config path), and `writeConfig` rewrites the file.
- `TestReloadUnderLoad`: 8 workers with keep-alive clients send requests nonstop while the test reloads 40 times through `POST /reload`, 20 ms apart. The configs cycle through a weight change, an added third backend, and a changed `X-Gen` response header, on a `least_conn` upstream with health checks and a breaker. A separate `side` upstream has a backend whose breaker was opened (3 failures, `open_for: 1h`) and whose health checks then fail. The test asserts:
  - zero failed requests (about 3,300 per run);
  - at most `2 × (workers + 2)` connections to the two backends that never leave (a run makes about 12). b3 is left out, because a reload that drops it closes its connections;
  - that b3 served traffic and `X-Gen: b` was seen, so the configs did apply;
  - that the side backend is still down with its breaker open right after the last reload, before fresh checks could have failed twice;
  - that every backend's inflight count settles at zero after the load stops.

  It was mutation-checked. Without the transport carry-over it counts 341 connections, and without the breaker or the shared-state carry-over the side backend's assertion fails. It passed 8 runs in a row under `-race`, about 1 s each.
- `scripts/reload-under-load.sh` (`make reload`) reuses `scripts/chaos/common.sh`: three faultservers, the real binary, and the loadgen at 300 req/s for 20 s. From second 2 it sends a `kill -HUP` every `INTERVAL` (0.25 s), each after writing the next config version to a temp file and renaming it into place. It reads `prism_config_reloads_total` from the admin API and writes the usual `load.csv`, `events.tsv`, `prism.log` and `graph.svg` to `bin/chaos/reload-under-load`. It exits 1 unless there were no request errors, no failed reloads, and as many applied reloads as signals.
- A note for later scripts: `start_prism` must not be fed through a pipe. A pipe runs it in a subshell, which loses `PIDS[prism]`.

### Step 9: docs

- `examples/prism.yaml` describes the reload (what is kept, and what needs a restart), the admin endpoints, and the request ID under `trusted_proxies`.
- `docs/project-plan.md` has the M5 status, and this handoff covers every step.

## Next steps

- M6 (advanced routing and security), from `docs/project-plan.md`. Tag its secrets with `redact:"secret"` so that `GET /config` hides them, and decide on admin authentication there.

## Open items carried forward

- [ ] **Health check streaks restart on every reload.** Each reload starts new checkers whose trackers begin from the shared healthy flag but with a zero streak and a random first offset. A backend that starts failing needs `unhealthy_after` failures in a row within one config, so reloading more often than every `unhealthy_after × interval` (a reload storm) delays marking it down. Carrying the checker over when the `health` config is equal would fix it.
- [ ] **Coalesced SIGHUPs.** The signal channel holds one pending SIGHUP, so several sent during one reload become one more reload, which reads the latest file anyway. That is fine, but a script that counts one reload per signal must give each reload time to finish (the script waits 0.25 s).

- [ ] **`go mod tidy` is broken by the stray `/home/mudit/go/go.mod`.** Removing or moving that file (the user's environment, so ask first) would let tidy mark direct dependencies properly.
- [ ] **Readiness on the admin port** (from M4). Not in the M5 plan; still possible on `admin.New`'s mux.
- [ ] **Admin authentication.** Still none. `/config` (step 4, which shows header values, cert and key paths and everything else in the file except masked URL passwords) and `POST /reload` (step 7, which also returns the reload error text) raise the stakes, so the non-loopback warning matters more. Revisit with M6.
- [ ] **Per-request metric cost under heavy contention.** 105 ns parallel on one hot route. If M7 profiling shows it, per-CPU sharding or native histograms are options.
- [ ] Unchanged from `docs/m4-handoff.md`: the leader's undeadlined drain, `stale-while-revalidate`, WebSocket, ip_hash spread and the rate-limit key cap.

## Conventions

- Claude writes code directly into the files; the user doesn't type it from chat. Plan each step in chat, implement one step at a time, and run `make lint test` after each.
- Edit in place and never overwrite the user's own work. Match the existing style: short doc comments that explain why, table-driven tests, and fuzz tests against a simple reference.
- Time-based logic takes an injectable `now`, and tests run on a fake clock.
- Every hot-path function gets a benchmark with `ReportAllocs`.
- Add each new fuzz target to the Makefile's `fuzz` target.
- Libraries are allowed only for YAML, Prometheus and OpenTelemetry.
- Commit only when the user asks.
