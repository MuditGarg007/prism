# M4 handoff

As of 2026-10-20, M4 (caching) is complete: all 10 steps are done, the exit test passes end to end (1,000 concurrent requests for a cold key make exactly 1 upstream request), and `make lint test validate` passes with `-race`. It is committed as `b2416f9` and pushed to main. Next is M5 (operations).

- The repo lives at `/home/mudit/prism`. The module is `github.com/MuditGarg007/prism`, on Go 1.27.
- M4 builds on `6f22412` (M3 plus its docs commit).
- Run `make lint test validate` to check the current state, and `make fuzz` to run the eight 30 s fuzz targets (`FuzzCacheControl` is new).
- The as-built cache behavior (X-Cache values, hit-for-pass, stale-if-error, purge) is summarized under M4's "Status" in `docs/project-plan.md`.
- Every `go` command prints `go: warning: ignoring go.mod in $GOPATH /home/mudit/go`. This comes from the local environment and is harmless.
- The source plan is `docs/project-plan.md`, section "M4: Caching, Layer 4". Earlier state and decisions are in `docs/m1-handoff.md`, `docs/m2-handoff.md` and `docs/m3-handoff.md`.

## Decisions

These were accepted at the start of M4. The user also approved changing the pipeline order in `project-plan.md` (D9) and bringing the admin listener forward from M5 (D1). Don't reopen them unless asked.

| # | Decision | Choice | Why |
| --- | --- | --- | --- |
| D1 | Config | A top-level `cache: { max_size: 64MB }` sizes one store that all routes share. Route `cache: { enabled, max_object: 1MB, lock_timeout: 5s, bypass_header: "", stale_if_error: 0 }`. A new top-level `admin: { listen }` block, where a non-loopback address only gets a warning. | One byte budget for the whole process. Purge needs an admin API, so the admin listener arrives in M4 and M5 extends it. |
| D2 | What is cached | Only GET is stored. A HEAD can be answered from a GET entry, but a HEAD miss passes through uncached. Requests with `Range` or with `no-store` bypass the cache. Other client `Cache-Control` directives are ignored, as nginx does. A request with `Authorization` is cached only if the response says `public`, `s-maxage` or `must-revalidate`. Only 200/301/404 are stored. Responses with `Set-Cookie`, `no-store`, `private`, `Vary: *`, or no explicit freshness are skipped. `no-cache` is stored but always revalidated (it needs a validator). | RFC 9111 rules for a shared cache, kept conservative. Clients can't force traffic through to the origin. |
| D3 | Freshness | Lifetime is `s-maxage`, then `max-age`, then `Expires − Date`. Age follows RFC 9111 §4.2.3, and hits send `Age`. No heuristic freshness. The Cache-Control parser is hand-written. | Guessing a lifetime is how stale pages get served. |
| D4 | Key and Vary | The primary key is method, scheme, host, escaped path and query. An index maps each primary key to its latest Vary names, and the full key appends the normalized values of those request headers. Purge removes every variant. | One lock, two map reads per lookup. Vary is honored without storing responses that differ by header under the same key. |
| D5 | Store | A hand-written byte-bounded LRU (a map plus a doubly linked list) behind one mutex. Entries are immutable. A 304 merge stores a new entry that shares the body. Expired entries stay until evicted. | Hits write to the client without holding the lock, and stale entries can still be revalidated. |
| D6 | Coalescing | A hand-written singleflight per key: one leader and N waiters, who re-check the store after the leader commits. A waiter past `lock_timeout` fetches for itself and doesn't store the result. Hit-for-pass: an uncacheable result marks the key for 10 s so later requests don't queue. | nginx `proxy_cache_lock` semantics plus Varnish hit-for-pass, so an uncacheable URL never serializes requests. |
| D7 | Tee | The body is buffered into the entry while it streams to the client. Caching is abandoned when the size is over `max_object` (checked on Content-Length or while buffering) or on a read error. The leader fetches on `context.WithoutCancel` and keeps filling after its client leaves. | The waiters depend on the leader's fetch. |
| D8 | Revalidation and stale | A stale entry with an ETag or Last-Modified is revalidated with `If-None-Match`/`If-Modified-Since`. A 304 merges headers per RFC 9111 §3.2. On the cache path the client's own conditionals are stripped going upstream and answered locally (304 when they match). `stale-if-error` is the response directive capped by the route setting. `stale-while-revalidate` is deferred. | A 304 is never stored in place of a body, and clients still get 304s. |
| D9 | Pipeline order | The cache lookup and the coalescing wait come **before** the concurrency slot. Only the leader (a miss or a revalidation) takes a slot. Rate limiting and the body checks still run first. Stored headers are taken before `response_set`/`response_remove`, and those rules are applied on serve. `docs/project-plan.md` is updated. | Otherwise cheap hits and 1,000 coalesced waiters would hold slots and starve real work. |
| D10 | Visibility | An `X-Cache` response header (`HIT`, `MISS`, `STALE`, `REVALIDATED`, `BYPASS`), a `cache=` field in the access log, and atomic `cache.Stats` counters served as JSON by admin `GET /cache/stats`. Admin `POST /cache/purge` takes `{host, path, prefix}`. | Prometheus comes in M5; the counters are ready to map onto it. |

