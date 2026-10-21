# M7 handoff

As of 2026-10-21, M7 (benchmarks and write-up) is complete: all 7 steps are done, and `make lint test validate` passes with `-race`. The user asked for M6 and M7 to be committed separately and pushed to main. M6 is `81f0422`, and M7 is the commit after it.

- The repo lives at `/home/mudit/prism`. The module is `github.com/MuditGarg007/prism`, on Go 1.27.
- M7 builds on M6 (`81f0422`), which builds on `2a87a3a` (M5). `docs/m6-handoff.md` describes M6.
- **How the two commits were split:** M6 and M7 were both uncommitted in one working tree, and M7's step 1, 2 and 4 changes touched files M6 had also changed. The M6 commit was built in a separate worktree from the working tree with M7's changes taken out by hand: `bench/`, `docs/bench/`, this handoff, `server/bench_test.go`, `admin.pprof` and `TestPprof`, the faultserver's `cache_control`, `bench-compare`, the vegeta and benchstat tools in `go.mod`, and the step 4 allocation fixes (`parseTime`, the shared forwarding array, `Trace.header`/`HexIDs`). `make lint test validate` passed on that tree before it was committed.
- Run `make lint test` to check the current state. Run `QUICK=1 make bench-compare` (about 1 minute) to check the bench harness.
- **Before a full bench run:** the laptop must be on AC power (on battery, throughput drifts down by 15% or more during a run). The CPU governor must be `performance`; it resets to `powersave` on every boot, and setting it needs the user's sudo password (`echo performance | sudo tee /sys/devices/system/cpu/cpu*/cpufreq/scaling_governor`). Stop the `verexa-pg` and `autosploit-hardening-control-plane` containers (the user allowed this for bench runs) and start them again afterwards.
- Every `go` command prints `go: warning: ignoring go.mod in $GOPATH /home/mudit/go`, and `go mod tidy` fails because of that stray file. Add dependencies with `go get`.
- The source plan is `docs/project-plan.md`, section "M7: Benchmarks and write-up".
- **New tool dependencies:** `go get -tool` added `github.com/tsenart/vegeta/v12` v12.13.0 and `golang.org/x/perf/cmd/benchstat`. They sit in `go.mod`'s `tool` block next to staticcheck, and they pulled in several indirect modules. None of it is linked into the `prism` binary (`go list -deps ./cmd/prism` shows neither).

## Decisions

The user accepted these with "do first 2 steps". The three open points took their recommended defaults:

- vegeta as a `go tool`.
- nginx through Docker.
- The cache-hit comparison is included.

Don't reopen these unless asked.

| # | Decision | Choice |
| --- | --- | --- |
| D1 | Load tool | vegeta, built from the `go tool` dependency. Fixed-rate runs are open loop, so a slow proxy can't slow the sender down and hide its latency. The closed-loop runs (`-rate=0`, fixed workers) measure maximum requests per second. |
| D2 | nginx | `nginx:1.28.0-alpine` in Docker with `--network host` and `--cpuset-cpus`. `bench/nginx.conf` is set to do what Prism does: HTTP/1.1 keep-alive to the backend, the X-Forwarded-* and X-Request-Id headers added, one access-log line per request, `worker_processes` equal to Prism's GOMAXPROCS, and `keepalive_requests` raised, since Go never closes a connection after a set number of requests. |
| D3 | Backend | `test/faultserver` with no faults and a 1 KiB body. It gained `-cache-control` (and `cache_control` on `/_fault`) for the cache runs. |
| D4 | Scenarios | `direct`, `nginx`, `prism-bare`, `prism-full` (least_conn over two backends, a breaker, outlier detection, header rules, a rate limit that never refuses, a body limit, a concurrency cap and retries with a budget), `nginx-cache` and `prism-cache`. |
| D5 | Method | `taskset`: load on CPUs 0–3, proxy on 4–7 (the fastest P-cores, 4 threads), backends on 8–11. Each scenario gets fresh processes and a warm-up. Then come `REPEATS` (3) runs of `DURATION` (30s) at each rate in `RATES` (5000 and 20000), and 3 closed-loop runs of 15s with 64 workers. Medians are reported. Added latency is a scenario's latency minus the direct backend's at the same rate. |
| D6 | Profiling | `BenchmarkPipeline` measures Prism's own per-request cost without sockets. `admin: {pprof: true}` serves `/debug/pprof/` on the admin listener, for profiles of the real binary under load. |
| D7 | Allocation fixes | Fix the top 2–3 sources the profiles show, with `benchstat` before/after, and change no behavior. |
| D8 | Results | `make bench-compare` runs `bench/run.sh`, which writes to `bin/bench/` (gitignored). `bench/report` writes `results.md` and `latency.svg`. Checked-in results go to `docs/bench/` in step 5. |
| D9 | README | Mermaid diagrams of the pipeline and the packages, a quickstart, design decisions and their tradeoffs taken from the handoffs, the benchmark table, links to the chaos graphs, a "what's not supported" section taken from the open items, and the test strategy. |
| D10 | Exit criterion | The plan has none for M7. Proposed: `make bench-compare` reproduces the table, allocations per request are measurably lower per `benchstat`, and the README is complete. |

