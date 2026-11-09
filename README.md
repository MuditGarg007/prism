# Prism

Prism is an nginx-style HTTP reverse proxy written in Go. It uses `net/http` for the server and `http.Transport` for the client, and writes the rest by hand: the forwarding layer (hop-by-hop headers, forwarded headers, streaming, cancellation, error mapping, retries, WebSocket upgrades), the load balancers, the rate limiter, the circuit breaker, the response cache and its request coalescing. It does not use `httputil.ReverseProxy`. Libraries are used only for YAML, Prometheus and bcrypt.

**New here?** The [docs](docs/README.md) explain how a reverse proxy works from the ground up, then take each part of Prism in turn: the problem, the idea, the code, an experiment and the tradeoffs.

## Features

- **Core proxy:** host and longest-prefix routing, TLS with SNI, header rules, X-Forwarded-* and RFC 7239 `Forwarded` with trusted proxies, streaming both ways, cancellation passed to the backend, graceful shutdown.
- **Traffic management:** round robin, smooth weighted round robin, least connections (power of two choices) and consistent-hash `ip_hash`; per-IP GCRA rate limits; concurrency caps with a bounded queue; body limits.
- **Resilience:** a circuit breaker per backend, outlier ejection, active health checks, and retries to untried backends under a per-route budget.
- **Caching:** an RFC 9111 response cache with Vary, revalidation, `stale-if-error`, `stale-while-revalidate`, request coalescing and purging.
- **Operations:** Prometheus metrics, access logs, request IDs, W3C `traceparent`, an admin API with auth, and SIGHUP reloads that drop no requests and keep health, breakers, pools and the cache when their settings are unchanged.
- **Routing and security:** IP allow and deny lists, Basic auth with bcrypt and API keys, a limit on failed logins, sticky canary splits, shadow traffic, WebSockets, and HTTP/2 on TLS and h2c.

## Quickstart

Prism needs Go 1.27.

```sh
make build                                        # bin/prism
./bin/prism -config examples/prism.yaml -validate # check a config
./bin/prism -config examples/prism.yaml -log-format json
kill -HUP $(pgrep prism)                          # reload the config
```

A minimal config:

```yaml
listeners:
  - addr: ":8080"

upstreams:
  api:
    balancer: least_conn
    backends:
      - { url: "http://10.0.0.1:8080" }
      - { url: "http://10.0.0.2:8080" }
    health: { path: /healthz, interval: 5s }

routes:
  - match: { path_prefix: "/api/" }
    upstream: api
    retry: { attempts: 2, on: [connect_error, timeout] }

admin: { listen: "127.0.0.1:9901" }
```

[`examples/prism.yaml`](examples/prism.yaml) uses every feature and explains each setting. [Run it](docs/learn/03-run-it.md) walks through a first session, and the [config reference](docs/reference/config.md) lists every key and flag.

| target | what it does |
| --- | --- |
| `make lint test` | `go vet`, staticcheck, gofmt, and every test with `-race` |
| `make fuzz` | nine fuzz targets, 30 s each |
| `make bench` | the Go benchmarks, with allocations |
| `make chaos` | the two chaos scenarios, with graphs (about 90 s) |
| `make reload` | a SIGHUP every 0.25 s under load, failing on any error |
| `make bench-compare` | the comparison with nginx (about 25 minutes; needs Docker) |

## Numbers

Against nginx 1.28 on one laptop, each proxy on four pinned CPUs, with a 1 KiB response. The median of three runs; full setup and every percentile are in [`docs/bench/results.md`](docs/bench/results.md).

| scenario | closed-loop req/s | added p50 / p99 at 5k req/s | CPU per request |
| --- | --: | --: | --: |
| nginx | 98,666 | +41 µs / +51 µs | 18.8 µs |
| prism, one backend | 68,822 | +84 µs / +256 µs | 45.1 µs |
| prism, every feature on | 66,145 | +84 µs / +240 µs | 48.3 µs |
| prism, cache hits | 99,234 | +12 µs / +96 µs | 22.1 µs |

- Prism uses about 2.4 times nginx's CPU per proxied request and 1.3 times per cache hit. Turning on every feature costs about 4% of throughput.
- Prism's maximum latency is 13–28 ms, against 5 ms or less for nginx. Garbage collection is the likely cause, but that hasn't been confirmed.
- These runs date from 2026-10-21, before the last round of hardening. Run `make bench-compare` again before quoting them. [Performance](docs/concepts/performance.md) explains how to read them.

Failure demos from `make chaos`, at 300 req/s:

- **[A backend killed and restarted](docs/m3-chaos/kill-backend.svg):** 12,001 requests and no errors. The breaker opened 0.4 s after the kill, and half-open probes closed it after the restart.
- **[All backends returning 503 for 20 s](docs/m3-chaos/outage.svg):** with 2 retries per request, the retry budget held load amplification to 1.23 during the outage, against 2.99 without it.

## Tuning the Go runtime

Prism sets neither `GOGC` nor `GOMEMLIMIT`. A proxy without a cache keeps little live data, so raising `GOGC` (to 400, say) trades some memory for less garbage-collection work. With a large cache the live heap is big, and a high `GOGC` can grow the process to several times it; set `GOMEMLIMIT` below the container's memory limit first, and raise `GOGC` under that ceiling. Neither setting has been measured on Prism, so benchmark before relying on one. See [operating Prism](docs/guides/operating-prism.md#the-go-runtime).

## Not supported

- HTTP/2 or HTTP/3 to backends (backends get HTTP/1.1), and HTTP/3 on either side.
- WebSockets over HTTP/2 (RFC 8441), and a WebSocket close frame at shutdown.
- Cache invalidation on `POST`, `PUT` or `DELETE` (RFC 9111, section 4.4); entries stay until they expire or are purged.
- Heuristic freshness, a disk cache, and caching HEAD misses.
- One trace span per attempt, a per-route canary split, ACME, adaptive concurrency limits and OpenTelemetry export.

The concept pages explain the reasoning behind most of these.

## Documentation

- [Start here](docs/README.md#start-here): five pages, read in order, from "what is a reverse proxy" to one request through the code.
- [Concepts](docs/README.md#concepts): routing, load balancing, rate limits, retries, failure detection, caching, security, reloads, observability and performance.
- [Guides](docs/README.md#guides): adding a feature, testing, and running Prism in production.
- [Reference](docs/README.md#reference): config, admin API, metrics and status codes.
