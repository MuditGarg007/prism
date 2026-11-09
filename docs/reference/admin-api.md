# Admin API reference

The admin API shows Prism's live state and takes a few actions. It runs on its own listener, set by [`admin.listen`](config.md#admin), so client routes can never reach it. The code is in `internal/admin`.

The examples on this page are real output from this config, with `scripts/lab-backends.sh` running and 20 requests sent to `/app/`:

```yaml
listeners:
  - addr: "127.0.0.1:8080"
admin: { listen: "127.0.0.1:9901" }
upstreams:
  app:
    balancer: least_conn
    backends:
      - { url: "http://127.0.0.1:9101" }
      - { url: "http://127.0.0.1:9102", weight: 2 }
    health: { path: /healthz, interval: 5s }
    circuit_breaker: {}
  canary: { backends: [{ url: "http://127.0.0.1:9103" }] }
routes:
  - match: { path_prefix: "/app/" }
    upstream: app
    max_concurrency: 100
    queue_size: 50
    rate_limit: { per_ip: 100/s }
    retry: { attempts: 2, on: [connect_error, "503"] }
    canary: { upstream: canary, percent: 10, header: X-Canary }
    cache: { enabled: true }
```

## Endpoints

| method and path | does | needs |
| --- | --- | --- |
| [`GET /routes`](#get-routes) | the routes, with live concurrency and shadow use | |
| [`GET /upstreams`](#get-upstreams) | each backend's live state | |
| [`GET /config`](#get-config) | the effective config, with defaults filled in | |
| [`GET /cache/stats`](#get-cachestats) | cache size and result counts | a caching route |
| [`POST /cache/purge`](#post-cachepurge) | delete cached responses | a caching route |
| [`POST /reload`](#post-reload) | reload the config file, as `SIGHUP` does | |
| [`GET /metrics`](#get-metrics) | Prometheus metrics | |
| [`GET /ready`, `GET /live`](#get-ready-and-get-live) | probes | no auth |
| [`/debug/pprof/`](#debugpprof) | Go's profiler | `admin.pprof: true` |

### Conventions

- Bodies are JSON, except `/metrics`, `/ready`, `/live` and pprof.
- Every response carries `Cache-Control: no-store`.
- An error is a JSON object with one field: `{"error": "..."}`.
- An unknown path is `404`, and a known path with the wrong method is `405` with an `Allow` header. Both are Go's plain-text answers.
- Admin requests aren't written to the access log.

### Auth

With [`admin.auth`](config.md#auth) set, every endpoint except `/ready` and `/live` needs credentials, the same way a route does. A request without valid ones gets `401`, with `WWW-Authenticate` for Basic auth. After too many wrong ones from an IP, it gets `429` with `Retry-After`, before any password is checked.

The admin listener is reached directly, so the client IP is the TCP peer. `X-Forwarded-For` is never trusted here.

Without `admin.auth`, `admin.listen` must be a loopback address, or the config fails validation.

## GET /routes

The routes, in config order. Optional parts appear only when the route has them. Its full settings are in `GET /config`.

```console
$ curl -s 127.0.0.1:9901/routes
```

```json
{
    "routes": [
        {
            "name": "/app/",
            "host": "",
            "path_prefix": "/app/",
            "upstream": "app",
            "cache": true,
            "concurrency": {
                "in_use": 0,
                "limit": 100,
                "queued": 0
            },
            "canary": {
                "upstream": "canary",
                "percent": 10,
                "header": "X-Canary"
            }
        }
    ]
}
```

| field | meaning |
| --- | --- |
| `name` | host plus path prefix, the `route` label in metrics |
| `host` | `""` matches any host |
| `cache` | whether the route caches |
| `concurrency` | only with `max_concurrency`: slots in use, the limit, and requests queued for a slot |
| `access` | the route's allow and deny lists, if it has one |
| `auth` | `["basic"]`, `["api_keys"]` or both. Users and keys are in `GET /config`, without their hashes. |
| `canary` | upstream, percent, and the override header or cookie |
| `shadow` | upstream, percent, and copies `in_flight` against their `limit` |
| `websocket` | `{"idle_timeout": "5m0s"}` |

## GET /upstreams

Each upstream's live state, sorted by name, and its backends in config order.

```console
$ curl -s 127.0.0.1:9901/upstreams
```

```json
{
    "upstreams": [
        {
            "name": "app",
            "balancer": "least_conn",
            "available": 2,
            "backends": [
                {
                    "url": "http://127.0.0.1:9101",
                    "weight": 1,
                    "inflight": 0,
                    "healthy": true,
                    "ejected": false,
                    "breaker": "closed",
                    "available": true
                },
                {
                    "url": "http://127.0.0.1:9102",
                    "weight": 2,
                    "inflight": 0,
                    "healthy": true,
                    "ejected": false,
                    "breaker": "closed",
                    "available": true
                }
            ]
        },
        {
            "name": "canary",
            "balancer": "round_robin",
            "available": 1,
            "backends": [
                {
                    "url": "http://127.0.0.1:9103",
                    "weight": 1,
                    "inflight": 0,
                    "healthy": true,
                    "ejected": false,
                    "available": true
                }
            ]
        }
    ]
}
```

| field | meaning |
| --- | --- |
| `available` (upstream) | how many backends the balancer may pick |
| `url` | the backend, with any password removed |
| `inflight` | requests being forwarded to it now |
| `healthy` | health checks pass, or the upstream has none |
| `ejected` | outlier detection has ejected it |
| `breaker` | `closed`, `half_open` or `open`. Absent without a circuit breaker, as on `canary` above. |
| `available` (backend) | healthy, not ejected, and breaker not open |

## GET /config

The config that's running: the file as loaded, with every default filled in. Field names and value formats match the file, so durations read `"30s"` rather than nanoseconds. Secrets are hidden: a password in a backend URL is masked, and the values of `basic.users` and `api_keys.keys` are replaced by `"[redacted]"`, keeping the names.

After a reload that changed a restart-only setting, such as `listeners`, this shows the value still in use, not the one in the file.

```console
$ curl -s 127.0.0.1:9901/config
```

An excerpt:

```json
{
    "upstreams": {
        "app": {
            "balancer": "least_conn",
            "health": {
                "path": "/healthz",
                "interval": "5s",
                "timeout": "2s",
                "unhealthy_after": 3,
                "healthy_after": 2
            },
            "circuit_breaker": {
                "failure_ratio": 0.5,
                "min_requests": 20,
                "open_for": "30s",
                "window": "10s",
                "half_open_probes": 3
            },
            "max_idle_conns_per_host": 64
        }
    },
    "routes": [
        {
            "rate_limit": {
                "per_ip": "100/s",
                "burst": 100
            },
            "queue_timeout": "1s",
            "retry": {
                "attempts": 2,
                "on": ["connect_error", "503"],
                "budget_ratio": 0.2,
                "budget_min_per_sec": 10,
                "backoff": { "base": "25ms", "max": "250ms" }
            }
        }
    ]
}
```

It's a quick way to check what a default turned out to be.

## GET /cache/stats

The shared cache, with counters that only grow. `GET /metrics` reports the same numbers as `prism_cache_*`. Without any caching route, this answers `404` with `{"error":"no route has caching enabled"}`.

```console
$ curl -s 127.0.0.1:9901/cache/stats
{"entries":3,"bytes":2517,"max_bytes":67108864,"evictions":0,"waiting":0,"hit":17,"miss":23,"stale":0,"revalidated":0,"bypass":0,"updating":0}
```

| field | meaning |
| --- | --- |
| `entries`, `bytes`, `max_bytes` | what the cache holds, against `cache.max_size` |
| `evictions` | entries removed to make room |
| `waiting` | requests waiting now for another request's fetch of the same key |
| `hit`, `miss`, `stale`, `revalidated`, `bypass`, `updating` | requests by [`X-Cache`](status-codes.md#x-cache) result |

## POST /cache/purge

Deletes cached responses for a host and path, every variant, method and scheme included.

| field | type | meaning |
| --- | --- | --- |
| `host` | string | required. Lowercased, and `:80` or `:443` dropped, to match the cache's keys. |
| `path` | string | required, starting with `/`. Includes the query, if any. |
| `prefix` | bool | purge every path that starts with `path` |

```console
$ curl -s -X POST 127.0.0.1:9901/cache/purge -d '{"host":"127.0.0.1:8080","path":"/app/","prefix":true}'
{"purged":3}
$ curl -s -X POST 127.0.0.1:9901/cache/purge -d '{"host":"127.0.0.1:8080","path":"app"}'
{"error":"\"host\" is required and \"path\" must start with \"/\""}
```

The second one answered `400`. So does a body with unknown fields, trailing data, or more than 64 KB. Without a caching route, the answer is `404`. Each purge is logged at `INFO`.

## POST /reload

Reloads the config file, as `SIGHUP` does. The new config is parsed, validated and built in full before it replaces the old one.

```console
$ curl -s -X POST 127.0.0.1:9901/reload
{"reloaded":true}
```

A failed reload answers `500`, and the previous config keeps serving:

```console
$ curl -s -X POST 127.0.0.1:9901/reload
{"error":"reload failed: ref.yaml: invalid config:\n  routes[0].canary.percent: must be between 0 and 100, got 150"}
```

Either way the result is counted in `prism_config_reloads_total`, and a failure sets `prism_config_last_reload_successful` to 0. See [Hot reload and shutdown](../concepts/hot-reload-and-shutdown.md).

## GET /metrics

Prometheus metrics in the text format. See the [metrics reference](metrics.md).

## GET /ready and GET /live

Probes for load balancers and orchestrators. They answer before auth, and say nothing about the config.

| path | while serving | while draining |
| --- | --- | --- |
| `/ready` | `200 ready` | `503 draining` |
| `/live` | `200 live` | `200 live` |

Point readiness checks at `/ready` and liveness checks at `/live`. A liveness check that failed during a drain would get Prism killed before its requests finished. The top-level [`ready_path`](config.md#top-level) answers the same way on the client listeners.

## /debug/pprof/

With `admin.pprof: true`, Go's profiler is served under `/debug/pprof/`: the index, `cmdline`, `profile` (CPU), `symbol`, `trace`, and the named profiles (`heap`, `goroutine`, `allocs`, `block`, `mutex`, `threadcreate`) from the index. Without it, these paths are `404`.

```sh
curl -s -o cpu.prof '127.0.0.1:9901/debug/pprof/profile?seconds=10'
go tool pprof -top bin/prism cpu.prof
```

A CPU profile or execution trace slows the process while it runs. See [Observability](../concepts/observability.md#profile-a-live-process).