The 7 steps:

1. Pipeline benchmark and admin pprof
2. Bench harness
3. Baseline run and a profile under load
4. Allocation fixes with `benchstat`
5. Final run, with results in `docs/bench/`
6. README
7. `project-plan.md` status and this handoff

## What is built

### Step 1: pipeline benchmark and pprof (`server/bench_test.go`, `admin/admin.go`)

- `BenchmarkPipeline` sends a GET through `accessLog` (JSON to `io.Discard`), metrics, the runtime's handler and the transport call. Configs are built with `config.Parse` and `runtime.Build`.
- The backend is `stubBackend`, which answers from memory. It is put under each backend's real `http.Transport` with `Transport.RegisterProtocol("http", stub)`, so no production code had to change to make the transport injectable.
- The sub-benchmarks are `stub` (the stub alone, to subtract), `bare`, `full` and `cache_hit`. The stub's `Date` is the current time; a fixed date made every cache "hit" a stale revalidation.
- `discardWriter` keeps only the status, and its header map is cleared each iteration. The request is reused.
- `config.Admin.Pprof` (`admin.pprof`, default false). `admin.New` reads it from the current config, because admin settings need a restart anyway. It registers `pprof.Index`, `Cmdline`, `Profile`, `Symbol` (GET and POST, since a method-less pattern conflicts with `GET /debug/pprof/`) and `Trace`. `net/http/pprof` also registers on `http.DefaultServeMux`, which Prism never serves. `TestPprof` covers on and off. The example config documents it.

Baseline on the i7-13620H, 16 threads:

| benchmark | time | B/op | allocs/op |
| --- | --: | --: | --: |
| `BenchmarkPipeline/stub` | 299 ns | 672 | 6 |
| `BenchmarkPipeline/bare` | 3.7 µs | 2,590 | 26 |
| `BenchmarkPipeline/full` | 4.3 µs | 2,605 | 27 |
| `BenchmarkPipeline/cache_hit` | 2.4 µs | 1,073 | 13 |
| `BenchmarkCacheHit` (proxy only) | 843 ns | 784 | 11 |
| `BenchmarkProxy` (over loopback) | 58 µs | 14,313 | 158 |

So Prism's own work is about 20 allocations per proxied request; the rest of `BenchmarkProxy`'s 158 belong to the net/http server, client and test backend.

One known allocation source: 22% of `BenchmarkCacheHit`'s allocated objects are `time.newParseError`, from `http.ParseTime("")` on a missing `If-Modified-Since` or `Last-Modified` in `cache/conditional.go:18,22`. `cache/policy.go:146,157` has the same pattern. Step 4 fixed it.

### Step 2: bench harness (`bench/`)

- `bench/prism-bare.yaml`, `prism-full.yaml` and `prism-cache.yaml` listen on `127.0.0.1:8090`, with admin and pprof on `127.0.0.1:9901` and `max_idle_conns_per_host: 512`. Backends are on 9101 and 9102.
- `bench/nginx.conf` uses `:8090` to proxy and `:8091` to proxy with `proxy_cache` plus `proxy_cache_lock`. The cache location repeats `add_header X-Request-Id`, because nginx drops inherited `add_header`s in a block that has its own.
- `bench/run.sh`:
  - It builds `prism`, `faultserver`, `vegeta` and `report` into `bin/bench/` and writes `env.txt`: date, commit, CPU, kernel, Go, nginx image, governor, pinning and run settings.
  - For each run it writes a vegeta JSON report and hdrplot, the proxy's CPU microseconds, and Prism's allocations. CPU comes from `/proc/<pid>/stat` for Prism and the container's cgroup `cpu.stat` for nginx; allocations come from the `go_memstats_mallocs_total` delta. The raw `.bin` is deleted after reporting.
  - Access logs are truncated after every run, so they can't fill the disk. Prism's is opened with `>>`, and nginx's is pre-created by the user, so truncating works.
  - Everything is set through env vars. `QUICK=1` does one 3s run each at 2000 req/s.
  - vegeta's `-cpus` is a global flag, so it goes before `attack`.
