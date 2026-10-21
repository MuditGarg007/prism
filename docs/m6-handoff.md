# M6 handoff

As of 2026-10-20, M6 (advanced routing and security) is complete: all 10 steps are done, and `make lint test validate` passes with `-race`. The exit tests in `test/e2e/m6_test.go` pass, three runs in a row. Nothing from M6 is committed yet; commit only when the user asks. Next is M7.

- The repo lives at `/home/mudit/prism`. The module is `github.com/MuditGarg007/prism`, on Go 1.27.
- M6 builds on `2a87a3a` (M5).
- Run `make lint test validate` to check the current state. `make fuzz` now has nine targets (`FuzzTraceparent` is new).
- Every `go` command prints `go: warning: ignoring go.mod in $GOPATH /home/mudit/go`, and `go mod tidy` fails because of that stray file. Add dependencies with `go get <pkg>@<version>`.
- The source plan is `docs/project-plan.md`, section "M6: Advanced routing and security, rest of Layer 5". Earlier state and decisions are in `docs/m1-handoff.md` through `docs/m5-handoff.md`.
- **New dependency:** `golang.org/x/crypto` v0.58.0, for `bcrypt` (and it bumped `golang.org/x/sys` to v0.49.0). The conventions below allowed libraries only for YAML, Prometheus and OpenTelemetry. The plan asked for bcrypt, and D4 named the dependency when the user accepted the plan. Writing bcrypt by hand would be worse, but the user may want to confirm it.

## Decisions

The user accepted these with "do steps 1 to 5 now and then write the handoff". The three open points took their recommended defaults:

- The rate limit runs before auth.
- Tracing is hand-written `traceparent` propagation, with no OpenTelemetry SDK.
- A WebSocket holds its route's concurrency slot for its whole life.

Don't reopen these unless asked.

| # | Decision | Choice |
| --- | --- | --- |
| D1 | Canary | Route `canary: {upstream, percent, header, cookie}`. Assignment is sticky: the client IP is hashed into 10,000 buckets, so raising `percent` only adds clients. A client with no known IP gets a random bucket. A header or cookie set to `always` or `never` overrides the split for one request (nginx-ingress semantics). Retries stay inside the chosen upstream. Canary requests skip the cache. Each canary has its own metric handles, labelled with the canary upstream. |
| D2 | Shadow | Route `shadow: {upstream, percent (100), max_body (64KB), max_concurrency (16), timeout (5s)}`. There is one copy per request that reaches a backend: per `fetch`, not per attempt, and never for cache hits. A copy runs on `WithoutCancel` plus its own timeout. Only bodies with a known length up to `max_body` are copied. A copy is dropped when the semaphore is full (`TryAcquire`). The response is discarded, but results still feed the shadow pool's breaker and outlier detection. Copies are never retried, and carry `X-Prism-Shadow: 1`. Results are counted in `prism_shadow_requests_total{route,result}`. |
| D3 | IP allow/deny | `access: {allow, deny}`, both at top level (checked before routing) and per route (checked after matching). Deny wins. If `allow` is not empty, the client must be in it. The client IP is the one after `trusted_proxies`. Refusals get 403, kind `forbidden`. |
| D4 | Basic auth | `auth.basic: {realm, users: {name: bcrypt}}`, via `golang.org/x/crypto/bcrypt`. Unknown users are checked against a dummy hash. Verified passwords are cached for 5 minutes. bcrypt checks run at most GOMAXPROCS at a time. Failures get 401 with `WWW-Authenticate`. |
| D5 | API keys | `auth.api_keys: {header (X-API-Key), keys: {name: "sha256:<hex>"}}`. Keys are looked up by digest. When the route has both auth types, either one is accepted. |
| D6 | Credentials and identity | Prism removes the credentials it checked (`forward_credentials: true` keeps them). The optional `identity_header` is set to the user or key name, replacing any copy the client sent. Secrets are hidden in `GET /config`. A request that passed Prism's auth is treated like one with `Authorization` when deciding whether to store its response. |
| D7 | WebSocket | Opt-in per route with `websocket: {idle_timeout}`. Hijack through a `proxy.Hijacker` interface on the access log's writer, plus `TrackHijacked`. The idle timer counts activity in both directions. The slot and backend inflight count are held for the connection's life. |
| D8 | HTTP/2 | Tests for h2 over TLS. Listener `h2c: true`. Backends stay HTTP/1.1. |
| D9 | Tracing | Hand-written W3C `traceparent` propagation, with `trace_id`/`span_id` in the access log and a fuzz target for the parser. |
| D10 | Reload carry-over | Kept when its config is equal: the shadow semaphore and the authenticator (with its verified-password cache). Admin `/routes` shows the new settings without secrets. |

