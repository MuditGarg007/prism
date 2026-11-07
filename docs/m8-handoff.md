# M8 handoff

As of 2026-10-27, M8 (hardening, closing the gaps) is complete: all 10 steps are done, and `make lint test` passes with `-race`. Nothing is committed: the working tree holds all of M8 and its plan in `docs/project-plan.md`. Commit only when the user asks.

- The repo lives at `/home/mudit/prism`. The module is `github.com/MuditGarg007/prism`, on Go 1.27.
- M8 builds on M7 (`83d8865`). `docs/m7-handoff.md` describes M7. The plan is in `docs/project-plan.md`, section "M8: Hardening, closing the gaps".
- Run `make lint test` to check the current state.
- The stray `/home/mudit/go/go.mod`, which made every `go` command warn and broke `go mod tidy`, was moved to `/home/mudit/go/go.mod.bak` with the user's OK. `go mod tidy` now works, and `go.mod` marks its four direct dependencies (client_golang, client_model, yaml and x/crypto) as direct.

## Decisions

The user said "keep all decisions to recommended approach". Don't reopen these unless asked.

| # | Decision | Choice |
| --- | --- | --- |
| D1 | Dependencies | `golang.org/x/crypto` (bcrypt), and vegeta and benchstat as `go tool`s, are confirmed. No new libraries in M8. |
| D2 | Access log | Unbuffered by default. The opt-in `-log-buffer <size>` flag adds buffering. This is the same trade-off as nginx's `buffer=`: whoever turns it on accepts that a crash can lose up to a second of lines. |
| D3 | WebSocket idle default | It stays at 5 minutes. nginx's 60 s closes quiet sockets that send no pings, and `websocket.idle_timeout` already overrides it per route. |
| D4 | `traceparent` | Still accepted from any client. It decides nothing in Prism, and joining a trace that starts in a browser is the point. |
| D5 | Failed-auth limiter | `auth.failure_limit` is a rate or `off`, and defaults to `10/m` with a burst of its count. It counts only wrong credentials, never missing ones, per client IP and per /64 for IPv6. A client over the limit gets 429 with `Retry-After`. |
| D6 | Admin auth | `admin.auth` is the same `Auth` block as a route's. A non-loopback `admin.listen` without it is a validation error (it was a warning). `forward_credentials` and `identity_header` are errors under `admin.auth`, since they do nothing there. Admin auth needs a restart to change, like the rest of `admin`. |
| D7 | Admin probes | The server answers `GET /ready` and `GET /live` ahead of the admin handler, and so ahead of auth. `/ready` uses the same draining flag as `ready_path`. |
| D8 | Refusal metrics | No new metric family. A limited request is `prism_proxy_errors_total{kind="auth_limited"}` and shows `error=auth_limited` in the access log. 401s stay `kind="unauthorized"`, as before. |
| D9 | Health record | The shared record holds the next check's due time, not the last check's time. That also covers a reload storm before the first check, which a "last check" record cannot, and keeps the ±10% jitter. Taken during step 7. |
| D10 | ip_hash points | 4,096 points per unit of weight, up from Ketama's 160, with the 65,536-point cap unchanged. 10 equal backends are within ±5%; a lookup costs about 12 ns more (37 ns, up from 25 ns). Upgrading remaps most `ip_hash` clients once, and the README says so. |
| D11 | Eviction metric | `prism_ratelimit_evictions_total{route, limit}`, with `limit` either `rate_limit` or `auth_failures`, and `route="admin"` for the admin API's failure limiter. It is read from an atomic counter at scrape time, like the other gauges, so it costs nothing until a shard is full. |
| D12 | GC tuning | `GOGC=400` is recommended for proxy-only deployments in the README, not set by Prism. Cache entries are live heap, so a fixed `GOGC=400` would let a cache-heavy process grow to five times its live size before collecting. `GOMEMLIMIT` with a higher `GOGC` is the safer way to apply it with a cache. |
| D13 | Log buffer | Two buffers: the full one is written out after the lock is released, while the other fills. Taken during step 9, when the single buffer turned out to stall requests (below). |

## What is built

### Step 1: `-log-buffer` (`observability/buffer.go`, `cmd/prism/main.go`)

