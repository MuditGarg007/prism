# Metrics reference

Prism serves Prometheus metrics at [`GET /metrics`](admin-api.md#get-metrics) on the admin listener. This page lists all of them. For what to do with them, see [Observability](../concepts/observability.md) and [Operating Prism](../guides/operating-prism.md#what-to-alert-on).

There are two sources in the code:

- **Request metrics** are counted as requests finish. They're defined in `internal/observability/metrics.go`, and survive reloads: a route or backend that still exists keeps counting, and one that's gone has its series deleted.
- **State metrics** are read from the running config each time Prometheus scrapes, by the `collector` in `internal/admin/metrics.go`. They always describe the config that's serving now.

## Labels

| label | values |
| --- | --- |
| `route` | the route's name: its host followed by its path prefix, such as `/api/` or `api.example.com/`. `""` for requests that matched no route. `admin` in `prism_ratelimit_evictions_total` for the admin API's own limiter. |
| `upstream` | the upstream's name from the config. `""` for requests that matched no route. A canary's requests carry the canary's upstream. |
| `backend` | the backend's URL, without any password |
| `code` | the status class: `1xx`, `2xx`, `3xx`, `4xx`, `5xx`, or `other` for a status outside 100 to 599 |
| `kind` | an error kind, from the list in [Status codes](status-codes.md#error-kinds) |
| `result` | depends on the metric, listed with it |

Labels never hold paths, client IPs or exact status codes, because each distinct value is a new series. The access log has those details.

## Requests

| metric | type | labels | meaning |
| --- | --- | --- | --- |
| `prism_requests_total` | counter | `route`, `upstream`, `code` | Requests served. A `499` (client gone) counts as `4xx`. |
| `prism_request_duration_seconds` | histogram | `route`, `upstream` | From receiving a request to finishing its response, body included. |
| `prism_upstream_response_seconds` | histogram | `upstream`, `backend` | From sending a request to a backend to receiving its response headers, for each attempt that got a response. |
| `prism_retries_total` | counter | `route` | Attempts beyond the first. |
| `prism_retry_budget_exhausted_total` | counter | `route` | Requests whose retry the budget refused. |
| `prism_proxy_errors_total` | counter | `route`, `kind` | Requests that failed or were refused, by the error kind the access log shows. A series appears the first time its kind happens. |
| `prism_shadow_requests_total` | counter | `route`, `result` | Shadow copies, by `result`: `sent` (a response came back, whatever its status), `error` (no response), `busy` (dropped at `max_concurrency`) or `body_skipped` (the body was too large, of unknown length, or not complete in time). Only for routes with a shadow. |

Both histograms use the same buckets, in seconds: 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10 and 30. For a WebSocket, `prism_request_duration_seconds` counts only the handshake, since the connection's whole lifetime would swamp the histogram. The access log's `duration` is the whole connection.

## Backends and upstreams

| metric | type | labels | meaning |
| --- | --- | --- | --- |
| `prism_backend_inflight` | gauge | `upstream`, `backend` | Requests being forwarded to the backend now. |
| `prism_backend_healthy` | gauge | `upstream`, `backend` | 1 if health checks pass or aren't configured, else 0. |
| `prism_backend_ejected` | gauge | `upstream`, `backend` | 1 if outlier detection has ejected the backend, else 0. |
| `prism_backend_breaker_state` | gauge | `upstream`, `backend` | 0 closed, 1 half-open, 2 open. Only for upstreams with a circuit breaker. Ordered by how much traffic each state refuses, so `> 0` means trouble. |
| `prism_upstream_available_backends` | gauge | `upstream` | Backends the balancer may pick: healthy, not ejected, and breaker not open. |

## Routes

| metric | type | labels | meaning |
| --- | --- | --- | --- |
| `prism_route_concurrency_in_use` | gauge | `route` | Concurrency slots taken. Only for routes with `max_concurrency`. |
| `prism_route_concurrency_limit` | gauge | `route` | The route's `max_concurrency`. |
| `prism_route_queue_depth` | gauge | `route` | Requests waiting for a slot. |
| `prism_shadow_inflight` | gauge | `route` | Shadow copies being sent. Only for routes with a shadow. |
| `prism_ratelimit_evictions_total` | counter | `route`, `limit` | Clients forgotten by a full per-client limiter before they went idle, each getting a fresh burst. `limit` is `rate_limit` or `auth_failures`. Nonzero means more distinct clients than the limiter can track. |

`prism_ratelimit_evictions_total` is read from the limiter itself, so a limiter replaced by a reload (because its settings changed) starts again from 0. Prometheus treats that as a counter reset, which `rate()` handles.

## Cache

Only when some route has `cache.enabled`.

| metric | type | labels | meaning |
| --- | --- | --- | --- |
| `prism_cache_requests_total` | counter | `result` | Requests on caching routes, by [`X-Cache`](status-codes.md#x-cache) result: `HIT`, `MISS`, `STALE`, `REVALIDATED`, `BYPASS` or `UPDATING`. |
| `prism_cache_entries` | gauge | | Responses held. |
| `prism_cache_bytes` | gauge | | Bytes held, keys and headers included. |
| `prism_cache_max_bytes` | gauge | | `cache.max_size`. |
| `prism_cache_evictions_total` | counter | | Entries evicted to make room. |
| `prism_cache_waiting` | gauge | | Requests waiting on another request's fetch of the same key. |

## Config reloads

| metric | type | labels | meaning |
| --- | --- | --- | --- |
| `prism_config_reloads_total` | counter | `result` | Reloads from `SIGHUP` or `POST /reload`, by `result`: `success` or `failure`. Both series exist from startup. |
| `prism_config_last_reload_successful` | gauge | | 1 when the last load, at startup or by a reload, succeeded. 0 when it failed and the previous config is still serving. |
| `prism_config_last_reload_success_timestamp_seconds` | gauge | | Unix time of the last successful load. |

## Go and the process

The standard collectors from `prometheus/client_golang` are registered too:

- `go_*`: the Go runtime, such as `go_goroutines`, `go_memstats_heap_inuse_bytes`, `go_gc_duration_seconds` and `go_sched_gomaxprocs_threads`. A build with Go 1.27 on Linux exports 29 of these.
- `process_*`: `process_cpu_seconds_total`, `process_resident_memory_bytes`, `process_open_fds`, `process_max_fds`, `process_start_time_seconds`, `process_virtual_memory_bytes`, `process_virtual_memory_max_bytes`, and on Linux `process_network_receive_bytes_total` and `process_network_transmit_bytes_total`.

## A sample

From the config on the [admin API page](admin-api.md), after 40 requests and one failed reload:

```console
$ curl -s 127.0.0.1:9901/metrics | grep -E '^prism_(requests_total|backend_breaker|upstream_available|config_|cache_requests_total\{result="HIT)'
prism_backend_breaker_state{backend="http://127.0.0.1:9101",upstream="app"} 0
prism_backend_breaker_state{backend="http://127.0.0.1:9102",upstream="app"} 0
prism_cache_requests_total{result="HIT"} 17
prism_config_last_reload_success_timestamp_seconds 1.7942097824249308e+09
prism_config_last_reload_successful 0
prism_config_reloads_total{result="failure"} 1
prism_config_reloads_total{result="success"} 1
prism_requests_total{code="2xx",route="",upstream=""} 0
prism_requests_total{code="2xx",route="/app/",upstream="app"} 40
prism_requests_total{code="2xx",route="/app/",upstream="canary"} 0
prism_upstream_available_backends{upstream="app"} 2
prism_upstream_available_backends{upstream="canary"} 1
```

The output also had `prism_requests_total` lines for the `1xx`, `3xx`, `4xx`, `5xx` and `other` classes, all 0, left out here. Each route has a series for every status class from the start, at 0, for both its upstream and its canary. That way `rate()` sees the first request instead of a series appearing out of nowhere.