The 10 steps:

1. Config
2. IP filter
3. Auth
4. Canary
5. Shadow
6. WebSocket
7. HTTP/2 and h2c
8. Tracing
9. Runtime and admin
10. Exit tests and docs

### Request pipeline now

```
access log → request ID → trace → global access (403) → path check → route match → route access (403)
  → rate limit (429) → auth (401) → body size (413) → WebSocket check → canary pick
  → cache (skipped for the canary and WebSockets) → concurrency slot
  → fetch: shadow decision (never for WebSockets), attempts, shadow copy
  → write, or tunnel on a 101
```

## What is built

### Step 1: config (`config/config.go`, `defaults.go`, `validate.go`)

- **Top level:** `access` (`*Access{Allow, Deny []Prefix}`).
- **Route:** `access`, `auth` (`*Auth{Basic, APIKeys, ForwardCredentials, IdentityHeader}`), `canary` (`*Canary{Upstream, Percent, Header, Cookie}`) and `shadow` (`*Shadow{Upstream, Percent, MaxBody, MaxConcurrency, Timeout}`).
- **Defaults:**
  - realm `prism`
  - key header `X-API-Key`
  - shadow `percent` 100, `max_body` 64KB, `max_concurrency` 16, `timeout` 5s

  The canary's `percent` has no default: 0 is valid and means "overrides only".
- **Validation:**
  - An `access` block with both lists empty is an error.
  - `auth` needs `basic`, `api_keys` or both.
  - The realm must not contain quotes, backslashes, CR, LF or NUL.
  - User names must not be empty or contain colons or control characters, and every hash must pass `bcrypt.Cost`.
  - Key digests must be `sha256:` followed by 64 hex digits, and the same digest under two names is an error.
  - Header names (key header, `identity_header`, the canary header) go through the new `checkHeaderName`: a token, not hop-by-hop, not Host.
  - `identity_header` may be neither `Authorization` nor the key header.
  - The canary and shadow upstreams must exist and differ from the route's own.
  - The canary's `percent` must be in [0, 100] (NaN fails); the shadow's must be in (0, 100].
- `config.ParseKeyDigest` is exported; runtime uses it.
- **Redaction:** a new tag, `redact:"values"`, hides a map's values but keeps its keys, so `GET /config` shows who has access but no hashes. `Users` and `Keys` carry it (`admin/config.go` `encodeField`).
- The config for `websocket` and `h2c` came with steps 6 and 7.
- The example (`examples/prism.yaml`) now has:
  - a top-level `access` block;
  - an `api-v2` upstream;
  - `auth`, `canary` and `shadow` on the api route, with comments. The example user is `alice` / `s3cret` (a real cost-10 hash), and the key is `key-one`.
- Tests: `TestParseSecurityAndSplits` (values and defaults), `TestParseSecurityAndSplitErrors` (26 rows plus an empty top-level access block), `TestParseKeyDigest`, and new rows in `admin.TestEncodeConfigRedact`.

### Step 2: IP filter (`internal/security/ipfilter.go`)

