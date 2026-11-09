# Config reference

This page lists every setting in Prism's YAML config file, with its default and the rules validation checks. For a commented example, see [`examples/prism.yaml`](../../examples/prism.yaml). For why a setting exists, follow the links to the concept pages.

The code behind this page is `internal/config`: `config.go` has the types, `defaults.go` has the defaults and `validate.go` has the rules.

## How the file is read

`config.Load` reads the file in four steps.

1. **Strict decode.** An unknown key is an error, so a typo such as `max_concurency` fails instead of being ignored.
2. **Defaults.** `applyDefaults` fills in every setting left out. It only replaces zero values, so a value that is wrong, such as a negative weight, is left for validation to report.
3. **Paths.** Relative file paths (`cert`, `key`, `ca_file`) are resolved against the directory the config file is in, not the directory Prism runs from.
4. **Validation.** Every problem is collected and reported together, each with the path of the field it refers to:

```console
$ ./bin/prism -config ref.yaml -validate
prism: ref.yaml: invalid config:
  upstreams.app.health.timeout: must be less than interval (1s)
```

That error came from setting `health: { path: /healthz, interval: 1s }`. The default `timeout` is 2s, which is longer than the interval. Set both together.

`-validate` goes further than parsing: it builds the whole runtime, so a certificate or CA file that doesn't parse fails here too, as with `nginx -t`.

### Zero means "use the default"

Because defaults replace zero values, you can't set a setting to `0` when it has a default. `failure_ratio: 0` becomes `0.5`, and `shutdown.timeout: 0` becomes 15s. Settings where `0` has a meaning of its own say so in the tables below.

## Value formats

| kind | written as | notes |
| --- | --- | --- |
| duration | `500ms`, `5s`, `1m30s`, `1.5h` | Go's `time.ParseDuration`. A bare number like `5` is an error, not 5 seconds. |
| size | `1500B`, `64KB`, `1MB`, `2GB` | Whole numbers only (`1.5MB` is an error). Units are powers of 1024, and case and spaces don't matter (`64 mb` works). A number without a unit is bytes. |
| rate | `100/s`, `600/m`, `5000/h` | A positive whole number of events per second, minute or hour. |
| prefix | `10.0.0.0/8`, `::1`, `127.0.0.1` | A CIDR, or a bare address meaning one host. IPv4-mapped IPv6 prefixes are turned into IPv4. |
| header name | `X-API-Key` | Must be a valid HTTP token. Hop-by-hop headers (`Connection`, `Upgrade`, `Transfer-Encoding` and so on) are refused, since the proxy always removes them. |

## Top level

```yaml
trusted_proxies: [10.0.0.0/8]
forwarded_header: false
listeners: [...]
upstreams: {...}
routes: [...]
ready_path: /readyz
shutdown: {...}
cache: {...}
admin: {...}
access: {...}
```

