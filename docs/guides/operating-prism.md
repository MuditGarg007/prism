# Operating Prism

This guide is about running Prism for real traffic: deploying it, changing its config safely, sizing it, and knowing when it needs attention. It assumes you've read [Hot reload and shutdown](../concepts/hot-reload-and-shutdown.md) and [Observability](../concepts/observability.md).

Prism is a learning project. It's well tested, but it hasn't served production traffic. Read [Before you rely on it](#before-you-rely-on-it) first.

## Build

```sh
make build
```

This writes `bin/prism`, with the version from `git describe` baked in:

```console
$ ./bin/prism -version
prism d031229
```

That's the commit it was built from. A build with uncommitted changes to tracked files gets `-dirty` on the end, so build release binaries from a clean checkout. Prism is one static binary with no runtime dependencies, so deploying it is copying one file.

## Run it under systemd

```ini
[Unit]
Description=Prism reverse proxy
After=network-online.target
Wants=network-online.target

[Service]
ExecStartPre=/usr/local/bin/prism -config /etc/prism/prism.yaml -validate
ExecStart=/usr/local/bin/prism -config /etc/prism/prism.yaml -log-format json
ExecReload=/usr/local/bin/prism -config /etc/prism/prism.yaml -validate
ExecReload=/bin/kill -HUP $MAINPID
TimeoutStopSec=40
Restart=on-failure
LimitNOFILE=65536

DynamicUser=yes
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes

[Install]
WantedBy=multi-user.target
```

Why each part is there:

- **`-validate` before start and before reload.** A reload with a bad config is already safe: Prism keeps the old config. But `SIGHUP` can't report failure, so `systemctl reload` would say it worked. Validating first makes a bad config fail the `systemctl reload` command itself.
- **`TimeoutStopSec` longer than `drain_delay` plus `shutdown.timeout`.** systemd sends `SIGTERM`, then `SIGKILL` once this runs out. With the config below (10 s + 15 s), 40 s leaves room.
- **`LimitNOFILE`.** Every client connection and every pooled backend connection is a file descriptor. The usual default of 1,024 runs out under real load.
- **`DynamicUser` and `CAP_NET_BIND_SERVICE`.** Prism runs as a throwaway unprivileged user, and can still bind ports 80 and 443. The other lines stop it writing to the system. If your config names certificate files, that user must be able to read them.

To check this unit, `systemd-analyze verify` was run on it (its only complaint was that `/usr/local/bin/prism` didn't exist on the test machine). A copy with the paths changed and the sandboxing lines removed, since a user service can't use them, was run as a user service:

```console
$ systemctl --user start prism-docs-test
$ systemctl --user reload prism-docs-test && echo "good reload ok"
good reload ok
$ sed -i 's/attempts: 1/attempts: 11/' prism.yaml    # break the config
$ systemctl --user reload prism-docs-test
Job for prism-docs-test.service failed.
$ systemctl --user is-active prism-docs-test
active
```

The journal shows why the reload failed, and that the old config kept serving:

```text
prism: .../prism.yaml: invalid config:
  routes[0].retry.attempts: must be between 0 and 10, got 11
prism-docs-test.service: Control process exited, code=exited, status=1/FAILURE
Reload failed for Prism reverse proxy.
```

The full sandboxed unit wasn't run as a system service.

## Behind a load balancer

Most deployments put a cloud or hardware load balancer in front of several Prism instances. Three settings make that work:

```yaml
trusted_proxies: [10.0.0.0/8]       # the load balancer's addresses
ready_path: /readyz                  # the load balancer's health check
shutdown:
  drain_delay: 10s                   # longer than the LB takes to notice
  timeout: 15s
```

- **`trusted_proxies`.** Without it, Prism sees the load balancer as the client, so every rate limit, IP filter and access log line uses one address. List only the load balancer's addresses. Listing too much lets clients forge their IP with `X-Forwarded-For`. See [Security](../concepts/security.md#trust-boundaries).
- **`ready_path`.** Point the load balancer's health check here. It answers `200` until shutdown begins, then `503`.
- **`drain_delay`.** After `SIGTERM`, Prism keeps serving everything while `ready_path` says `503`. The load balancer needs time to see that: its check interval times the number of failures before it gives up on a target. With checks every 2 s and 3 failures, that's 6 s, so 10 s is safe.

`shutdown.timeout` then bounds how long in-flight requests get. Set it to the longest request you're willing to wait for. A WebSocket is closed when it runs out.

### Rolling restarts

To replace Prism with a new version, restart instances one at a time. Each one drains, the load balancer moves its traffic to the others, and it comes back. Wait until it's ready again before restarting the next. A reload is enough for config changes, so restarts are only for new binaries and [restart-only settings](../reference/config.md#what-a-reload-cant-change).

### On Kubernetes

The same ideas, in Kubernetes terms. This is a sketch: it hasn't been run on a cluster.

```yaml
spec:
  terminationGracePeriodSeconds: 40     # more than drain_delay + timeout
  containers:
    - name: prism
      args: ["-config", "/etc/prism/prism.yaml", "-log-format", "json"]
      readinessProbe:
        httpGet: { path: /readyz, port: 8080 }
        periodSeconds: 2
        failureThreshold: 3
      livenessProbe:
        httpGet: { path: /live, port: 9901 }   # the admin listener
        periodSeconds: 10
```

Use `ready_path` or the admin API's `/ready` for readiness, and `/live` for liveness, never the other way round. A draining Prism isn't ready, but it's alive. A liveness probe that failed during a drain would get it killed with requests still running. If the admin API needs to be reachable by the kubelet, its address isn't loopback, so `admin.auth` is required. `/ready` and `/live` skip auth.

## Changing config

1. **Validate** the new file: `prism -config new.yaml -validate`. It builds everything, so a bad certificate or CA file fails here.
2. **Look at the diff.** If it touches `listeners`, `admin`, `shutdown` or `ready_path`, a reload won't apply that part. Prism logs `setting changed, restart to apply it` and keeps the old value.
3. **Reload:** `systemctl reload prism`, `kill -HUP`, or `POST /reload` on the admin API, which answers `500` with the reason if it fails.
4. **Check:** `prism_config_last_reload_successful` should be 1, and `GET /config` shows what's actually running.

Write the new file to a temporary name and `mv` it into place. A reload that reads a half-written file fails, and the old config keeps serving, but that's still a failed reload.

What a reload keeps: each backend's health and in-flight count, each route's rate limiter, concurrency slots and retry budget while their settings are unchanged, and the cache. The [hot reload concept page](../concepts/hot-reload-and-shutdown.md#the-catch-state-has-to-survive) explains why each one matters.

## Tuning

Start from the defaults, which suit most setups, and change things when a metric tells you to.

| what | default | when to change it | how to tell |
| --- | --- | --- | --- |
| `upstreams.*.timeouts.response_header` | 30s | Set it a little above the backend's slowest normal response. 30 s holds a connection and a concurrency slot for a long time when a backend hangs. | `prism_upstream_response_seconds` |
| `max_idle_conns_per_host` | 64 | Raise it if Prism keeps opening new connections to busy backends. | many sockets in `TIME_WAIT` (`ss -s`) |
| `max_concurrency`, `queue_size` | off | Set it on routes to backends that degrade under too much concurrent work. By Little's law, the concurrency you need is throughput times latency: 2,000 req/s at 50 ms is 100. | `prism_route_concurrency_in_use`, `kind="overloaded"` errors |
| `rate_limit` | off | Per client IP. | `kind="rate_limited"` errors |
| `retry` | off | `attempts: 1` or `2`, on `connect_error` at least. Leave the budget on. | `prism_retries_total`, `prism_retry_budget_exhausted_total` |
| health checks | off | Turn them on for any upstream with more than one backend. | `prism_backend_healthy` |
| `cache.max_size` | 64MB | Raise it if evictions are steady while the hit ratio is low. It's memory. | `prism_cache_evictions_total` |
| `-log-buffer` | 0 | `64KB` cuts the cost of the access log, at the risk of losing a second of lines in a crash. | CPU profile |

### The Go runtime

- **`GOMAXPROCS`.** Since Go 1.25, Go reads the container's CPU limit, so Prism uses the CPUs it's allowed and no more. Prism's `go.mod` says `go 1.27.0`, so this is on. Nothing to set.
- **`GOGC`** sets how far the heap may grow between garbage collections, and **`GOMEMLIMIT`** sets a soft cap on memory. A proxy without a cache has little live data, so a higher `GOGC` trades a little memory for less GC work. With a large cache, the live heap is big, and a high `GOGC` can let the process grow to several times that. Set `GOMEMLIMIT` instead, below the container's memory limit. Neither has been measured on Prism, so benchmark before relying on either. See [Performance](../concepts/performance.md#tuning-go-itself).

## What to alert on

Alert on what hurts users and on what you'd need to act on. Everything else belongs on a dashboard. These rules are a starting point:

```yaml
groups:
  - name: prism
    rules:
      - alert: PrismDown
        expr: up{job="prism"} == 0
        for: 1m
        labels: { severity: page }
        annotations:
          summary: "Prometheus can't scrape Prism on {{ $labels.instance }}"

      - alert: PrismUpstreamNoBackends
        expr: prism_upstream_available_backends == 0
        for: 30s
        labels: { severity: page }
        annotations:
          summary: "Upstream {{ $labels.upstream }} has no backend that can take requests; clients get 503"

      - alert: PrismUpstreamDegraded
        expr: |
          prism_upstream_available_backends
            / count by (instance, job, upstream) (prism_backend_healthy)
          < 0.5
        for: 5m
        labels: { severity: ticket }
        annotations:
          summary: "Upstream {{ $labels.upstream }} has fewer than half its backends available"

      - alert: PrismHigh5xxRatio
        expr: |
          sum by (instance, job, route) (rate(prism_requests_total{code="5xx"}[5m]))
            / sum by (instance, job, route) (rate(prism_requests_total[5m]))
          > 0.05
          and sum by (instance, job, route) (rate(prism_requests_total[5m])) > 1
        for: 5m
        labels: { severity: page }
        annotations:
          summary: "Route {{ $labels.route }} answers more than 5% of requests with 5xx"

      - alert: PrismHighLatency
        expr: |
          histogram_quantile(0.99,
            sum by (instance, job, route, le) (rate(prism_request_duration_seconds_bucket[5m])))
          > 1
          and sum by (instance, job, route) (rate(prism_request_duration_seconds_count[5m])) > 1
        for: 10m
        labels: { severity: ticket }
        annotations:
          summary: "Route {{ $labels.route }} p99 latency is over 1s"

      - alert: PrismSheddingLoad
        expr: sum by (instance, job, route) (rate(prism_proxy_errors_total{kind="overloaded"}[5m])) > 0
        for: 5m
        labels: { severity: ticket }
        annotations:
          summary: "Route {{ $labels.route }} is refusing requests at max_concurrency"

      - alert: PrismRetryBudgetExhausted
        expr: sum by (instance, job, route) (rate(prism_retry_budget_exhausted_total[5m])) > 0
        for: 5m
        labels: { severity: ticket }
        annotations:
          summary: "Route {{ $labels.route }} is failing enough that its retry budget is used up"

      - alert: PrismBreakerOpen
        expr: prism_backend_breaker_state == 2
        for: 5m
        labels: { severity: ticket }
        annotations:
          summary: "Circuit breaker for {{ $labels.backend }} in {{ $labels.upstream }} has been open for 5 minutes"

      - alert: PrismConfigReloadFailed
        expr: prism_config_last_reload_successful == 0
        for: 1m
        labels: { severity: ticket }
        annotations:
          summary: "The last config reload on {{ $labels.instance }} failed; the previous config is still serving"

      - alert: PrismRateLimiterFull
        expr: increase(prism_ratelimit_evictions_total[1h]) > 0
        labels: { severity: info }
        annotations:
          summary: "The {{ $labels.limit }} limiter on {{ $labels.route }} is tracking as many clients as it can"

      - alert: PrismFileDescriptorsHigh
        expr: process_open_fds{job="prism"} / process_max_fds{job="prism"} > 0.8
        for: 5m
        labels: { severity: ticket }
        annotations:
          summary: "Prism on {{ $labels.instance }} is using over 80% of its file descriptor limit"
```

The thinking behind them:

- **Page on what users feel:** Prism down, an upstream with nothing to send to, a high error ratio. The `> 1` request-rate condition stops one failed request on a quiet route from paging anyone.
- **Ticket on what will hurt soon:** half the backends gone, load being shed, the retry budget used up, a breaker stuck open, a config that failed to load. A failed reload changes nothing for users right now, but the next restart would fail on the same file.
- **The 5xx ratio counts `503`s Prism makes itself,** such as `no_backend` and `overloaded`, not only backend errors. `prism_proxy_errors_total{kind}` says which.
- **`for:`** makes a condition last before it fires, so a single scrape doesn't page anyone. Shorter for user-facing conditions, longer for slow ones.

### How these rules were checked

`promtool check rules` accepted all 11. Then a real Prometheus (v3.5.0, in Docker) scraped a running Prism with them, at 50 requests a second. Both backends were made to fail, and a broken config was reloaded. After 65 seconds:

```text
firing   PrismConfigReloadFailed
firing   PrismUpstreamNoBackends    app
pending  PrismBreakerOpen           http://127.0.0.1:9101
pending  PrismBreakerOpen           http://127.0.0.1:9102
pending  PrismHigh5xxRatio          /
pending  PrismUpstreamDegraded      app
```

`pending` means the condition is true and its `for:` hasn't run out yet. After the backends recovered and a good config was reloaded, only `PrismHigh5xxRatio` was left pending, since its 5-minute rate still covered the outage. Stopping Prism brought `PrismDown` to pending. The rules for latency, load shedding, the retry budget, rate limiter evictions and file descriptors weren't triggered in this test, only syntax-checked.

### A dashboard

Alongside the alerts, one dashboard row per upstream answers most questions:

- request rate by `code`, from `prism_requests_total`
- p50 and p99 from `prism_request_duration_seconds`, next to `prism_upstream_response_seconds`, to tell Prism's time from the backend's
- `prism_upstream_available_backends`, and `prism_backend_inflight` per backend
- `prism_proxy_errors_total` by `kind`
- the cache hit ratio, for caching routes

[Observability](../concepts/observability.md#what-prism-exports) has the PromQL for these.

## When something goes wrong

| symptom | look at | likely cause |
| --- | --- | --- |
| `502`, `error=connect` | `GET /upstreams`, the backend itself | the backend is down or refusing connections |
| `504`, `error=timeout` | `prism_upstream_response_seconds` | the backend is slow, or `response_header` is too short |
| `503`, `error=no_backend` | `GET /upstreams`: `healthy`, `ejected`, `breaker` | every backend is down, ejected or behind an open breaker |
| `503`, `error=overloaded` | `prism_route_concurrency_in_use`, `prism_route_queue_depth` | the route is at `max_concurrency`, maybe because the backend slowed down |
| `429`s from one IP for everyone | the access log's `client` field | `trusted_proxies` doesn't include the load balancer |
| a config change didn't take effect | the log, `GET /config` | a failed reload, or a restart-only setting |
| `499`s | the access log's `duration` | clients giving up first: a slow backend, or a client timeout shorter than Prism's |
| memory growing | `prism_cache_bytes`, `go_memstats_heap_inuse_bytes`, `/debug/pprof/heap` | the cache filling up to `max_size` is normal |

Every response from Prism's proxy handler has an `X-Request-Id`. When a user reports a failure with that ID, `grep` the access log for it, and for its `WARN` line if there is one. [Status codes](../reference/status-codes.md) lists every error kind.

For CPU or memory questions, turn on `admin.pprof` (a restart-only setting) and take a profile. See [Observability](../concepts/observability.md#profile-a-live-process).

## Before you rely on it

Known gaps, so nothing here surprises you in production:

- **It hasn't served production traffic.** The testing is thorough (see [Testing](testing.md)), but real traffic finds what tests don't.
- **The cache doesn't invalidate on unsafe methods.** A `POST` or `DELETE` to a URL leaves its cached `GET` in place. Use `POST /cache/purge`, or keep caching to responses that can be stale for their `max-age`.
- **The auth failure limit also refuses correct passwords.** Once an IP is over `failure_limit`, it gets `429` until the limit refills, right password or not. Behind a NAT, many users share that IP.
- **The latency tail isn't explained.** In the benchmarks, Prism's maximum latency is 13 to 28 ms, against 5 ms or less for nginx. Garbage collection is the likely cause, but that hasn't been confirmed.
- **The benchmark numbers are from before M8**, the last round of changes. Rerun `make bench-compare` before quoting them.
- **No trace exporter.** Prism passes trace IDs along and logs them, but sends no spans anywhere.
- **No OCSP stapling, ACME or automatic certificate renewal.** Certificates reload with the config, so renew them with another tool and reload.

## Check yourself

1. Why does the systemd unit run `-validate` before sending `SIGHUP`, when a bad config can't break a running Prism anyway?
2. Your load balancer checks every 5 s and gives up after 2 failures. What's the shortest safe `drain_delay`?
3. Why would pointing a liveness probe at `ready_path` cause dropped requests during deploys?
4. All your `429`s come from the same client IP. What's the first config setting to check?
5. Why is `PrismConfigReloadFailed` a ticket and not a page?

Back to the [docs index](../README.md).