- `security.NewIPFilter(allow, deny []netip.Prefix)` and `Allowed(ip)`. Deny wins; a non-empty allow list must match. The address is unmapped first. An invalid IP (an unknown client) matches no prefix, so it passes a deny list but fails an allow list.
- Lookup is a linear scan: 126 ns with 0 allocations for 33 prefixes (about 4 ns a prefix). A trie would only pay off with thousands of prefixes.
- `proxy.NewHandler(table, fwd, access, unmatched, log)` takes the global filter. It is checked right after the request ID, before path validation and routing, so a denied client gets 403 even on paths that match no route. `Route.Access` is checked right after the match, before the rate limit. Both use `reject` with the new kind `forbidden` (403, access log only).
- runtime: `ipFilter(cfg)` and a `prefixes` helper, which `trusted_proxies` now uses too.
- Tests: `security.TestIPFilter` (12 rows), `proxy.TestProxyAccess`, and `runtime.TestBuildSecurityAndSplits`.

### Step 3: auth (`internal/security/auth.go`)

- `security.NewAuthenticator(AuthConfig{Realm, Users, KeyHeader, Keys map[[32]byte]string, Forward, IdentityHeader})`.
- `Check(r)` returns the identity, `ErrUnauthorized`, or the context's error when the client leaves while waiting for a bcrypt slot.
  - **API key path:** the key header must have exactly one value. When the header is `Authorization`, the value must be `Bearer <key>`, with the scheme matched case-insensitively. The key is looked up by `sha256.Sum256`. Timing can only leak information about the digest.
  - **When the client sends a key and a password:** a key that is present but wrong refuses the request even if the Basic credentials are good. A `Basic` value in `Authorization` falls through to the password check, so Basic and Bearer can share the header.
  - **Password path:** if the user is known and `sha256(user + "\x00" + pass)` is in the verified cache, against the same hash and within the TTL, it is a hit. Otherwise Prism waits for a slot (a channel of `GOMAXPROCS`) and runs bcrypt. Unknown users run against `dummyHash(cost)`, made lazily per cost in a `sync.Map`, at the highest cost among the users. Only successes are cached: 1,024 entries; when full, expired ones are swept and then an arbitrary one is dropped.
- `Challenge()`:
  - `Basic realm="…", charset="UTF-8"` when users are configured;
  - `Bearer realm="…"` for keys in `Authorization`;
  - nothing for a custom key header.
- `Prepare(h, user)` runs on the outgoing header in `proxy.outgoing`, after the forwarding headers and request ID and before the route's `request_set`/`request_remove` rules. It deletes `Authorization` (when Basic is configured) and the key header unless `Forward` is set, and sets the identity header to exactly one value.
- **proxy:**
  - `Route.Auth` is checked after the rate limit and before the body check.
  - A refusal sends `WWW-Authenticate`, then `reject(401, kind unauthorized)`. A client that leaves while waiting is answered by `fail`, which logs 499.
  - `Annotations.User` is in the access log as `user`.
  - `outgoing(r, rt, b, rb, an)` now takes the annotations instead of the request ID.
- **Cache:** `cache.Storable(authenticated bool, …)` replaces its `req http.Header` argument. `proxy.authenticated(r, an)` is `an.User != "" || Authorization present`, so an authenticated response is stored only when it is `public`, `s-maxage` or `must-revalidate`. Auth runs before the cache, so a hit always needs credentials.
- **runtime:** `authenticator(cfg)` builds it. A route keeps its `*Authenticator`, verified cache included, when `reflect.DeepEqual` finds the old and new `auth` configs equal. Any change, such as a removed user, gets a new one and an empty cache.
- **Performance:** a remembered password takes 188 ns with 2 allocations, in parallel. A bcrypt miss takes about 50 ms at cost 10.
- **Tests:**
  - `security.TestAuthenticatorCheck` (14 rows);
  - `TestAuthenticatorTwoKeyValues`;
  - `TestAuthenticatorRemembers` (a hit needs no slot; a wrong password waits for one; TTL; a changed hash);
  - `TestAuthenticatorCacheCap`;
  - `TestAuthenticatorPrepare`;
  - `proxy.TestProxyAuth` (forward on and off: 401 with a challenge, the identity header replaced, credentials stripped or kept);
  - `TestProxyAuthCache` (mutation-checked against dropping `an.User` from `authenticated`);
  - an auth row in `runtime.TestReloadKeepsRouteState`.