| key | type | default | rules and meaning |
| --- | --- | --- | --- |
| `trusted_proxies` | list of prefixes | none | Peers whose `X-Forwarded-*` headers and `X-Request-Id` are kept. From anyone else, they're replaced. See [Security](../concepts/security.md#trust-boundaries). |
| `forwarded_header` | bool | `false` | Also send an RFC 7239 `Forwarded` header to backends. |
| `listeners` | list | required | At least one. See [listeners](#listeners). |
| `upstreams` | map of name to upstream | required | At least one. See [upstreams](#upstreams). |
| `routes` | list | required | At least one. See [routes](#routes). |
| `ready_path` | path | none | Answered on every listener before routing: `200 ready` while serving, `503 draining` once shutdown has begun. Must start with `/` and contain no `?` or `#`. Not written to the access log. |
| `shutdown` | object | | See [shutdown](#shutdown). |
| `cache` | object | | The shared response cache. See [cache](#cache-the-shared-store). |
| `admin` | object | none | Serves the admin API on its own listener. See [admin](#admin). |
| `access` | object | none | Filters every request by client IP, before routing. See [access](#access). |

### What a reload can't change

A reload (`SIGHUP` or `POST /reload`) applies everything except `listeners`, `admin`, `shutdown` and `ready_path`, which are tied to sockets opened at startup. If one of these changes, `keepRestartOnly` (`internal/runtime/reloader.go`) keeps the running value and logs `setting changed, restart to apply it`. Two exceptions: listener certificates reload, and the cache's `max_size` resizes the running cache. See [Hot reload and shutdown](../concepts/hot-reload-and-shutdown.md).

## listeners

```yaml
listeners:
  - addr: ":8443"
    tls:
      certs:
        - { cert: certs/example.pem, key: certs/example-key.pem }
    timeouts: { read_header: 10s, idle: 120s }
```

| key | type | default | rules and meaning |
| --- | --- | --- | --- |
| `addr` | `host:port` | required | An empty host listens on every interface. Two listeners can't have the same address, except port `0`, which picks a free port. |
| `tls.certs` | list of `{cert, key}` | | At least one pair, and both files must exist. The certificate is chosen by SNI: exact names first, then wildcards, and the first pair is the fallback. TLS 1.2 or later. TLS listeners also speak HTTP/2. |
| `timeouts.read_header` | duration | `10s` | How long a client has to send the request headers. Guards against slowloris. |
| `timeouts.idle` | duration | `120s` | How long a keep-alive connection may sit idle between requests. |
| `h2c` | bool | `false` | Also accept HTTP/2 without TLS from clients that start with it ("prior knowledge"). An error on a TLS listener. |

There's deliberately no timeout for reading a whole request or writing a whole response: either one would cut off long uploads and streams. See [Timeouts and retries](../concepts/timeouts-and-retries.md).

## upstreams

```yaml
upstreams:
  api:
    balancer: least_conn
    backends:
      - { url: "http://10.0.1.10:8080", weight: 2 }
      - { url: "https://10.0.1.11:8443", tls: { server_name: api.internal, ca_file: ca.pem } }
    timeouts: { connect: 5s, response_header: 30s, idle_conn: 90s }
    max_idle_conns_per_host: 64
    health: {...}
    circuit_breaker: {...}
    outlier: {...}
```

| key | type | default | rules and meaning |
| --- | --- | --- | --- |
| `balancer` | string | `round_robin` | `round_robin`, `weighted`, `least_conn` or `ip_hash`. See [Load balancing](../concepts/load-balancing.md). |
| `backends` | list | required | At least one, with no duplicate URLs. |
| `backends[].url` | URL | required | `http://` or `https://`, with a host, and no path, query or fragment. A password in the URL is masked in `GET /config`. |
| `backends[].weight` | int | `1` | At least 1. Used by `weighted`, `least_conn` and `ip_hash`, and ignored by `round_robin`. |
| `backends[].tls.server_name` | string | the URL's host | The name the backend's certificate is checked against. Needed when the URL has an IP address. |
| `backends[].tls.ca_file` | path | system roots | Trust only this CA. The file must exist. A `tls` block on an `http://` backend is an error. |
| `timeouts.connect` | duration | `5s` | Opening a TCP (and TLS) connection to a backend. |
| `timeouts.response_header` | duration | `30s` | From sending the request to receiving the response headers. Doesn't limit how long the body takes. |
| `timeouts.idle_conn` | duration | `90s` | How long an unused pooled connection to a backend is kept. |
| `max_idle_conns_per_host` | int | `64` | At least 1. Idle keep-alive connections kept per backend. Go's own default is 2, which forces new connections under any real concurrency. |

### health

Active health checks. Each backend is sent `GET path` every `interval`. Leave the block out to skip health checks, and backends count as healthy. See [Detecting failure](../concepts/detecting-failure.md#health-checks).

| key | type | default | rules and meaning |
| --- | --- | --- | --- |
| `path` | path | required | Must start with `/`. |
| `interval` | duration | `10s` | Time between checks. |
| `timeout` | duration | `2s` | Must be less than `interval`. |
| `unhealthy_after` | int | `3` | At least 1. Consecutive failures that mark a backend down. |
| `healthy_after` | int | `2` | At least 1. Consecutive passes that mark it up again. |

### circuit_breaker

A breaker per backend that opens when too many requests fail. `circuit_breaker: {}` turns it on with every default. See [Detecting failure](../concepts/detecting-failure.md#circuit-breakers).

| key | type | default | rules and meaning |
| --- | --- | --- | --- |
| `failure_ratio` | number | `0.5` | Above 0 and at most 1. The share of failures in `window` that opens the breaker. |
| `min_requests` | int | `20` | At least 1. Fewer requests than this in the window never open it. |
| `window` | duration | `10s` | At least 1s. The sliding window, kept as 10 buckets. |
| `open_for` | duration | `30s` | How long the breaker stays open before letting probes through. |
| `half_open_probes` | int | `3` | At least 1. Requests let through at once to test the backend. All must succeed to close the breaker. |

### outlier

Ejects a backend after consecutive failures, for `base_ejection` times the number of times it has been ejected, up to `max_ejection`. `outlier: {}` turns it on with every default.

| key | type | default | rules and meaning |
| --- | --- | --- | --- |
| `consecutive_failures` | int | `5` | At least 1. |
| `base_ejection` | duration | `30s` | |
| `max_ejection` | duration | `300s` | At least `base_ejection`. |
| `max_ejection_percent` | int | `50` | 1 to 100. The most of a pool that may be ejected at once, so ejection alone never empties it. |

## routes

Routes are matched by host first: an exact host, otherwise the most specific wildcard, otherwise the routes with no host. Within that group, the longest path prefix wins, and there's no fallback to another group. Prefixes are plain string prefixes, so `/api` also matches `/apiv2`; end a prefix with `/` to match whole segments. See [Routing](../concepts/routing.md). A route's name, used in logs, metrics and the admin API, is its host followed by its path prefix, such as `/api/` or `api.example.com/`.

```yaml
routes:
  - match: { host: "*.example.com", path_prefix: /api/ }
    upstream: api
    strip_prefix: true
    preserve_host: false
    headers: {...}
    max_body: 10MB
    rate_limit: { per_ip: 100/s, burst: 200 }
    max_concurrency: 512
    queue_size: 128
    queue_timeout: 1s
    retry: {...}
    cache: {...}
    access: {...}
    auth: {...}
    canary: {...}
    shadow: {...}
    websocket: {...}
```

| key | type | default | rules and meaning |
| --- | --- | --- | --- |
| `match.host` | string | any host | An exact name or a leading wildcard such as `*.example.com`, lowercased on load. `*` is allowed only as a leading `*.`. |
| `match.path_prefix` | path | `/` | Must start with `/`. No two routes may have the same host and prefix. |
| `upstream` | name | required | Must name an upstream. |
| `strip_prefix` | bool | `false` | Remove `path_prefix` from the path sent to the backend. |
| `preserve_host` | bool | `false` | Send the client's `Host` instead of the backend's. |
| `max_body` | size | `0`, no limit | A larger declared `Content-Length` gets `413` before any of the body is read. A body of unknown length is cut off at the limit. |
| `max_concurrency` | int | `0`, no limit | At least 0. Requests forwarded at once. Cache hits don't take a slot. |
| `queue_size` | int | `0` | At least 0. Requests that may wait for a slot. Needs `max_concurrency`. |
| `queue_timeout` | duration | `1s` if `queue_size` is set | How long a queued request waits before it's shed with `503`. An error without `queue_size`. |

See [Rate limits and overload](../concepts/rate-limits-and-overload.md) for the limits.

### headers

```yaml
headers:
  request_set: { X-Env: prod }
  request_remove: [X-Debug]
  response_set: { Strict-Transport-Security: "max-age=31536000" }
  response_remove: [Server]
```

| key | type | rules and meaning |
| --- | --- | --- |
| `request_set`, `response_set` | map of name to value | Set, replacing any existing value. Values can't contain CR, LF or NUL, which could split a message. |
| `request_remove`, `response_remove` | list of names | Removed. `Host` can't be removed: set it, or use `preserve_host`. |

### rate_limit

Per client IP, using GCRA.

| key | type | default | rules and meaning |
| --- | --- | --- | --- |
| `per_ip` | rate | required | The sustained rate. |
| `burst` | int | the rate's count | At least 1. How many requests may arrive at once. |

A client over the limit gets `429` with `Retry-After`.

### retry

| key | type | default | rules and meaning |
| --- | --- | --- | --- |
| `attempts` | int | `0` | 0 to 10. Retries after the first try, so `2` means up to 3 tries. `0` turns retries off. |
| `on` | list | `[connect_error]` | `connect_error`, `timeout`, or a status from `500` to `599` (quoted, as in `"503"`). |
| `budget_ratio` | number | `0.2` | Above 0 and at most 1. Over the last 10 seconds, retries may not exceed this share of requests, plus `budget_min_per_sec` for each second. |
| `budget_min_per_sec` | int | `10` | At least 1. |
| `backoff.base` | duration | `25ms` | Retry `n` waits a random time in `[0, min(max, base × 2ⁿ))`. |
| `backoff.max` | duration | `250ms` | At least `base`. |

Only some requests are retried, whatever `on` says:

- A connect error is retried for any method, since the backend never saw the request.
- A timeout or a status is retried only for idempotent methods: `GET`, `HEAD`, `OPTIONS`, `TRACE`, `PUT` and `DELETE`.
- A body can be sent again only if it was held in memory: its declared length is at most 64 KB. A longer body, or one of unknown length, can be retried only if the backend hadn't started reading it.

See [Timeouts and retries](../concepts/timeouts-and-retries.md).

### cache

Per-route caching settings. The store itself, shared by every caching route, is the top-level [`cache`](#cache-the-shared-store).

| key | type | default | rules and meaning |
| --- | --- | --- | --- |
| `enabled` | bool | `false` | Without it, the rest of the block is validated but does nothing. |
| `max_object` | size | `1MB` | At most the top-level `cache.max_size`. Larger responses pass through uncached. |
| `lock_timeout` | duration | `5s` | How long a request waits for another request's fetch of the same key before fetching for itself. |
| `bypass_header` | header name | none | A request with this header skips the lookup. The response may still be stored. |
| `stale_if_error` | duration | `0`, off | The most an entry may be served past its freshness when the backend fails. Also caps a response's own `stale-if-error`. |
| `stale_while_revalidate` | duration | `0`, off | The most an entry may be served past its freshness while one background request revalidates it. Also caps the response's own value. |

See [HTTP caching](../concepts/http-caching.md) and [Inside the cache](../concepts/inside-the-cache.md). Note that the cache doesn't invalidate entries when an unsafe method such as `POST` goes to the same URL; use `POST /cache/purge`.

### access

Allowed on the top level and on a route. The client IP is the one left after `trusted_proxies`.

| key | type | rules and meaning |
| --- | --- | --- |
| `allow` | list of prefixes | When not empty, only clients in it get through. |
| `deny` | list of prefixes | Clients in it are refused, even if `allow` lists them. |

At least one of the two must be set. A refused client gets `403`.

### auth

Allowed on a route and under `admin`. With both `basic` and `api_keys` set, either is accepted. See [Security](../concepts/security.md#passwords-basic-auth).

```yaml
auth:
  basic:
    realm: prism
    users:
      alice: "$2y$10$..."          # htpasswd -nbB alice password
  api_keys:
    header: X-API-Key
    keys:
      ci: "sha256:9f86d0..."       # printf %s "$KEY" | sha256sum
  forward_credentials: false
  identity_header: X-User
  failure_limit: 10/m
```

| key | type | default | rules and meaning |
| --- | --- | --- | --- |
| `basic.realm` | string | `prism` | No quotes, backslashes, CR, LF or NUL. |
| `basic.users` | map of user to bcrypt hash | required | At least one. Names can't be empty or contain colons or control characters. Each value must be a bcrypt hash. |
| `api_keys.header` | header name | `X-API-Key` | Where the key is read from. With `Authorization`, the key is read from `Authorization: Bearer <key>`. |
| `api_keys.keys` | map of name to digest | required | At least one. Each value is `sha256:` and 64 hex digits, the SHA-256 of the key. No two names may share a key. |
| `forward_credentials` | bool | `false` | Keep the checked `Authorization` or key header on the request to the backend. An error under `admin`. |
| `identity_header` | header name | none | Tells the backend who was let in: the user name or the key's name. Any copy from the client is removed. Can't be `Authorization` or the key header. An error under `admin`. |
| `failure_limit` | rate, or `off` | `10/m` | Wrong credentials allowed per client IP (per /64 for IPv6), with a burst of the rate's count. Over it, the client gets `429` before any password is checked. |

Prism warns at load time about a route with `basic` users, `failure_limit: off` and no `rate_limit`, since only bcrypt's cost would then slow password guessing. Once an IP is over the limit, it gets `429` even with the right password, until the limit refills.

### canary

Sends a share of clients to another upstream. See [Canary, shadow and WebSockets](../concepts/canary-shadow-websockets.md).

| key | type | default | rules and meaning |
| --- | --- | --- | --- |
| `upstream` | name | required | Must exist, and differ from the route's own upstream. |
| `percent` | number | `0` | 0 to 100. The share of clients, chosen by a hash of the client IP, so a client stays on one side while the percent is unchanged. |
| `header` | header name | none | A request with this header set to `always` or `never` overrides the split. |
| `cookie` | cookie name | none | The same, from a cookie. The header wins over the cookie. |

Canary requests skip the cache.

### shadow

Sends a copy of some requests to another upstream, and throws the response away.

| key | type | default | rules and meaning |
| --- | --- | --- | --- |
| `upstream` | name | required | Must exist, and differ from the route's own upstream. |
| `percent` | number | `100` | Above 0 and at most 100. |
| `max_body` | size | `64KB` | A request with a larger body, or one of unknown length, isn't copied. |
| `max_concurrency` | int | `16` | At least 1. A copy that finds this many already running is dropped. |
| `timeout` | duration | `5s` | For the whole copy, body included. |

Copies carry an `X-Prism-Shadow` header, never delay the real request, and are counted in `prism_shadow_requests_total`.

### websocket

| key | type | default | rules and meaning |
| --- | --- | --- | --- |
| `idle_timeout` | duration | `5m` | A WebSocket with no data in either direction for this long is closed. |

Without this block, a route doesn't let requests upgrade to WebSockets.

## shutdown

| key | type | default | rules and meaning |
| --- | --- | --- | --- |
| `drain_delay` | duration | `0` | How long Prism keeps serving after a `SIGTERM`, with `ready_path` answering `503`, so load balancers can notice. Can't be negative. |
| `timeout` | duration | `15s` | How long active requests get to finish once Prism stops accepting connections. |

A second signal skips both. See [Hot reload and shutdown](../concepts/hot-reload-and-shutdown.md#graceful-shutdown).

## cache: the shared store

| key | type | default | rules and meaning |
| --- | --- | --- | --- |
| `max_size` | size | `64MB` | Total bytes held, keys and headers included. Only used when some route has `cache.enabled`. A reload resizes the running cache, evicting entries if it shrinks. |

## admin

```yaml
admin:
  listen: "127.0.0.1:9901"
  pprof: false
  auth: {...}
```

| key | type | default | rules and meaning |
| --- | --- | --- | --- |
| `listen` | `host:port` | required | Can't be the same as a listener. Without `auth`, it must be a loopback address (`127.0.0.1`, `[::1]` or `localhost`), or validation fails. |
| `pprof` | bool | `false` | Serve Go's profiler under `/debug/pprof/`. A CPU profile or trace slows the process while it runs. |
| `auth` | [auth](#auth) | none | Credentials for every endpoint except `/ready` and `/live`. |

See the [admin API reference](admin-api.md).

## Command line

```text
prism [-config prism.yaml] [-validate] [-log-level info] [-log-format text] [-log-buffer 0] [-version]
```

| flag | default | meaning |
| --- | --- | --- |
| `-config` | `prism.yaml` | The config file. |
| `-validate` | | Load and build everything, print `configuration <path> is valid`, and exit. |
| `-log-level` | `info` | `debug`, `info`, `warn` or `error`. |
| `-log-format` | `text` | `text` or `json`. |
| `-log-buffer` | `0` | Buffer up to this much log output (a size, at most 64MB), flushed every second. `0` writes each line at once. A crash can lose up to a second of lines. |
| `-version` | | Print the version and exit. |

Exit codes: `0` for a clean exit, `1` for a config that fails to load, a port that can't be bound or an unclean shutdown, and `2` for bad flags or an unknown log level or format.

| signal | effect |
| --- | --- |
| `SIGHUP` | Reload the config. A failed reload keeps the old config serving. |
| `SIGTERM`, `SIGINT` | Graceful shutdown. A second one closes everything at once. |