- `observability.BufferedWriter` flushes when the next line wouldn't fit, every `FlushInterval` (1 s) and at `Close`. A line is never split across two writes, and a line bigger than the whole buffer goes straight through. The first write error sticks and is returned from then on. `Close` is idempotent (`sync.Once`) and doesn't close the underlying writer.
- `-log-buffer` takes a size such as `64KB` (via `config.ParseByteSize`), from 0 to 64MB; 0 is the default and means off. When it is set, `run` swaps `stderr` for the buffer, so the `prism: ...` error lines keep their place among the log lines. A deferred `Close` flushes at exit.
- `BenchmarkBufferedWriter`: 319 ns/op in parallel with 200-byte lines, 0 allocs. (Reworked in step 9; see there.)
- `TestRunLogBuffer` checks the flag's values and that an error reported through the buffer still reaches stderr at exit. The README describes the flag.

### Step 2: failed-auth limiter (`ratelimit`, `security/auth.go`, `config`, `proxy/proxy.go`)

- `ratelimit.Limiter.Peek` answers what `Allow` would, but spends nothing and stores no key. `FuzzGCRA` now checks `Peek` against the reference token bucket before each `Allow`.
- `config.OptionalRate` is a `Rate` that may also be written `off`, with `Set()` and a `MarshalText` that `GET /config` uses. `Auth.FailureLimit` defaults to `10/m` in `defaultAuth`, which routes and the admin block now share.
- `security.AuthConfig` gains `FailureCount` and `FailurePer`. `Check(r, client)` now takes the client IP. It first runs `Peek` and returns a `*LimitedError{RetryAfter}`. On a wrong key or password (the internal `errWrong`, which wraps `ErrUnauthorized`), it calls `Allow` to record the failure. A password check runs `Peek` again after it gets its bcrypt slot, so guesses that queued while earlier ones failed are refused without a bcrypt run. `failureKey` unmaps IPv4-mapped addresses and masks IPv6 to its /64.
- The limiter lives inside the `Authenticator`, so it survives a reload whenever the auth block is unchanged (the existing `reflect.DeepEqual` check).
- `proxy` maps `*security.LimitedError` to `reject(429, kindAuthLimited, RetryAfter)`. The route's own `rate_limit` still runs first.
- `runtime` warns only about a Basic auth route that has `failure_limit: off` and no `rate_limit`. Before, it warned whenever a Basic route had no `rate_limit`.
- Tests: `TestAuthenticatorFailureLimit` (missing credentials are free, the right key is refused once over the limit, other clients are unaffected), `TestFailureKey`, `TestAuthenticatorFailureLimitAfterQueue` (the check after the slot), `TestPeekSpendsNothing`, `TestProxyAuthFailureLimit` (keyed by the forwarded client IP, `Retry-After: 30`), `TestParseAuthFailureLimit`, and a `TestBuildBasicAuthWarning` rewritten as a table.

### Step 3: admin auth (`admin/admin.go`, `config/validate.go`, `runtime`)

- `config.Admin.Auth`. `validate` adds `admin.auth` checks through `validateAuth`, and the loopback check moved there from `runtime` (`config.loopback`). An address that is not loopback (`:9901`, `0.0.0.0`, `[::]` or a LAN IP) without auth is an error.
- `runtime.NewAuthenticator` (exported; it was `authenticator`) builds both route and admin authenticators. `admin.New` builds one from the config at New, as it does for `pprof`.
- `Handler.ServeHTTP` runs `authorize` before the mux. The client is the TCP peer; `X-Forwarded-For` is not trusted on the admin port. The answers are JSON errors: 401 with `WWW-Authenticate` when there is a challenge, or 429 with `Retry-After`. A client that leaves while waiting for a bcrypt slot gets nothing. Each 401 is logged at Info as "admin request refused", with client, method and path.
- `keepRestartOnly` compares `admin` with `reflect.DeepEqual`, because `Admin` now holds a pointer; `TestReloaderAdminAuth` covers this. `TestBuildAdminWarning` was removed in favour of `TestAdminListen` and the new cases in `TestParseErrors`.
- `TestAuth` (admin) covers Basic and bearer access, a 401 with a challenge and `no-store`, `POST /reload` with a key, and the failure limit.

### Step 4: admin probes (`server/server.go`)

- `Server.Admin` wraps the admin handler in `readiness("/ready", liveness("/live", h))`. Both skip the access log, like the rest of the admin listener. `/ready` turns 503 at `Drain`; `/live` stays 200 until the listener closes.
- `TestServerAdmin` checks both probes before and after `Drain`, and that `/ready/x` still reaches the handler.