### Step 4: canary (`proxy/canary.go`)

- `proxy.Canary{Pool, Buckets, Header, Cookie, Metrics}`, where `Buckets` is `round(percent × 100)` out of 10,000.
- `pick(r, client)`:
  - The header beats the cookie, and only `always`/`never` (case-insensitive) count.
  - Otherwise `canaryBucket(ip)` (FNV-1a over `As16` of the unmapped address, plus the splitmix64 finalizer, unseeded) is compared with `Buckets`.
  - A salt keeps the split independent of ip_hash.
  - The hash ignores the route, so a client in one route's canary is in every route's canary with an equal or larger share.
- **New `Annotations.Pool`:** the upstream actually used. It is set at the match and replaced by the canary's. `fetch`, `retryTarget`, `logError` and the access log's `upstream` attribute read it instead of `Route.Pool`, and `an.Metrics` switches to the canary's handles.
- The canary is picked after the body check and before the cache. A canary request never looks in or fills the cache, so it gets no `X-Cache` header.
- runtime: `route.Canary` is built per route, and the header name is canonicalized. `Runtime.series()` includes the `(route, canary upstream)` series, so `Prune` keeps them.
- Performance: 74 ns, with 1 allocation from `r.Cookie` when a cookie is configured and the request has cookies.
- Tests:
  - `TestCanaryBucketSpread`: 100,000 distinct IPs give a 5% and a 10% share within ±0.5 points, every 5% client stays in at 10%, 0% and 100% are exact, and a mapped address hashes the same as its plain form.
  - `TestCanaryOverride` (10 rows).
  - `TestProxyCanary`: the canary is neither served from the cache nor stored in it, and the annotated pool is the canary's.
  - `runtime.TestBuildSecurityAndSplits`.

### Step 5: shadow (`proxy/shadow.go`)

- `proxy.Shadow{Pool, Buckets, MaxBody, Slots *limits.Semaphore, Timeout, Metrics}`. `ShadowHeader = "X-Prism-Shadow"`.
- **In `fetch`, after `newRequestBody`:** `shadowFor` samples at random against `Buckets`. If the request is sampled and its `ContentLength` is -1 (unknown) or over `MaxBody`, it counts `body_skipped`.
  - A body that is empty or already buffered (a retry route with a body of at most 64 KiB) is copied at once, before the first attempt.
  - A streamed body is captured as the transport reads it: `readCounter.capture(n)`, and `keep` appends until n bytes have arrived, then publishes the slice through `atomic.Pointer`, after which nothing writes to it. A deferred `sendShadow` runs when `fetch` returns. A body the backend had not finished reading by then is `body_skipped`, because waiting would delay the response.
- **`sendShadow`:**
  - It runs `TryAcquire`; a full semaphore counts `busy`.
  - It picks from the shadow pool, which goes through breaker admission like a real request. No backend counts as `error`.
  - It builds the request synchronously with `outgoing`, so the route's header rules and auth `Prepare` apply, and the request is never read after its handler returns. It adds the shadow header, sets `Trailer = nil`, and uses the context `WithTimeout(WithoutCancel(r.Context()), Timeout)`.
  - It then runs `roundTripShadow` on a goroutine. That function drains the response body, reports `Success`/`Failure` to the shadow backend (5xx, a broken body and transport errors are failures), observes the backend latency, and returns `sent` or `error`.