The 10 steps are:

1. Config
2. Policy
3. LRU
4. Keys, Vary and Store
5. Singleflight (`cache/flight.go`)
6. Split `proxy.forward` into fetch and write, with no behavior change
7. Cache path in the proxy
8. Revalidation and stale-if-error
9. Admin listener and wiring
10. E2E tests and docs

The exit test is in step 10: 1,000 concurrent requests for a cold key make exactly 1 upstream request.

## What is built

### Step 1: config (`internal/config`)

- New top-level `cache: { max_size }`. It's a value, not a pointer, and always defaults to 64 MB. The store is only built (step 9) when some route enables caching.
- The route `cache` block gains `lock_timeout` (default 5s, must be positive), `bypass_header` (off when empty; must be a valid header name and not hop-by-hop) and `stale_if_error` (default 0, which never serves stale; must not be negative). `max_object` keeps its 1 MB default and must not exceed `cache.max_size`.
- New top-level `admin: { listen }` (nil means off). The address is checked like a listener's and must not equal a listener address. `checkAddr` now does that check for both.
- Sizes can't be negative, and 0 takes the default, so the only size bound validation needs is `max_object <= max_size`.
- `runtime.Build` warns `option=admin` and `option=routes[N].cache` as not implemented yet. Step 9 removes both, which leaves `warnIgnored` empty.
- `examples/prism.yaml` shows `cache`, `admin` and the new route fields.
- `docs/project-plan.md`: the pipeline order is changed for D9, with a paragraph on why. The M4 bullet says the admin listener arrives here. The example config has `cache` and `admin`.

### Step 2: Cache-Control and freshness (`cache/cachecontrol.go`, `cache/policy.go`)

