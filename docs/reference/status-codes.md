# Status codes reference

Most statuses a client sees come from the backend, passed through unchanged. This page lists the ones Prism produces itself, why, and how each shows up in the access log and metrics.

The code is `internal/proxy/errors.go` (`classify`, `fail` and `reject`) and the checks in `Handler.ServeHTTP` in `internal/proxy/proxy.go`.

## The list

In the order a request meets them:

| status | error kind | when | `Retry-After` |
| --- | --- | --- | --- |
| `400` | | Malformed request, answered by Go's `net/http` before Prism sees it | |
| `431` | | Request headers over 1 MB, answered by `net/http` | |
| `503` | | `ready_path` while draining, with body `draining` | |
| `403` | `forbidden` | The client IP is refused by the top-level `access` | |
| `400` | | The path has `.` or `..` segments or an empty segment, even encoded as `%2e` or `%2F`, with body `invalid request path` | |
| `404` | | No route matches the host and path | |
| `403` | `forbidden` | Refused by the route's `access` | |
| `429` | `rate_limited` | Over the route's `rate_limit` | until a request is allowed |
| `429` | `auth_limited` | Too many wrong credentials from this IP (`failure_limit`) | until the next attempt is allowed |
| `401` | `unauthorized` | Missing or wrong credentials, with `WWW-Authenticate` for Basic auth | |
| `413` | `body_too_large` | Declared `Content-Length` over `max_body`, before any of the body is read, or a body of unknown length passing it while it's sent | |
| `304` | | A cached entry matches the client's `If-None-Match` or `If-Modified-Since` | |
| `503` | `overloaded` | `max_concurrency` reached and the queue is full, or the wait passed `queue_timeout` | `1` |
| `400` | `client_body` | Reading the request body from the client failed | |
| `503` | `no_backend` | No backend can be picked: all are down, ejected, have an open breaker, or were already tried | |
| `502` | `connect` | Connecting to the backend failed, for example a refused connection | |
| `504` | `timeout` | Connecting or waiting for response headers passed `timeouts.connect` or `timeouts.response_header` | |
| `502` | `upstream` | Any other failure to get a response, such as a reset connection, or a `101` to a request that didn't ask for a WebSocket | |
| `500` | `internal` | Taking over the client connection for a WebSocket failed | |
| `499` | `client_closed` | The client went away first. Only logged, since nobody is left to send it to. | |

A `429` or `503` with `Retry-After` gives whole seconds, rounded up, so a client that waits that long is let in.

### When the backend answers

A backend's response is passed through as it is, whatever its status. With [`retry.on`](config.md#retry) listing a status such as `"503"`, Prism retries it, and if the retries run out, the client gets the last backend response. A `5xx` from the backend isn't an error kind: Prism forwarded it successfully.

There's one exception. On a caching route with `stale_if_error`, a backend `5xx` or a failure to get a response is replaced by the stale entry, sent with the entry's own status and `X-Cache: STALE`. The access log still records the error: `error=upstream` for a `5xx`, or the failure's own kind, such as `connect`. It's counted in `prism_proxy_errors_total` too, so the failure is visible even though the client didn't see it.

If the backend fails after the response headers were sent, the status can't change any more. Prism cuts the response off, so the client sees it fail rather than mistaking it for a complete one, and the access log line has `aborted=true`.

## Seeing them for real

This config produces each one:

```yaml
listeners: [{ addr: "127.0.0.1:8080" }]
upstreams:
  app:  { backends: [{ url: "http://127.0.0.1:9101" }] }
  slow:
    backends: [{ url: "http://127.0.0.1:9103" }]
    timeouts: { response_header: 200ms }
  dead: { backends: [{ url: "http://127.0.0.1:9199" }] }
  down:
    backends: [{ url: "http://127.0.0.1:9199" }]
    health: { path: /healthz, interval: 500ms, timeout: 200ms, unhealthy_after: 1 }
routes:
  - { match: { path_prefix: /app/ }, upstream: app }
  - { match: { path_prefix: /deny/ }, upstream: app, access: { deny: [127.0.0.1] } }
  - match: { path_prefix: /key/ }
    upstream: app
    auth: { api_keys: { keys: { ci: "sha256:1ec1c26b50d5d3c58d9583181af8076655fe00756bf7285940ba3670f99fcba0" } } }
  - { match: { path_prefix: /rl/ }, upstream: app, rate_limit: { per_ip: 1/m } }
  - { match: { path_prefix: /body/ }, upstream: app, max_body: 10B }
  - { match: { path_prefix: /dead/ }, upstream: dead }
  - { match: { path_prefix: /slow/ }, upstream: slow }
  - { match: { path_prefix: /down/ }, upstream: down }
  - { match: { path_prefix: /busy/ }, upstream: slow, max_concurrency: 1 }
```