- **Metrics:** `observability.Metrics.Shadow(route)` gives a `*ShadowMetrics` with the four result counters resolved in advance (nil-safe). `Series.Shadows` lets `Prune` delete the shadow series of a route that stops shadowing or is removed.
- **runtime:** `route.Shadow` is built per route. The semaphore is carried over when `max_concurrency` is equal, so copies still running on the old runtime keep counting against the cap.
- **Tests:**
  - `TestProxyShadowCopies`: GET and POST, with and without retries, so both the buffered and the captured path are covered. The copy has the same method, path and body plus the marker, and its 500 is thrown away.
  - `TestProxyShadowSkipsBodies`: a body over the limit and a chunked body.
  - **`TestProxyShadowAddsNoLatency`, the explicit test the plan asks for.** The shadow backend stalls for 3 s. Ten POSTs, each with a 2 s client deadline that is canceled right after, must all finish within 500 ms. Exactly 8 copies are `busy` and 2 stay in flight with their slots held, and both come back `sent` once released, so a client leaving did not cancel them. It was mutation-checked: making `sendShadow` synchronous fails it.
  - `TestProxyShadowTimeout` (`error`, and the slot is released).
  - `TestShadowCapture`.
  - `TestShadowSample`.
  - `observability.TestShadowMetrics` (including the prune).
  - A shadow row in `runtime.TestReloadKeepsRouteState`.
- `BenchmarkProxy` is unchanged at 154 allocations per request, before and after M6 so far.

### Step 6: WebSocket (`proxy/websocket.go`)