### Step 5: bounded cache drain (`proxy/cache.go`)

- `leaderContext(parent, gone, d)` replaces the bare `context.WithoutCancel` for a Flight leader's fetch. It keeps `parent`'s values and is canceled with the cause `errDrainTimeout` `d` (the route's `lock_timeout`) after `gone` ends. For a leader, `gone` is the client's context, so the bound runs from when the client leaves, whether the leader is then blocked writing to it or draining the rest of the body.
- When the fill is abandoned, nothing is stored, the deferred `Done(Failed)` releases any waiters (who fetch for themselves, as at their own lock timeout), and the concurrency slot is released. A "response not stored" Debug line gives `reason=drain_timeout`.
- An abandoned body does not overturn the backend's success, in the drain or in `write` (`abandoned(resp)` checks the cause on `resp.Request`'s context): a slow body is not a failed one, as it would not be with the client still there.
- `TestCacheLeaderDrainTimeout` stalls a body after 5 of 10 bytes, cancels the client, and checks that the backend sees the cancellation no sooner than `lock_timeout`, that the slot comes free, and that only the later response is stored. With the bound removed it fails after 10 s.

### Step 6: `stale-while-revalidate` (`proxy/cache.go`, `config`, `cache/stats.go`, `admin`)

- `cache.stale_while_revalidate` (a duration, default 0 = off, negative is a validation error) becomes `proxy.Cache.StaleWhileRevalidate`. `staleOK` and the new `updateOK` share `staleWithin`: the route's window, narrowed by the response's directive, and refused by `must-revalidate`, `no-cache` and `s-maxage`.
- In `serveCache`, a GET whose entry is stale but within the window joins the Flight as before. The leader calls `revalidateLater` and answers `UPDATING` at once; a request that gets `Wait` answers `UPDATING` without waiting. `Pass` and HEAD keep their old paths.
- `revalidateLater` clones the request with a detached context, copies the annotations it needs (route, pool, client IP, request ID, trace, user), and runs `fill` on a goroutine with `background: true`, a `backgroundWriter` that discards, a route copy without `Shadow` (it is not client traffic), and a cache copy without `Stats` (only the client's `UPDATING` is counted). A deferred `recover` swallows `http.ErrAbortHandler` from `abort`, and only that.
- In `fill`, a background fetch takes a slot with `TryAcquire` and gives up without one, never serves the stale entry on failure, and is bounded like a leader whose client left: `gone` is a context canceled when the headers arrive, so the body gets `lock_timeout`. Header waits are bounded by the upstream's own timeouts. A 304 refreshes the entry, a new response replaces it, and a failure keeps it and frees the key for the next request to try.
- `Stats.Updating`, `/cache/stats` `"updating"`, and `prism_cache_requests_total{result="UPDATING"}`.
- Tests: `TestCacheStaleWhileRevalidate` (the window table, mirroring `TestCacheStaleIfError`, plus the stats), `TestCacheStaleWhileRevalidateOnce` (100 concurrent requests all `UPDATING`, none waiting, one 304 from the backend), `TestCacheStaleWhileRevalidateFails` (5xx, body cut off, body stalled), `TestCacheStaleWhileRevalidateNoSlot` (no queueing; checked by mutation against `acquire`).
- README (features, a "never queues" decision, the not-supported line) and `examples/prism.yaml` (`stale_while_revalidate: 10s`) updated.

### Step 7: health state across reloads (`upstream`, `health`)

- `upstream.HealthRecord{Streak, Due}` lives in the backend's shared `state` behind a mutex, with `HealthRecord` and `SetHealthRecord`. `Pool.Commit` clears it along with marking the backend healthy when the new config has no health check.
- `Checker.run` takes `Due` from the record; only when it is zero does it pick the random first offset, and it writes that back, so even a storm before the first check keeps one schedule. `until(due)` caps the wait at one interval, for a reload that shortened it. When the timer fires and the record's `Due` has moved, the replaced checker checked in the overlap, so it waits for the new `Due` instead of checking twice.
- `record` seeds the tracker from `Healthy()` and the stored streak, and stores the new streak and `Due = now + next()`. The caller logs a flip before `SetHealthy`, so whoever sees the change can find the log line (`TestCheckerFlips` was flaky with the old order once `record` did more work).
- `Checker.now` is injectable.
- Tests: `TestCheckerContinuesRecord` (a streak of 2 plus one failure marks down; the first check comes at `Due`, not a random hour away), `TestCheckerReloadStorm` (checker replaced every 5 ms against a 20 ms interval, as the reloader does it; the backend still goes down after at least `unhealthy_after` checks), and the record cases added to the `NewPool` carry-over table. Both health tests fail with the record ignored.
- README: the two "known limits" this fixes are gone, and the reload decision mentions the check schedule.

### Step 8: ip_hash spread and rate-limit evictions (`balancer`, `ratelimit`, `security`, `admin`)

- `pointsPerWeight` is 4,096 (D10). `TestIPHashSpread` now holds every case to ±5%. `BenchmarkIPHash` went from 25 to 37 ns, because the 1 MiB ring fits less well in cache.
- `ratelimit.Limiter` counts forced evictions in an `atomic.Uint64`: `makeRoom` reports whether it deleted from a full shard, and `Allow` adds one when it did. `Evictions()` reads it. The idle sweep doesn't count, since forgetting an idle key changes nothing.
- `security.Authenticator.FailureEvictions() (uint64, bool)` exposes the failure limiter's count, and false when `failure_limit` is off.
- The admin `collector` holds the admin authenticator as well as the runtime, and emits `prism_ratelimit_evictions_total` for each route's `rate_limit`, each route's failure limiter, and `route="admin", limit="auth_failures"` (D11).
- Tests: `TestFloodIsBounded` checks that evictions equal the keys that didn't fit; `TestIdleKeysAreSwept` that sweeping counts none; `TestMetrics` the two series for a route with both limits and none for a route with neither; and admin `TestAuth` the admin series over an authenticated `GET /metrics`.
- `TestCanaryIPHash` (e2e): 4 + 4 ip_hash backends with a 20% canary and 4,000 client IPs. The canary share is within 17–23%, each backend's share of its side within ±25%, and every 20th client is sent again and must land on the same backend. The salt is not what keeps the two hashes independent: with 4,096 points per backend the ring is dense enough that even a split correlated with the ring would spread evenly. The test comment says so.

### Step 9: latency tail

See [Step 9 findings](#step-9-findings) below.

### Step 10: exit tests and wrap-up

The M8 exit criteria, each with an e2e test against the real stack (`test/e2e/m8_test.go`) or the real binary's `run` (`cmd/prism/main_test.go`):

| Exit criterion | Test | What it checks |
| --- | --- | --- |
| A burst of wrong passwords gets 429, bcrypt work stays flat | `TestAuthFailureBurst` | `failure_limit: 5/m` on a Basic route. Five wrong passwords get 401; the next 100, from 10 workers, all get 429 with `Retry-After`, and use less process CPU (`getrusage`) than the five bcrypt runs did (72 ms against 598 ms; under `-race`, 165 ms against 7.3 s). The right password is then refused too, and another client IP gets 200. |
| The admin API refuses requests without credentials | `TestAdminAuth` | Every endpoint, with no key, a wrong key and the right one. A broken config is written first, so a refused `POST /reload` is seen not to have run. |
| A non-loopback admin listener without auth fails `-validate` | `TestRunValidateAdminAuth` | `0.0.0.0:9901` without `admin.auth` exits 1 naming `admin.auth`; with `api_keys` it exits 0. |
| `/ready` flips to 503 at shutdown | `TestAdminReadyAtShutdown` | After `Drain`, admin `/ready` is 503, `/live` 200, and the proxy still answers. |
| A stale entry within the window is served at once, with one background request | `TestCacheStaleWhileRevalidate` | `max-age=2` and a 600 ms backend. Once stale, 50 concurrent requests all get `UPDATING` with the old body within 300 ms, the stats count 50, then a `HIT` has the new body, and the backend saw exactly 2 requests. |
| A leader whose client left gives up on a stalled body after `lock_timeout` | `TestCacheLeaderDrainTimeout` | The backend sends 5 of 10 bytes and stalls; the client leaves. The backend's request is canceled between `lock_timeout` and `lock_timeout` + 2 s after that, and the next request is a `MISS` with the full body. |
| A failing backend is marked down under a reload every 0.25 s | `TestHealthReloadStorm` | 300 ms checks with `unhealthy_after: 3`, and a reload every 250 ms that changes a response header. The backend is marked down after at least 3 checks, about 0.7–0.9 s after it starts failing. |
| The ip_hash spread is within ±5% | `TestIPHashSpread` | Unit test, from step 8. |

- Two test fixes along the way. The storm test's shutdown hung for 5 s: a shared `http.DefaultClient` left a dialed but unused connection, which `Shutdown` waits on. It now uses the suite's `newClient`, whose cleanup closes idle connections. The SWR test first used `max-age=1` with a 1 s backend, which makes the new entry stale on arrival, because RFC 9111's age counts the response delay.
- README: "what's not supported" now lists the out-of-scope items of the plan; the ip_hash, rate-limit, `-log-buffer` and benchmark text are updated; the history says nine milestones.
- `go mod tidy`, after moving `~/go/go.mod` (see the top).

### Checked by hand

The real binary, with `admin.listen: 0.0.0.0` and `failure_limit: 2/m`, gave: `/live` 200, `/ready` 200, `/routes` without a key 401, with the key 200, two wrong keys 401 each, then the right key 429 with `Retry-After: 30`. Log lines written with `-log-buffer 16KB` all appeared at exit. The same config without `auth` failed `-validate`.

### Docs updated

- README: features, the new "admin API is on loopback or behind auth" decision, the failure limit replacing the old "rate limit runs before auth" item, the `-log-buffer` flag, and "what's not supported" without admin auth, the failed-login limiter or admin readiness.
- `examples/prism.yaml`: the admin comment (auth, probes, a Prometheus bearer key) and `failure_limit: 10/m` on the example route.

Steps 1–4 were checked by hand against the real binary; steps 5–7 only by the tests above.

## Next steps

Steps 8–10 of the plan:

8. **ip_hash, canary and rate-limit evictions.** Raise `pointsPerWeight` for a spread of about ±5%, add the e2e test for canary with ip_hash on both sides, and add `prism_ratelimit_evictions_total`.
9. **Latency tail.** Needs AC power, the `performance` governor (the user's sudo) and the two containers stopped, as in M7.
10. **Exit tests and wrap-up.** e2e tests for the M8 exit criterion (the admin auth and probe tests above are unit-level so far), the README, the project-plan status, this handoff, and `go mod tidy` once the user allows moving `~/go/go.mod`.

## Open items

- [ ] **A reload during a health check wastes that check.** The old checker is stopped mid-check and drops the result, and the new one checks again at the same `Due`. The backend sees one extra request per such reload; the count toward `unhealthy_after` is unaffected.
- [ ] **A background revalidation that finds no free slot gives up**, and the next request in the window starts another. On a route pinned at `max_concurrency`, an entry may then age out of the window before it is refreshed, and fall back to an ordinary miss.
- [ ] **Background revalidations write no access log line.** Their failures are logged ("upstream request failed" and so on, with the triggering request's ID), and their backend latency is recorded, but `prism_requests_total` counts only the client's `UPDATING`.

- [ ] **Admin 401s are logged at Info**, scanners included. The failure limit bounds wrong credentials, but requests with no credentials are not limited, and each one logs a line. Lower it to Debug if it gets noisy.
- [ ] **Concurrent guesses can overshoot the limit by up to the in-flight requests.** API keys are a map lookup with no bcrypt slot, so a burst of concurrent wrong keys all pass `Peek` before any of them is recorded. That costs only map lookups, and the overshoot is then charged, which pushes the client's lockout further out. Basic auth rechecks after the bcrypt slot, so its overshoot is at most GOMAXPROCS.
- [ ] **One failure limiter per authenticator.** A client is counted separately on each route and on the admin API. A global per-client limit would need a shared limiter in the runtime.
- [ ] Carried from M7 and earlier: see the plan's M8 scope and "Out of scope" list.

## Conventions

- Claude writes code directly into the files; the user doesn't type it from chat. Plan each step in chat, implement one step at a time, and run `make lint test` after each.
- Edit in place and never overwrite the user's own work. Match the existing style: short doc comments that explain why, table-driven tests, and fuzz tests against a simple reference.
- Time-based logic takes an injectable `now`, and tests run on a fake clock.
- Every hot-path function gets a benchmark with `ReportAllocs`.
- Add each new fuzz target to the Makefile's `fuzz` target.
- Libraries are allowed only for YAML, Prometheus and OpenTelemetry, plus `golang.org/x/crypto/bcrypt`, and vegeta and benchstat as `go tool`s (all confirmed in M8).
- Docs are plain Markdown files in `docs/`.
- Commit only when the user asks.