- `ParseCacheControl(vv []string) Directives` treats all field lines as one list. It handles case-insensitive names, OWS, quoted values with `\` escapes, commas inside quotes, and empty members. Unknown directives are ignored. Anything unparseable is skipped up to the next comma outside quotes.
- `Directives` holds the flags `NoStore`, `NoCache`, `Private`, `Public` and `MustRevalidate` (which also covers `proxy-revalidate`), and the durations `MaxAge`, `SMaxAge`, `StaleIfError` and `StaleWhileRevalidate`. A duration is `cache.Absent` (−1) when not given.
  - A value that isn't a valid delta-seconds (non-digits, negative, missing, trailing garbage, unterminated quote) counts as **0**, so the response is stale. RFC 9111 §4.2.1 encourages that.
  - When a directive repeats, the **smallest** value wins.
  - An overflow is capped at 2^31 s (§1.2.2).
  - The field-name forms `private="…"` and `no-cache="…"` apply to the whole response.
- `Directives.String()` formats a canonical value. `FuzzCacheControl` checks that durations stay in range and that `Parse(String(d)) == d`. It's in `make fuzz`.
- `RequestCacheable(method, h)`: GET or HEAD, no `Range`, no request `no-store`.
- `Storable(reqHeader, code, h, cc) string` returns a reason (`status`, `no-store`, `private`, `set-cookie`, `vary`, `authorization`, `no-freshness`), or `""` when the response can be stored. The reason is meant for debug logs.
  - "Explicit freshness" means `s-maxage`, `max-age` or any `Expires` header, even an invalid one, which then gives a lifetime of 0.
  - `no-cache` with no freshness is stored only if it has an ETag or Last-Modified.
- `NewFreshness(h, cc, requested, received) Freshness{Received, InitialAge, Lifetime}`.
  - `InitialAge` is corrected_initial_age: max(apparent age, Age + transit).
  - A missing or invalid Date counts as `received`.
  - An invalid Age is ignored, and a list uses its first member.
  - `no-cache` makes Lifetime 0.
  - Methods: `Age(now)`, `Fresh(now)`, `Staleness(now)` (negative while fresh), and `AgeHeader(d)` (whole seconds, clamped to [0, 2^31]).
- Note for step 8: RFC 9111 says `s-maxage` implies `proxy-revalidate`. `Directives.MustRevalidate` does **not** fold that in, so the stale-if-error check must treat `SMaxAge != Absent` as must-revalidate.

### Step 3: LRU (`cache/lru.go`, `cache/entry.go`)

- `Entry{Key, Primary, Vary, Status, Header, Body, CC, Freshness}` is immutable once stored. `Size()` = body + key + primary + Vary names + header bytes (16 B per value) + `entryOverhead` (512 B), so a store full of tiny responses still respects `max_size`.
- `lru` (unexported, not concurrency-safe) uses a map from key to node and a list with a sentinel root, most recent first.
  - Methods: `get` (moves the entry to the front), `add` (replaces the entry under the same key, then evicts from the back until it fits; an entry larger than the whole cache is refused and nothing is evicted), `remove`, and `removeFunc` (O(n), for purge).
  - `onRemove` fires for every entry that leaves, whether evicted, replaced or removed. `evictions` counts only evictions to make room.
- Tests: eviction order, recency on get, one big entry evicting several, replace accounting, too-big refusal, removal, and a 20,000-operation random model test against a plain slice.
- Benchmarks: `get` 10.5 ns with 0 allocations; `add` with eviction 129 ns (2 allocations, the benchmark's Entry and the node).

### Step 4: keys, Vary and Store (`cache/key.go`, `cache/store.go`)

- `PrimaryKey(r)` returns `"GET http://example.com/a%2Fb?x=1"`.
  - HEAD maps to GET. The host is lowercased and drops `:80` for http and `:443` for https. The path is `EscapedPath()`, so `%2F` stays distinct.
  - The raw query is kept in order, and an empty `?` is dropped.
  - The scheme comes from `r.TLS`, which is right while Prism terminates TLS itself. (`X-Forwarded-Proto` from a trusted proxy isn't considered; revisit if that matters.)
- `VaryNames(h)` returns the canonical names, sorted and deduplicated. `VariantKey(primary, names, req)` returns primary + for each name `NUL name ':' values`. Whitespace around list members is trimmed, and empty members are dropped. A missing header and an empty one count as the same. Without names, the key is the primary key itself, with no allocation.
- `Store` is a mutex around the lru plus `vary map[primary]*varyIndex{names, n}`.
  - `Put` sets the index names to the latest response's Vary and increments `n`. The lru's `onRemove` decrements it and deletes the index entry with the last variant, so the index can't leak.
  - When a backend changes its Vary, lookups use the new names. Old variants become unreachable (their keys include the old names) and age out.
  - `NewStore(maxBytes)`.
  - `NewEntry(primary, req, code, h, body, cc, f)` builds Key and Vary.
  - `Get(primary, req)` returns entries whether fresh or not; the caller decides.
  - `Put(e) bool`.
  - `Remove(e)` deletes only if `e` is still the entry under its key, so a stale reader can't drop a newer entry.
  - `Purge(host, path, prefix) int` matches on host+path+query and covers every method, scheme and variant. An exact purge of `/x` doesn't touch `/x?q=1`. With prefix, `/x` also matches `/xyz` and `/x?q=1`.
  - `Stats()` returns `{Entries, Bytes, Evictions}`.
  - Note for step 9: with prefix set, an empty path would match other hosts that share the prefix (`a.com` matches `a.company`). The admin handler must require a path starting with `/`.
- Tests: key normalization, Vary names and keys, variants, a response without Vary serving every request, a Vary change, the index following eviction, `Remove` sparing a newer entry, purge (exact, prefix, schemes, variants, host boundary), and 8 goroutines of mixed operations under `-race` with an index-consistency check.
- Benchmarks (16 cores, parallel): `Get` without Vary 94 ns with 0 allocations; with Vary 135 ns with 1 allocation (the variant key). The single mutex is the contention point. Sharding the store by key is an option for M7 if profiles show it.

### Step 5: singleflight (`cache/flight.go`)

- `Flight` is a mutex around `map[key]*Call`. `Join(key)` returns a role:
  - `Lead`: the caller fetches, then calls `Call.Done(res)` with `Stored`, `Uncacheable` or `Failed`. Only the first `Done` counts, so the proxy defers `Done(Failed)` as a fallback (it also covers a panic) and reports the real result earlier.
  - `Wait`: `Call.Wait(ctx, timeout)` returns the leader's result, `TimedOut` or `Canceled`.
  - `Pass`: the key is marked hit-for-pass, and the `Call` is nil.
- `Done(Uncacheable)` replaces the call with a marker that lasts `PassFor` (10 s). Expired markers are swept at most once per `PassFor`, on a `Join` that creates a call, so keys nobody asks for again don't leak.
- `Waiting()` counts requests blocked in `Wait`. The tests use it to know when every waiter has joined, and it could feed a gauge in M5.
- Tests: 1,000 goroutines produce exactly one leader and 999 `Stored` waiters; independent keys; timeout (including a timeout of 0); cancel; `Failed` releases everyone and leaves the key free; `Done` only counts once, and a late `Done` can't touch the key's next leader; hit-for-pass expiry on a fake clock; the sweep.
- Benchmark: `Join` plus `Done` 122 ns, 2 allocations (the `Call` and its channel).

### Step 6: split `forward` (`proxy/proxy.go`)

- `fetch(r, rt, an) (fetched, *fetchError)` is the retry loop. It writes nothing to the client, removes hop-by-hop response headers, and leaves the backend acquired until `fetched.release()`.
- `fetchError{b, err, clientBody}` carries what `fetchFailed` needs to answer as before: 400/499 for a client body failure, `fail` otherwise.
- `write(w, r, rt, an, f, body)` applies `response_set`/`response_remove`, writes the headers, copies the body (from `f.resp.Body` or a wrapper) and sends trailers.
- `forward` is now these three calls. `acquire`/`release` took the concurrency-slot code out of `ServeHTTP`.
- There is no behavior change: every existing test passes unchanged, and `BenchmarkProxy` stays at 147 allocations.

### Step 7: cache path (`proxy/cache.go`, `cache/conditional.go`, `cache/stats.go`)

- `Route.Cache *proxy.Cache` holds the shared `Store`, `Flight` and `Stats`, plus the route's `MaxObject`, `LockTimeout`, `BypassHeader` (canonical) and `StaleIfError`, and an injectable `Now`.
- Order in `ServeHTTP` (D9): path check, route, rate limit, body checks, then the cache, then the concurrency slot. Only a request that goes to a backend (`fill`) takes a slot, so hits and waiters never do. A test fills the only slot and checks that a hit is still served while a miss gets 503.
- `cacheable(r)` is `RequestCacheable` plus: no request body (`ContentLength != 0`), no `If-Match`, no `If-Unmodified-Since`. Requests that fail it skip the cache entirely and get no `X-Cache`. Otherwise one client's 412 would mark the key hit-for-pass for everyone.
- `serveCache` loop:
  1. `Store.Lookup` (new; it also returns the key, which is the variant key once the Vary index knows the names, and the primary key before that). If the entry is fresh, it's a `HIT`.
  2. A HEAD that misses goes to the backend as it is (`MISS`, nothing stored).
  3. `Flight.Join(key)`. `Lead` or `Pass` calls `fill` with storing on. Waiters `Wait` until a deadline taken once from `lock_timeout` on the real clock. `Stored` loops back to step 1 (a different variant then joins its own key). `Canceled` is a 499. On `Failed` with a usable stale entry, the waiter serves it as `STALE` (step 8). Otherwise, after a timeout or a leader with nothing to share, the waiter calls `fill` alone with storing off.
  4. An entry stored while the request waited answers it even if it's already stale (for example `max-age=0`). Otherwise every waiter would revalidate in turn. There's a test for that.
- `fill`:
  - The leader fetches on a clone of the request with `context.WithoutCancel`. Pass, bypass and waiter fetches keep the client's context.
  - Whenever it stores or revalidates, it strips the client's `If-None-Match`/`If-Modified-Since` going upstream. A plain HEAD miss or a waiter's alone-fetch keeps them, since nothing is stored.
  - `unstorable` = `Storable` plus `size` (Content-Length > `max_object`), `trailers`, and `event-stream`. The reason goes to a debug log line, "response not stored".
  - An unstorable answer releases the waiters at once, rather than after the body: `Uncacheable` when status < 500 (this also removes a stale entry the answer replaces), and `Failed` for a 5xx, which says nothing about cacheability.
  - Otherwise the header is cloned **before** the route's response rules, and the body goes through `teeBody`. That buffer is pre-sized from Content-Length. Past `max_object` it drops the buffer and calls `Done(Uncacheable)` right away.
  - After `write`, if the client left mid-body and this is the leader, the rest is drained into the buffer (D7). A read error while draining overturns the backend's success.
  - The stored header gets `Content-Length` = the buffered length. `Put` failing (an entry larger than the whole store) counts as `Uncacheable`.
- `serveEntry`:
  - Copies the entry's header map (values are shared but capped, see `Revalidated`), sets `Age` and `X-Cache`, and then applies the route's response rules. Operators can therefore remove `X-Cache` with `response_remove`.
  - A 200 whose validators match the client's conditionals gets a 304 carrying only Cache-Control, Content-Location, Date, ETag, Expires, Last-Modified and Vary.
  - A HEAD gets no body.
- `cache.NotModified(req, h)` follows RFC 9110 §13.1.2–3: a weak ETag comparison with a hand-written entity-tag scanner (commas may appear inside quotes), `*`, and If-Modified-Since only when If-None-Match is absent and Last-Modified is valid.
- A leader's own `MISS` is always the full 200, even if its client sent a matching If-None-Match. Answering it locally would mean holding the body back while it tees. Later requests get 304s from the entry.
- `X-Cache` is one of `HIT`, `MISS`, `STALE`, `REVALIDATED` or `BYPASS`, and `Annotations.Cache` records the same value. The access log has a `cache=` field when the value is set (pulled forward from step 9; it was one line). `cache.Stats` has one atomic counter per value, and `Snapshot()` returns `StatsSnapshot` (no JSON tags yet).
- Bypass header present (any value, even empty): the request skips the lookup and the Flight, but its response is still stored.
- Tests (`proxy/cache_test.go`, on a fake clock that starts on a whole second, so Date and Age are exact):
  - hit and Age, misses on other URLs, and expiry;
  - nine kinds of unstorable response, including chunked over `max_object` and trailers;
  - Authorization with `public`;
  - requests that skip the cache;
  - response rules applied on serve but not stored;
  - HEAD;
  - bypass;
  - client conditionals;
  - Vary;
  - **1,000 concurrent cold requests → exactly 1 backend request, 1 MISS and 999 HIT**, handler-level (step 10 repeats this end to end);
  - waiters released early for an uncacheable response, then hit-for-pass not queueing;
  - lock timeout;
  - the leader's client leaving mid-body while its waiter still gets the full entry;
  - hits skipping the concurrency limit.
- Benchmark: `BenchmarkCacheHit` (1 KB body, handler only) 630 ns, 9 allocations: the primary key, `Annotations`, the Age and X-Cache values, and the header map.

### Step 8: revalidation and stale-if-error

- The leader, or a `Pass`/alone fetch, carrying a stale entry sends that entry's ETag as `If-None-Match` and its Last-Modified as `If-Modified-Since`.
- A 304 goes through `cache.Revalidated(e, h, requested, received)`, which follows RFC 9111 §3.2:
  - The 304's fields replace the stored ones, except Content-Length. The stored `Age` is dropped unless the 304 sends one.
  - The Cache-Control and freshness are worked out again. Key, Vary names and body are kept (the body is shared).
  - Merged values are `slices.Clip`ped, so an `Add` on a served header can't write into a shared array.
  - The merged entry is `Put` if it's still storable (`Done(Stored)`). Otherwise the old entry is removed (`Done(Uncacheable)`).
  - The client gets `REVALIDATED`, or a 304 if its own conditionals match.
- A full response to a revalidation replaces the entry as a normal miss does. If that response can't be stored, the stale entry is removed.
- Stale-if-error: `Cache.staleOK(e, now)`.
  - **Interpretation taken:** the route's `stale_if_error` is the window and applies even when the response has no directive. A response's `stale-if-error` can only narrow it, never widen it. That matches the config field's doc comment ("caps how long past its freshness an entry may be served"), and nginx's `proxy_cache_use_stale`. D8's wording could also be read as "only with the directive"; flip this if the user wants that.
  - `must-revalidate`, `no-cache` and `s-maxage` (which implies proxy-revalidate, per the step 2 note) never serve stale.
  - Triggers: a transport error (connect, timeout or upstream), or any status ≥ 500. The access log's `error=` carries the failure kind while the client gets `STALE` with the right `Age`. A warn line is logged ("upstream request failed, serving stale" or "upstream returned an error, serving stale").
  - A 5xx that can't be served stale is passed through, and the stale entry stays in the store.
  - Waiters whose leader failed serve the stale entry themselves, rather than each sending the failing backend a request: with 50 concurrent requests, the backend sees 1.
- Tests: ETag and Last-Modified revalidation; the merged entry fresh again; a client 304 from a revalidated entry; replacement by a new 200, and removal when it's uncacheable; an eight-case stale-if-error table covering the route and response windows, must-revalidate and s-maxage; stale on a connection error with the exact Age; stale waiters.
- `cache` tests: `NotModified` (17 cases), `scanETag`, `Revalidated` (header merge, Age drop, shared body, the old entry untouched, capped values), `Lookup` keys. `BenchmarkNotModified` 75 ns with 0 allocations.

### Step 9: admin listener and wiring

- `runtime.Build` builds one `runtime.Cache{Store, Flight, Stats, MaxSize}` when any route has `cache.enabled`, and keeps it on `Runtime.Cache` (nil otherwise). Each enabled route gets a `proxy.Route.Cache` from its `max_object`, `lock_timeout`, `stale_if_error` and `bypass_header`, the last through `http.CanonicalHeaderKey`, so `bypass_header: x-no-cache` matches `X-No-Cache`.
- `warnIgnored` is gone: nothing in the config is unimplemented any more. In its place, Build warns "admin API listens beyond loopback and has no authentication" when `admin.listen`'s host is empty, a non-loopback IP, or a name other than `localhost`. It's in Build, so `-validate` shows it.
- `internal/admin`: `admin.New(cur, log)` is an `http.Handler` on a `ServeMux` with method patterns. It loads `cur.Load().Cache` on each request, so it follows reloads. Every answer has `Cache-Control: no-store`, and errors are `{"error": "..."}`.
  - `GET /cache/stats` returns `admin.CacheStats`: `entries`, `bytes`, `max_bytes`, `evictions`, `waiting`, then `hit`, `miss`, `stale`, `revalidated`, `bypass`. The wire format lives in `admin`, so `cache` has no JSON tags.
  - `POST /cache/purge` takes `admin.PurgeRequest` (`host`, `path`, `prefix`) and answers `{"purged": n}`. The body is capped at 64 KB, unknown fields and trailing data are refused, `host` is required, and `path` must start with `/` (the step 4 note). The host is lowercased and loses a trailing `:80` or `:443`, since a purge covers both schemes. Each purge is logged at info.
  - Both answer 404 when no route caches.
- `server.Server.Admin(addr, h)` adds the admin API as one more `http.Server`, without the access log or the ready path. `Start` binds it with the listeners, so a port in use fails before any traffic, and `Shutdown`/`Drain` cover it. `Addrs()` lists it last, and `AdminAddr()` returns it. `cmd/prism` calls `Admin` when `cfg.Admin` is set.
- Change to step 7/8 behavior: a miss whose fetch fails with a transport error (connect, timeout) and has no stale entry to serve now sends `X-Cache: MISS`, logs `cache=MISS` and counts a miss, as a backend 5xx already did. Before, it had no cache label at all.
- `examples/prism.yaml` drops the "ignored until M4" comments and shows `stale_if_error` and `bypass_header`.
- Tests: `TestBuildCache` (only enabled routes cache, one shared store, bypass header case), `TestBuildNoCache`, `TestBuildAdminWarning` (six addresses), `TestBuildExample` now expects no warnings at all; `admin` tests for stats, a nine-case purge table (exact, query, prefix, host case and ports, other hosts), ten invalid bodies, routing and 405s, 404 without a cache, and following a reload; `TestServerAdmin`.

### Step 10: E2E tests and docs

- `test/e2e/e2e_test.go`: `startProxyLogged` is split, and the new `launch` wires the admin API the way `main` does and returns the `*server.Server`.
- `test/e2e/m4_test.go`, each through the real server built from YAML, with the admin API on `localhost:0` (validation refuses the listener's own `127.0.0.1:0` string, even with port 0):
  - `TestCacheStampede`, the exit test: 1,000 concurrent clients on a cold key. The backend holds its answer until the admin API reports 999 requests waiting, so every request has really joined. Then exactly 1 upstream request, 1 `MISS`, 999 `HIT`, every body complete, and the stats agree. Stable over `-count=10` under `-race` (about 0.15 s each).
  - `TestCacheHit`: `MISS`, then `HIT`, then a 304 for a matching `If-None-Match` and a 200 for one that doesn't, all from one upstream request; `cache=` in the access log.
  - `TestCacheRevalidate`: a `max-age=0` entry with an ETag goes `MISS`, `REVALIDATED` (the backend sees `If-None-Match` and answers 304), then `HIT`.
  - `TestCacheStaleIfError`: `STALE` while the backend answers 503 and again after it is closed; a path never stored gets 502 `MISS`; the stats count 2 stale; the warn line is logged.
  - `TestCachePurge`: an exact purge, then a prefix purge with an uppercase host, then exactly the purged URLs miss again.
- `docs/project-plan.md`: M4 has a "Status" paragraph with the as-built behavior.
- Smoke-tested the real binary: the admin API listens, `/cache/stats` and `/cache/purge` answer, and the access log shows `cache=MISS`.

## Open items carried forward

- [ ] **Store across reloads (M5).** The store is built in `runtime.Build`, so a reload starts cold. M5 should carry it over to the new runtime when `cache.max_size` is unchanged, rather than starting cold.
- [ ] **Readiness on the admin port.** The admin listener exists now, so `ready_path` could move there. That's still planned for M5 along with the other admin endpoints, which `admin.New`'s mux makes easy to add.
- [ ] **Prometheus (M5).** `admin.CacheStats` maps onto counters (`hit` … `bypass`, `evictions`) and gauges (`entries`, `bytes`, `waiting`).
- [ ] **Admin authentication.** There is none; the startup warning is the only guard. Revisit with M6's security work if the admin API ever needs to leave loopback.
- [ ] **The leader's drain has no time limit.** After its client leaves, the leader reads the rest of the body on a detached context. The transport has no body timeout, so a backend that stalls mid-body holds that goroutine and its concurrency slot indefinitely. Waiters are safe, because they give up at `lock_timeout`. Bounding the drain (for example with the upstream's response-header timeout, or a new `cache` setting) is left for M7.
- [ ] **`stale-while-revalidate`** is parsed (`Directives.StaleWhileRevalidate`) but not acted on, as D8 deferred.
- [ ] **State across reloads, WebSocket, ip_hash spread and the rate-limit key cap.** These are unchanged from `docs/m3-handoff.md`.

## Conventions

- Claude writes code directly into the files; the user doesn't type it from chat. Plan each step in chat, implement one step at a time, and run `make lint test` after each.
- Edit in place and never overwrite the user's own work. Match the existing style: short doc comments that explain why, table-driven tests, and fuzz tests against a simple reference.
- Time-based logic takes an injectable `now`, and tests run on a fake clock.
- Every hot-path function gets a benchmark with `ReportAllocs`.
- Add each new fuzz target to the Makefile's `fuzz` target.
- Libraries are allowed only for YAML, Prometheus and OpenTelemetry. The cache, LRU and singleflight are hand-written.
- Commit only when the user asks.