- **Config:** route `websocket: {idle_timeout}`, default **5m** (Envoy's stream idle default; nginx uses 60s). It must be positive. runtime sets `proxy.Route.WebSocketIdle`, where 0 means the route does not upgrade.
- **Detection:** `isWebSocket(r)` is a GET over HTTP/1.1 with `upgrade` in `Connection` and `websocket` in `Upgrade`, both as case-insensitive tokens. `hasToken` in `headers.go` now also backs `wantsTrailers`. `an.WebSocket` is set after the body check, only on a route with `WebSocketIdle`. On any other route the upgrade headers are dropped like every hop-by-hop header, so the backend sees a plain GET (the old behavior).
- **Forwarding:** `outgoing` sets `Connection: Upgrade` and `Upgrade: websocket` again for `an.WebSocket`. Retries work as for any GET. The cache and the shadow skip WebSockets (`!an.WebSocket` in `ServeHTTP` and in `fetch`).
- **The 101:** `fetch` keeps the backend's `Upgrade` value in `fetched.upgrade` before stripping hop-by-hop headers. A 101 to a request that did not ask for one is `errBadUpgrade` right there (`Overturn`, 502 `upstream`), so neither `write` nor the cache path ever sees a 101. `forward` hands a 101 to `tunnel`, which refuses a body that is not an `io.ReadWriteCloser` or a protocol other than `websocket` the same way.
- **`tunnel`:**
  - It hijacks through `proxy.Hijacker` (`HijackTracked() (conn, brw, done, err)`), falling back to `http.ResponseController` untracked. A failed hijack is 500, kind `internal` (new).
  - It writes the 101 by hand, as `httputil.ReverseProxy` does: the request ID from `w.Header()`, the backend's headers after the route's response rules, then `Connection: Upgrade` and `Upgrade: websocket`.
  - Bytes the client sent right behind its request, already in `brw.Reader`, go first.
  - `pipe` copies each direction through the shared 32 KB buffers. Either direction ending closes both, as in `ReverseProxy`.
  - `idleTimer`: a touch only stores the time (atomic). The `time.AfterFunc` timer checks when it fires and sleeps again for the rest. A mutex guards the timer field and the stopped flag. The test for it uses the real clock (50 ms), not a fake one, since it times real connections.
  - The slot and the backend's inflight count are released by the existing defers in `ServeHTTP` and `forward`, so they last as long as the connection.
- **Annotations:** `WebSocket`, `Upgraded`, `Tunnel` (how long it stayed open), `BytesUp`, `BytesDown`.
- **server:** `logWriter.HijackTracked` hijacks, sets the logged status to 101 and calls `track` (`Server.TrackHijacked`, passed into `accessLog`). The access log adds `websocket=true bytes_up=… bytes_down=…`. The request metrics use `d - an.Tunnel`, so only the handshake reaches the latency histogram; the access log `duration` is the whole life.
- **Tests:** `TestProxyWebSocketEcho` (headers both ways, early bytes, 100 KB echo, tracking, inflight held and released), `TestProxyWebSocketIdle` (client-only and backend-only traffic keep it open; silence closes it after the timeout), `TestProxyWebSocketRefused` (no websocket block, an unsolicited 101, an `h2c` 101), `TestProxyWebSocketHoldsSlot` (503 for a second request while open; no cache or shadow use, mutation-checked for the shadow), `TestIsWebSocket`, `TestIdleTimer`, and `server.TestServerWebSocket` (access log fields; Shutdown closes it at the deadline).

### Step 7: HTTP/2 and h2c (`server/server.go`, `proxy/h2_test.go`)

- **Config:** listener `h2c: bool`, an error on a TLS listener. `sameListener` compares it, so changing it needs a restart (warned).
- **server:** an h2c listener gets `http.Protocols` with HTTP/1 and unencrypted HTTP/2. That is prior knowledge only: Go has no `Upgrade: h2c`.
- **Port 0:** two listeners on `…:0` no longer count as duplicates, since each gets its own port. The e2e test needed it.
- **Tests:** `proxy.TestProxyHTTP2` over TLS (a flushed chunk arrives before the rest, trailers with `TE: trailers`, a streamed upload of unknown length, and a cancel that reaches the backend and logs `client_closed`); `server.TestServerH2C` (h2c and HTTP/1 on one listener, and a plain listener refusing prior knowledge).

### Step 8: tracing (`proxy/trace.go`)

- `proxy.Trace{ID, Parent, SpanID, Flags}` on `Annotations.Trace`, set by `newTrace(r.Header)` right after the request ID.
- **Join or start:** exactly one valid `traceparent` is joined: same trace ID and flags, a new span ID, and its parent ID kept as `Parent`. Anything else starts a trace with flags `01` (sampled, like OpenTelemetry's default sampler for a root span).
- **Deviation from the step's note,** flagged for the user: the note said "a new span ID per outgoing request". Instead there is one span per incoming request, and every request Prism sends for it (each attempt and the shadow copy) names that span as its parent. With no span exporter, per-attempt span IDs would be parents that appear nowhere; this way the parent a backend sees is the `span_id` in Prism's access log.
- **Trust:** the client's `traceparent` is accepted from anyone, unlike `X-Request-Id`, because it decides nothing in Prism and joining traces from browsers and apps is the point. Flagged below.
- `propagate` runs in `outgoing` after the request ID and before the route's rules, so `request_set` can still override it. `tracestate` goes on only with a joined trace; it is removed when Prism starts one.
- `parseTraceparent` follows the spec: lowercase hex only, version `ff` invalid, all-zero IDs invalid, version 00 exactly 55 characters, later versions may append `-…` fields. Prism always sends version 00.
- The access log adds `trace_id`, `span_id` and, when joined, `parent_id`.
- **Performance:** `newTrace` plus formatting takes 100–125 ns with 1 allocation. `BenchmarkProxy` went from 154 to **158** allocations per request (the header value and its slice, and the header map growing); M7 can look at it.
- **Tests:** `TestParseTraceparent` (15 rows), `FuzzTraceparent` (against a regexp of the spec's rules, plus a format round trip; added to `make fuzz`), `TestNewTrace`, `TestProxyTrace` (joined, none and malformed; the shadow copy carries the same header), `BenchmarkNewTrace`, and new fields in `server.TestAccessLogProxied`.

### Step 9: runtime and admin (`admin/state.go`, `admin/metrics.go`, `runtime/runtime.go`)

- `GET /routes` adds, per route: `access` (the allow and deny prefixes), `auth` (`["basic", "api_keys"]`, no names or hashes), `canary` (upstream, percent, header, cookie), `shadow` (upstream, percent, `in_flight`, `limit`) and `websocket` (`idle_timeout`).
- New gauge `prism_shadow_inflight{route}`.
- **Open item addressed in part:** Build now warns for a route with `auth.basic` and no `rate_limit` ("guessing is limited only by bcrypt"). There is still no separate failed-auth limiter.
- **Tests:** `TestRoutes` and `TestMetrics` with the new fields and gauge, `TestConfig` checks that no `$2a$` hash or `sha256:` digest appears but the user and key names do, and `runtime.TestBuildBasicAuthWarning`.

### Step 10: exit tests and docs (`test/e2e/m6_test.go`)

- `writeConfig` writes a body that starts with `listeners:` as it is, for the HTTP/2 test.
- `TestCanarySplit`: 2,000 client IPs through a 20% canary land within 17–23%, clients stay on their side, and the header beats the cookie.
- `TestShadowAddsNoLatency`: 20 POSTs with a 1 s shadow backend finish in under 500 ms, and exactly 2 copies arrive (`max_concurrency: 2`).
- `TestAuthMatrix`: 9 cases — none, password, wrong password, unknown user, key, wrong key, wrong key with a good password, a spoofed identity header, and a denied address. The backend never sees credentials and gets the right identity.
- `TestWebSocket`: echo, the idle cutoff (200 ms), the access log line, and a Shutdown that waits until its deadline while the WebSocket is busy, then closes it.
- `TestHTTP2`: HTTP/2 over TLS (a generated certificate) and h2c, to an HTTP/1.1 backend, with a flushed body, a trailer and the trace context.
- The example has `h2c: true`, a `/live/` WebSocket route and a note on `traceparent`. `docs/project-plan.md` has the M6 status.

## Next steps

M7 (benchmarks and write-up), per `docs/project-plan.md`. The 4 new allocations per request from tracing are a candidate for its allocation work.

## Open items

- [ ] **Dependency check:** `golang.org/x/crypto` (see above).
- [ ] **No 401 rate limit of its own.** Password guessing is limited only by the route's `rate_limit`, if one is set. Build now warns about a basic auth route without one (step 9). A separate failed-auth limiter is still open.
- [ ] **traceparent is accepted from anyone** (step 8). If the user wants it to follow the `X-Request-Id` policy (trusted peers only), it is a one-line check in `newTrace`.
- [ ] **One span per request, not per attempt** (step 8 deviation, see above).
- [ ] **WebSocket idle default is 5m.** nginx uses 60s; change `defaultWebSocketIdleTimeout` if preferred.
- [ ] **No WebSocket close frame at shutdown.** At the deadline the TCP connection is closed; Prism does not send a 1001 "going away" frame, since it does not parse frames.
- [ ] **WebSockets over HTTP/2 (RFC 8441, extended CONNECT) are not supported.** Only HTTP/1.1 clients can upgrade.
- [ ] **The shadow sends nothing when the backend answers before reading the whole body.** It counts as `body_skipped`, which is the intended trade-off.
- [ ] **Canary with `ip_hash` on both upstreams.** Fine, because the salts differ, but it's untested end to end.
- [ ] **The canary hash ignores the route.** That is deliberate: one client gets one version across routes. If per-route independence is ever wanted, mix the route name into the salt.
- [ ] Carried from M5: health streaks restart on reload, coalesced SIGHUPs, `go mod tidy`, readiness on the admin port, **admin authentication** (still none; `/config` now hides the hashes and keys, but shows user and key names), and per-request metric cost.
- [ ] Unchanged from `docs/m4-handoff.md`: the leader's undeadlined drain, `stale-while-revalidate`, ip_hash spread and the rate-limit key cap.

## Conventions

- Claude writes code directly into the files; the user doesn't type it from chat. Plan each step in chat, implement one step at a time, and run `make lint test` after each.
- Edit in place and never overwrite the user's own work. Match the existing style: short doc comments that explain why, table-driven tests, and fuzz tests against a simple reference.
- Time-based logic takes an injectable `now`, and tests run on a fake clock.
- Every hot-path function gets a benchmark with `ReportAllocs`.
- Add each new fuzz target to the Makefile's `fuzz` target.
- Libraries are allowed only for YAML, Prometheus and OpenTelemetry, plus `golang.org/x/crypto/bcrypt` as of M6 (pending the user's confirmation). OpenTelemetry stayed unused: tracing is hand-written.
- Commit only when the user asks.