- `bench/report` (`main.go`, `svg.go`, `main_test.go`):
  - Medians are taken metric by metric, and NaN is ignored for unmeasured CPU or allocations.
  - The tables are fixed-rate latency (p50, p90, p99, max, added p50 and p99, success, CPU/req, allocs/req) and closed loop. Errors are listed when there are any.
  - The SVG plots latency against percentile on a log scale of nines up to 99.99%, at the highest fixed rate, using the run with the median p99.
- `make bench-compare`. `.PHONY` updated.

### Trial run (`QUICK=1`, one 3s run each)

These numbers only show that the harness works; they are too short to quote.

| scenario | added p50 at 2k req/s | added p99 | closed-loop req/s | CPU/req (closed loop) | allocs/req |
| --- | --: | --: | --: | --: | --: |
| direct | | | 109k | | |
| nginx | +33 µs | +64 µs | 98k | 19 µs | |
| prism-bare | +65 µs | +122 µs | 67k | 47 µs | 87 |
| prism-full | +84 µs | +155 µs | 63k | 50 µs | 91 |
| nginx-cache | +1 µs | −3 µs | 95k | 19 µs | |
| prism-cache | +28 µs | +115 µs | 93k | 23 µs | 34 |

Every run had 100% success. The graph renders correctly; it was checked with `rsvg-convert`.

### Step 3: baseline and a profile under load (`bin/bench-baseline/`, not checked in)

- The baseline run is in `bin/bench-baseline/` (`results.md`, `runs/`). The pre-fix `BenchmarkPipeline`, `BenchmarkCacheHit` and `BenchmarkNotModified` output is in `old.txt`, and the post-fix output is in `new.txt`.
- `prof-old/` holds a 10 s CPU profile and two allocation snapshots (before and after the CPU profile, for `pprof -base`), taken from the admin pprof while prism-bare ran closed loop at about 64k req/s. They were taken with a small script outside the repo. It started the backends and `prism -config bench/prism-bare.yaml` pinned as in `run.sh`, then ran a 25 s closed-loop vegeta attack and fetched `/debug/pprof/allocs`, `profile?seconds=10` and `allocs` again.
- What the profile showed:
  - CPU goes mostly to syscalls (about 21%). `mallocgc` takes 10% and GC assist 5%. The access log (slog JSON, one unbuffered write per line) takes 9.6%.
  - About 87 allocations per request. Prism's own share is about 20, and the rest belong to net/http's server and client.
  - Prism's own biggest sources:
    - In `outgoing`: the `Header.Clone`, a `Set` for each X-Forwarded-* header, and a slice for each of X-Request-Id and traceparent.
    - In the access log: `hex.EncodeToString` for `trace_id` and `span_id`.
    - On cache hits: three `time.newParseError` from `http.ParseTime("")`, one for each layout it tries.
- Prism's max latency is 12–70 ms against 5 ms or less for nginx. Probably GC, but not investigated.

### Step 4: allocation fixes

| fix | where | saves |
| --- | --- | --- |
| `parseTime` skips an empty value | `cache/policy.go`, used by `conditional.go` | 3 per cache hit |
| `Forwarding.Apply` sets all its values from one array, each slice capped (`vals[n-1:n:n]`) so an append copies | `proxy/forwarded.go` | 2 per proxied request |
| The traceparent is formatted once, in `newTrace` (`Trace.header`). `Trace.HexIDs()` slices `trace_id` and `span_id` out of it for the access log. `outgoing` shares one capped array between X-Request-Id and traceparent, so `propagate` now takes the value slice. | `proxy/trace.go`, `proxy/proxy.go`, `server/accesslog.go` | 3 per proxied request, 1 per cache hit (the trace is now formatted on hits too) |

- Nothing changed behavior. `TestForwardingApply` now checks that `Header.Add` on each header leaves the others alone (the capping), and `TestNewTrace` checks the stored traceparent and `HexIDs` against the fields.
- `BenchmarkNotModified` gained a `plain` case (no conditional headers). Its old single benchmark is now `etag`, so benchstat can't pair those two.
- benchstat, in `docs/bench/benchstat.txt`:

| benchmark | allocs/op | time |
| --- | --: | --: |
| `Pipeline/bare` | 26 → 21 | −1.7% |
| `Pipeline/full` | 27 → 22 | −4.0% |
| `Pipeline/cache_hit` | 13 → 9 | −6.1% |
| `CacheHit` (proxy only) | 11 → 9 | −10.9% |

Under load, allocations per request went from 87 to 82 (bare), 91 to 86 (full) and 35 to 31 (cache).

### Step 5: final run (`docs/bench/`)

