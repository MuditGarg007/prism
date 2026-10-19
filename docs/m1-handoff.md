# M1 handoff

As of 2026-10-19, M1 (the core proxy) is complete. All 9 steps are done, and `make lint test validate` passes with `-race`, including the five e2e exit tests. The binary runs: `prism -config <file>` serves traffic and shuts down on SIGINT/SIGTERM.

- The repo lives at `/home/mudit/prism`. The module is `github.com/MuditGarg007/prism`, on Go 1.27.
- The last commit is `6a28c1c` (M0). All M1 work is uncommitted.
- Run `make lint test validate` to check the current state, and `make fuzz` to run the four 30 s fuzz targets.
- Every `go` command prints `go: warning: ignoring go.mod in $GOPATH /home/mudit/go`. This comes from the local environment and is harmless.
- The source plan is `docs/project-plan.md`, section "M1: Core proxy, Layer 1".

## Decisions

These were accepted at the start of M1. Don't reopen them unless asked.

| Decision | Choice | Why |
| --- | --- | --- |
| Upstream Host header | Rewritten to the backend's host. A route can opt out with `preserve_host: true`. The original host is sent in `X-Forwarded-Host`. | This is nginx's default. |
| Transport scope | One `http.Transport` per backend, not per upstream | TLS `server_name` and `ca_file` are set per backend. |
| Balancer in M1 | The `Balancer` interface plus an atomic round-robin. Every other balancer type falls back to round-robin until M2. | Configs with several backends work now. |
| Path matching | Plain string prefix, so `/api` also matches `/apiv2` | nginx semantics. To match whole path segments, end the prefix with `/`. |
| `trusted_proxies` | Top level, shared by all listeners | Simpler, and nothing needs per-listener lists yet. |
| Host-group fallback | None. A path miss inside the chosen host group is a 404. | nginx server blocks and Envoy virtual hosts behave the same way. |
| Upstream protocol | HTTP/1.1 only, set explicitly with `http.Protocols` | Keeps Upgrade simple and gives one request per connection, which least-conn assumes. |
| `ca_file` | Replaces the system roots instead of adding to them | Keeps verification strict for private CAs. |
| Listener timeouts | Only `read_header` and `idle`. There is no read or write timeout for the whole request. | A whole-request timeout would kill 1 GB uploads and SSE streams. |

## What is built (steps 1–9)

### Step 1: config (`internal/config`)