With `scripts/lab-backends.sh` running and backend c made slow with `curl '127.0.0.1:9103/_fault?latency=1s'`, each request below printed its status and `Retry-After`:

```text
127.0.0.1:8080/nothing                       404
--path-as-is 127.0.0.1:8080/app/../etc       400
127.0.0.1:8080/deny/x                        403
127.0.0.1:8080/key/x                         401
-H X-API-Key: s3cret 127.0.0.1:8080/key/x    200
127.0.0.1:8080/rl/x                          200
127.0.0.1:8080/rl/x                          429 60
-d more than ten bytes 127.0.0.1:8080/body/x 413
127.0.0.1:8080/dead/x                        502
127.0.0.1:8080/slow/x                        504
127.0.0.1:8080/down/x                        503
127.0.0.1:8080/busy/x                        503 1
```

The `/busy/` request was sent while another one held the route's only slot. The access log has the error kind for each:

```text
path=/nothing status=404
path=/app/../etc status=400
path=/deny/x status=403 error=forbidden
path=/key/x status=401 error=unauthorized
path=/key/x status=200
path=/rl/x status=200
path=/rl/x status=429 error=rate_limited
path=/body/x status=413 error=body_too_large
path=/dead/x status=502 error=connect
path=/slow/x status=504 error=timeout
path=/down/x status=503 error=no_backend
path=/busy/x status=503 error=overloaded
path=/busy/x status=504 error=timeout
```

The last line is the request that held the slot, which timed out on the slow backend. A `431` never reaches the access log, because `net/http` answers it before any handler runs:

```console
$ python3 -c '
import socket
s = socket.create_connection(("127.0.0.1", 8080))
s.sendall(b"GET /app/x HTTP/1.1\r\nHost: x\r\nX-Big: " + b"a" * 1100000 + b"\r\n\r\n")
print(s.recv(200).split(b"\r\n")[0])'
b'HTTP/1.1 431 Request Header Fields Too Large'
```

## Error kinds

The `error` field of the access log line, and the `kind` label of `prism_proxy_errors_total`. The values are a fixed list, so they're safe to alert on.

| kind | status | own log line |
| --- | --- | --- |
| `forbidden` | 403 | none |
| `rate_limited` | 429 | none |
| `auth_limited` | 429 | none |
| `unauthorized` | 401 | none |
| `body_too_large` | 413 | `DEBUG` |
| `overloaded` | 503 | none |
| `client_body` | 400 | `DEBUG` |
| `no_backend` | 503 | `WARN` |
| `connect` | 502 | `WARN` |
| `timeout` | 504 | `WARN` |
| `upstream` | 502 | `WARN` |
| `internal` | 500 | `ERROR` |
| `client_closed` | 499 | `DEBUG` |

When a cache serves a stale entry instead, the kind is recorded with the entry's status, as described [above](#when-the-backend-answers).

Refusals get no log line of their own besides the access log entry. Under a flood, a warning per refused request would be a second flood. Failures that need attention get a separate line with the same `request_id`, and a retried attempt that failed gets `upstream attempt failed, retrying` at `WARN`.

## Headers on Prism's answers

| header | on | meaning |
| --- | --- | --- |
| `X-Request-Id` | every response from Prism's proxy handler, including its `400`, `403` and `404` | The request's ID, also sent to the backend and logged as `request_id`. Kept from the request only when it comes from a trusted proxy. |
| `X-Cache` | responses on caching routes | See below. |
| `Retry-After` | `429`, and `503 overloaded` | Seconds to wait. |
| `WWW-Authenticate` | `401` | `Basic realm="..."` for Basic auth, or `Bearer realm="..."` when API keys are read from `Authorization`. |
| `Connection: close` | `413`, `400 client_body`, and every HTTP/1 response while draining | The connection is closed after the response. |

### X-Cache

| value | meaning |
| --- | --- |
| `HIT` | served fresh from the cache |
| `MISS` | fetched from the backend |
| `REVALIDATED` | stale, and the backend confirmed it with a `304` |
| `STALE` | stale, and served because the backend failed |
| `UPDATING` | stale, served while a background request refreshes it |
| `BYPASS` | the route's `bypass_header` skipped the lookup |

See [HTTP caching](../concepts/http-caching.md#x-cache) and [Inside the cache](../concepts/inside-the-cache.md).