- `docs/bench/results.md`, `latency.svg` and `benchstat.txt`. The run's settings and machine are at the top of `results.md`.
- The first final run was invalid: the laptop was on battery, and throughput fell run by run (prism-bare from 64k to 54k req/s on the same binary). It was redone on AC after a reboot.

| scenario | closed-loop req/s | added p50 / p99 at 20k req/s | CPU/req (closed loop) | allocs/req |
| --- | --: | --: | --: | --: |
| direct | 115k | | | |
| nginx | 99k | +40 µs / +136 µs | 18.8 µs | |
| prism-bare | 69k | −24 µs / +68 µs | 45.1 µs | 82 |
| prism-full | 66k | −21 µs / +202 µs | 48.3 µs | 86 |
| nginx-cache | 101k | +4 µs / −20 µs | 17.5 µs | |
| prism-cache | 99k | +14 µs / +307 µs | 22.1 µs | 31 |

- **The baseline is not a fair "before".** It ran while the machine was about 10% slower overall: direct 104k against 115k now, nginx 90k against 99k. So the end-to-end gains since then can't be credited to step 4. Step 4's evidence is benchstat and the allocations under load. A fair end-to-end comparison would need the old binary (`bin/bench-baseline/prism`) re-run under the same conditions; `run.sh` always rebuilds, so it would need a way to take a prebuilt binary.
- prism-bare's p50 at 20k req/s is below direct's. Prism keeps pooled connections to the backends, while vegeta's own connections to the backend do worse at that rate. The README explains this rather than claim Prism makes requests faster.
- `bench/report`: the SVG's y-axis now uses one unit for every tick (`axisLabel`, with `TestAxisLabel`); before, it mixed "5000 µs" with "10.0 ms".

### Step 6: README (`README.md`)

- It follows D9: features, a quickstart (its minimal config passes `-validate`), the make targets, Mermaid diagrams of the pipeline and the package imports, a package table, design decisions and tradeoffs grouped by layer, the benchmark table with what the numbers say and their caveats, the allocation fixes, the two chaos graphs, the test strategy and "what's not supported".
- The caveats from the open items below are in it: prism-bare's p50 below direct's at 20k req/s, inflated CPU at fixed rates, one machine, the nginx cgroup, and the unconfirmed cause of the tail.

### Step 7: status and handoff

- `docs/project-plan.md` has the M7 status. This handoff is final.

## Next steps

M7 was the last milestone in the plan. Candidates from the open items: confirm the cause of the latency tail, decide on a buffered access log, and the items carried from M6 below. The plan's "Later, if ever" list has a disk cache, HTTP/3, adaptive concurrency limits and ACME.

## Open items

- [ ] **Tool dependencies** (vegeta, benchstat) added indirect modules to `go.mod`. They are dev tools only, but the user may want to confirm them, as with bcrypt.
- [ ] **Prism's latency tail.** Max latency is 12–28 ms in the final run, against 5 ms or less for nginx, showing at the 99.9th percentile and above in `latency.svg`. GC is the likely cause (GC assist was 5% of CPU), but it is not confirmed. A `GODEBUG=gctrace=1` run or an execution trace from `/debug/pprof/trace` would settle it.
- [ ] **The access log writes one unbuffered line per request** (9.6% of CPU under load). nginx does too by default. A buffered writer would change what is lost on a crash, so it is a decision for the user, not an allocation fix.
- [ ] **CPU/req at low rates is inflated** for Prism. The Go scheduler spins idle Ps, and `/proc` ticks are 10 ms. The closed-loop numbers are the fairer CPU comparison; the README says so.
- [ ] **vegeta shares the machine.** At around 100k req/s, the direct backend may be limited by the load generator on 4 cores rather than by the backend. The README says that the numbers come from one laptop.
- [ ] **nginx CPU comes from the container's cgroup**, which also counts the nginx master process. That is negligible; the README notes it.
- [ ] Carried from M6: see the open items in `docs/m6-handoff.md`. The README's "what's not supported" section lists them.

## Conventions

- Claude writes code directly into the files; the user doesn't type it from chat. Plan each step in chat, implement one step at a time, and run `make lint test` after each.
- Edit in place and never overwrite the user's own work. Match the existing style: short doc comments that explain why, table-driven tests, and fuzz tests against a simple reference.
- Time-based logic takes an injectable `now`, and tests run on a fake clock.
- Every hot-path function gets a benchmark with `ReportAllocs`.
- Add each new fuzz target to the Makefile's `fuzz` target.
- Libraries are allowed only for YAML, Prometheus and OpenTelemetry, plus `golang.org/x/crypto/bcrypt` as of M6, plus vegeta and benchstat as `go tool`s as of M7 (both pending the user's confirmation).
- Docs are plain Markdown files in `docs/`.
- Commit only when the user asks.