- New fields:
  - Top level: `trusted_proxies` and `forwarded_header`.
  - Listeners: `timeouts.read_header` (default 10s) and `timeouts.idle` (default 120s).
  - Upstreams: `timeouts.idle_conn` (default 90s) and `max_idle_conns_per_host` (default 64; Go's own default of 2 is too low).
  - Routes: `preserve_host`.
- The `Prefix` type wraps `netip.Prefix` and parses straight from YAML, so errors carry line numbers.
  - A bare IP becomes `/32` or `/128`.
  - Host bits are masked off.
  - IPv4-mapped prefixes are converted to plain IPv4.
  - Zoned addresses are rejected.
- Validation now also rejects:
  - header names that aren't tokens
  - header values containing CR, LF or NUL
  - hop-by-hop headers in header rules
  - `Host` in `request_remove`
- `examples/prism.yaml` shows every new field.

### Step 2: router (`internal/router`)

- `Compile[T](entries) (*Table[T], error)` and `Match(host, path) (T, bool)`. The router is generic and does not import config.
- Host matching is tried in this order:
  1. An exact host, via a map.
  2. A wildcard. Each suffix is looked up from the leftmost dot, so the longest suffix wins. `*.example.com` matches any depth of subdomain, but not `example.com` itself.
  3. Routes with no host.
- Hosts are normalized by lowercasing and stripping the port, IPv6 brackets and a trailing dot.
- Within a host group, prefixes are sorted longest first.
- `StripPrefix` always keeps a leading `/`.
- `Match` takes 34–53 ns with 0 allocations, a figure from benchmarking 202 routes.
- Tested by `FuzzMatch`, which compares `Match` against a brute-force reference.

### Step 3: upstream (`internal/upstream`, `internal/balancer`)

- `Backend` holds the URL, weight and transport, plus counters:
  - `inflight` (`Acquire`, `Release`, `Inflight`)
  - an inverted `unhealthy` flag, so a zero-value Backend is available
- `Pool` has `NewPool`, `Pick` and `Close`. `Close` closes idle connections and is meant for the M5 reload.
- `Balancer` and `ErrNoBackend` live in `upstream`. The implementations live in `internal/balancer`, which avoids an import cycle.
- Transport settings:
  - `Proxy: nil`, so the environment's proxy settings are ignored
  - a dialer with the connect timeout and 30s TCP keep-alive
  - the idle and response-header timeouts from config
  - `DisableCompression: true`
  - TLS 1.2 or newer
  - `ca_file` and `server_name` from config
- `balancer.New(kind)` returns round-robin for every kind. `balancer.Implemented(kind)` lets the runtime warn about that.
- Round-robin picks take 15.7 ns with 0 allocations.

### Step 4: headers (`internal/proxy/headers.go`, `forwarded.go`)

- `cleanRequestHeaders` strips the fixed hop-by-hop list, plus every header named in `Connection`.
  - The list includes `Proxy-Authorization` and `Proxy-Authenticate`.
  - `TE: trailers` survives, because gRPC needs it.
- `HeaderOps` (`NewHeaderOps`, `Apply`, `Get`, `Empty`):
  - Removes first, then sets.
  - Names are canonicalized at build time.
  - Sets are applied in sorted order.
  - Each request gets fresh value slices.
- `PeerAddr(remoteAddr)` returns the peer IP unmapped and without a zone. It returns an invalid address for non-IP peers.
- `Forwarding.Apply(out, peer, host, isTLS)`:
  - **Untrusted peer:** X-Forwarded-For is replaced with the peer IP. `X-Forwarded-Proto` and `X-Forwarded-Host` are rewritten. `Forwarded` is dropped.
  - **Trusted peer:** the peer IP is appended to X-Forwarded-For, and multiple lines are joined. Proto and Host values the request already has are kept.
  - **`forwarded_header: true`:** an RFC 7239 element is added, such as `for="[v6]";host=...;proto=...`. It is appended to the existing header when the peer is trusted.
- `Forwarding.ClientIP(inHeader, peer)`:
  - Walks X-Forwarded-For from the right, skipping trusted hops. The first untrusted address wins.
  - If every entry is trusted, the leftmost one wins.
  - A garbage entry stops the walk, and the last address reached is returned.
  - Accepts `ip`, `ip:port`, `[v6]:port` and `[v6]`.
  - Takes 97 ns with 0 allocations.
- Tested by `FuzzClientIP`, which compares `ClientIP` against a reference and checks that `Apply` never keeps a forged chain from an untrusted peer.
  - An earlier fuzz failure came from a bug in the test oracle itself (`TrimSpace` vs `textproto.TrimString`). The failing input is kept in `testdata/fuzz` as a regression seed.

### Step 5: forwarding core (`internal/proxy/proxy.go`)

- `Route` carries the pool, `PathPrefix`, `StripPrefix`, `PreserveHost`, and the request and response `HeaderOps`. Step 7 builds these.
- `Handler` (`NewHandler(table, forwarding)`) is immutable. A reload builds a new `Handler` and swaps the pointer, so `proxy` never imports `runtime` and there is no import cycle.
- `validPath` rejects, with 400, any path that is not rooted or that has an empty, `.` or `..` segment. A trailing `/` is allowed.
  - It checks the decoded `URL.Path`, so `%2e%2e` and `..%2F` are caught too. The cost is that `%2F%2F` is rejected as `//`.
  - It takes 25 ns with 0 allocations. `FuzzValidPath` checks it against `path.Clean`.
- `targetURL` takes the scheme and host from the backend and keeps `RawQuery` and `ForceQuery`. With `strip_prefix`, `RawPath` is stripped only if it has the same literal prefix. Otherwise it is dropped and Go re-encodes `Path`.
- The outbound request is a struct literal plus `WithContext(r.Context())`, not `NewRequestWithContext`, which would parse a URL string again.
  - Headers are built in this order: clone, `cleanRequestHeaders`, `Forwarding.Apply`, the route's request ops, then the Host rule. Host is the backend's host, or the client's with `preserve_host`, or the `request_set` value. A `request_set` Host is also deleted from the header map.
  - When the client sent no `User-Agent`, an empty one is set, so Go doesn't add `Go-http-client/1.1`.
  - A `ContentLength == 0` body becomes `http.NoBody`. Otherwise `r.Body` and `r.Trailer` pass straight through.
- The response is built in this order: strip hop-by-hop headers, apply response ops, copy headers, announce trailers (sorted), then `WriteHeader`.
  - If the backend sent no `Content-Type`, it is set to nil, which stops net/http from sniffing one.
  - Headers are flushed right away for streams (`text/event-stream` or length `-1`) and whenever there are trailers. Without that flush, net/http would swap chunking for a Content-Length on short bodies and drop the trailers.
  - The body is copied through a 32 KB `sync.Pool` buffer and flushed after every write on streams. `ErrNotSupported` just turns flushing off.
  - Trailers are copied with `http.TrailerPrefix`.
- A failed body read from the backend after the headers went out does `panic(http.ErrAbortHandler)`. A failed write to the client just returns.
- A whole request through the proxy over loopback takes about 58 µs and 147 allocations, mostly inside net/http.

### Step 6: error mapping (`internal/proxy/errors.go`)

- `NewHandler` now takes a `*slog.Logger` as its third argument.
- `classify(ctx, err)` returns a status and a kind. Kinds are stable strings, meant to become metric labels in M4.

  | Condition | Status | Kind |
  | --- | --- | --- |
  | Client's context is `Canceled` | 499 (logged only, nothing is written) | `client_closed` |
  | `ErrNoBackend` | 503 | `no_backend` |
  | `context.DeadlineExceeded`, or a `net.Error` with `Timeout()`. This covers both the connect timeout and the response-header timeout. | 504 | `timeout` |
  | `*net.OpError` with `Op == "dial"` | 502 | `connect` |
  | Anything else | 502 | `upstream` |

- The cancellation check is first, because a backend error during a client cancel is only a side effect. It matches `context.Canceled` only, so a future per-route deadline on the context gives 504, not 499.
- `ErrNoBackend` is 503, not 502. No backend was tried, and the cause is usually temporary. This matches Envoy's "no healthy upstream".
- A connect timeout is 504, as in nginx.
- `fail` writes `http.Error` with the status text. It logs at WARN, or at DEBUG for 499, with method, host, path, upstream, backend, status, kind and error.
- `abort` handles a body read error after the headers were sent. It logs at WARN, then does `panic(http.ErrAbortHandler)`. If the client already left, it returns quietly instead.
- `StatusClientClosedRequest` (499) is exported for the step 8 access log.
- The tests build their errors from real failures:
  - a refused port
  - a 1 ns dial timeout
  - a backend that never sends headers
  - a listener that accepts and hangs up
- `TestProxyClientCanceled` checks that a client cancel reaches the backend within 1 s and that nothing is written.

### Step 7: runtime snapshot (`internal/runtime`)

- `Build(cfg, log) (*Runtime, error)` builds the parts below. If any of them fails, it closes the pools it already built.
  - One pool per upstream, built in sorted name order so logs and errors come out the same every run.
  - The `*proxy.Route` values, with their `HeaderOps` compiled once.
  - The router table.
  - `Forwarding`.
  - The `proxy.Handler`.
- `Runtime` holds `Config`, `Handler` and `Pools`. It never changes after `Build`. `Close` closes the idle connections of every pool.
- `Current` wraps an `atomic.Pointer[Runtime]`.
  - `ServeHTTP` loads it once per request, so each request is served entirely by one config.
  - `Swap` returns the old runtime. M5 must drain it before calling `Close`.
- Warnings:
  - An unimplemented balancer: "balancer is not implemented yet, using round_robin".
  - A config option that is set but not implemented yet: "config option is not implemented yet and is ignored", with `option=routes[0].max_body` and similar. This covers `health`, `circuit_breaker`, `rate_limit`, `max_body`, `max_concurrency`, `retry` and `cache`. Remove each one from `warnIgnored` as its milestone lands.
- A request that misses every route costs 149 ns and 3 allocations through `Current`. The allocations all come from `http.NotFound`.
- Tests:
  - routing through a built runtime: host groups, no fallback, strip prefix, header rules
  - a bad CA PEM failing `Build`
  - an unknown upstream returning an error instead of panicking
  - `Swap`
  - a `-race` test that swaps the runtime while requests are in flight

### Step 8: server and wiring (`internal/server`, `cmd/prism/main.go`)

- **Proxy annotations.** `proxy.Annotations` holds `Route`, `Backend`, `ClientIP` and `Error` (the error kind).
  - The proxy fills them in when the `ResponseWriter` implements `proxy.Annotated`. The access log reads them, so it needs neither the reloadable trusted-proxy list nor a `context.WithValue`.
  - `ClientIP` is computed for every request, before path validation, so a 400 also logs the client.
  - `proxy.Route.Name` is the route's host plus its path prefix, such as `api.example.com/v1/`.
- **`certs.go`.** `loadCerts` indexes each pair by its leaf `DNSNames` (lowercased).
  - When two certificates share a name, the first one listed wins. The first certificate is also the fallback.
  - A certificate with no DNS SANs is an error.
  - `get` tries the exact name, then a wildcard covering exactly one label (RFC 6125, stricter than the router), then the fallback.
  - `get` takes 27 ns with 0 allocations.
- **`accesslog.go`.** `logWriter` records the status (the first one ≥ 200) and the bytes written. It has `Unwrap`, so `ResponseController` can still Flush through it.
  - The log line is written in a defer that recovers and re-panics, so aborted responses are logged with `aborted=true`.
  - A request where nothing was written and the client's context is done is logged as 499.
  - Fields: method, host, path (no query string), proto, status, bytes, duration, client, route, upstream, backend, error.
- **`server.go`.** `New(listeners, handler, log)` builds one `http.Server` per listener, with `ReadHeaderTimeout`, `IdleTimeout`, and `ErrorLog` at WARN.
  - TLS uses `MinVersion` 1.2 and `GetCertificate`. It is served with `ServeTLS(ln, "", "")`, which gives h2.
  - `Start` binds every listener before serving any of them, so a port that is in use fails before traffic is accepted.
  - `Err()` reports listener failures. `Addrs()` returns the bound addresses, which tests need for `:0`.
  - `Shutdown(ctx)` shuts down every listener in parallel, then calls `Close` on any that missed the deadline.
- **`main.go`.**
  - Startup order: load the config, `runtime.Build`, then `server.New`. Both of the last two also run under `-validate`, so a bad CA PEM or cert pair fails validation, as with `nginx -t`.
  - Then `Start`, and wait for a signal or a listener error.
  - Shutdown has a 15 s deadline.
- **Tests.**
  - certificate selection, including duplicates, depth and SNI case
  - certificate load errors
  - timeouts copied from config, and no whole-request timeouts
  - a slow-header client cut off after `read_header`
  - TLS end to end: SNI picks the certificate, h2 is negotiated, TLS 1.1 is rejected
  - a port in use
  - Shutdown
  - access log for a proxied request with every field, through a real runtime
  - access log for a 499, an abort, and Flush through the wrapper
  - `-validate` failing on a CA file with no certificates
- A manual smoke test with the real binary covered a 200, a 400 for `/a/../etc/passwd`, a 502 with `kind=connect` once the backend was down, and a clean exit 0 on SIGTERM.

### Step 9: e2e tests (`test/e2e`)

- `startProxy` builds the real stack from a YAML string: `config.Parse`, `runtime.Build`, `server.New` and `Start` on `127.0.0.1:0`. Backends are `httptest` servers registered first, so the proxy shuts down before them.
- One test per exit criterion below. The whole package takes about 3.5 s under `-race`.
  - The 1 GB upload uses a fixed `Content-Length`. The 1 GB download is chunked, so it also covers the flush-per-write path. Measured heap growth is about 0.2 MB up and 3 MB down.
  - The XFF test also checks `Forwarded`: dropped from an untrusted peer, passed through from a trusted one.
  - The SSE request has a 5 s deadline, because a buffering proxy never sends headers and the test would otherwise hang.
- Each test was checked against a deliberate bug and failed as it should: flushing turned off (SSE), `context.WithoutCancel` on the outgoing request (cancel), every peer trusted (XFF), and `DisableKeepAlives` on the backend transport (keep-alive).

## Open items carried forward

- [x] **Path cleaning (step 5).** Done: non-canonical paths get 400 before `Match`.
- [x] **Host from rules (step 5).** Done in `Handler.outgoing`.
- [x] **`-validate` builds the runtime (steps 7–8).** Done: it runs `runtime.Build` and `server.New`.
- [x] **Balancer warning (step 7).** Done in `runtime.Build`.
- [ ] **WebSocket (M6).** `cleanRequestHeaders` currently strips `Upgrade`. M6 has to keep `Upgrade` and `Connection: upgrade` for upgrade requests.
- [ ] **Client IP use (M2).** The rate limiter and ip_hash must key on `ClientIP`, never on the raw X-Forwarded-For header.

## Exit criteria

M1 is done when these e2e tests pass under `go test -race`. They all pass. Each one asserts on the backend side.

1. **1 GB streaming with flat memory, both directions.**
   - The body comes from a generated `io.Reader`, never a 1 GB buffer.
   - The backend counts the bytes and discards them.
   - Heap growth measured with `runtime.MemStats` must stay under about 32 MB.
   - Skip the test under `-short`.
2. **SSE passthrough.** The backend writes event 1, flushes, and then blocks. The client must receive event 1 before the backend sends event 2.
3. **Cancellation reaches the backend.** The client cancels the request, and the backend's `r.Context().Done()` must fire within 1 s.
4. **Spoofed XFF is rejected.**
   - With an untrusted peer, `X-Forwarded-For: 1.2.3.4` never reaches the backend; the backend sees only `127.0.0.1`.
   - With `127.0.0.1` trusted, the chain is appended instead.
5. **Keep-alive reuse.** The backend counts `StateNew` in `ConnState`. After 100 sequential requests whose bodies are fully read, the count must be exactly 1.

## Conventions

- Claude writes code directly into the files; the user doesn't type it from chat. Plan each step in chat, implement one step at a time, and run `make lint test` after each.
- Edit in place and never overwrite the user's own work. Match the existing style: short doc comments that explain why, table-driven tests, and fuzz tests that check against a simple reference implementation.
- Every hot-path function gets a benchmark with `ReportAllocs`. Where it should be allocation-free, add a `testing.AllocsPerRun` check.
- Add each new fuzz target to the Makefile's `fuzz` target.
- Libraries are allowed only for YAML, Prometheus and OpenTelemetry. Write everything else by hand, and don't depend on `httputil.ReverseProxy`.
- Commit only when the user asks.
