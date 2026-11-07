# Glossary

Plain definitions of the terms used in these docs.

**Access log.** One line per finished request: who asked, what for, what happened, how long it took.

**Admin API.** A separate HTTP listener for operators, showing Prism's live state, metrics and controls. Never reachable through client traffic.

**Backend.** A server that does the real work, sitting behind the proxy. Also called an origin or upstream server elsewhere.

**Backoff.** Waiting before a retry, so a struggling server gets room to recover. Full jitter means waiting a random time, so many clients don't retry in sync.

**Balancer.** The strategy that picks which backend gets a request.

**Bounded queue.** A waiting line with a maximum size. Past that size, requests are refused right away instead of waiting forever.

**Canary.** Sending a small share of real traffic to a new version, to catch problems before everyone is affected.

**Circuit breaker.** A per-backend switch that stops sending requests after too many failures, then lets a few test requests through later to see if the backend has recovered. States: closed (normal), open (refusing), half-open (testing).

**Coalescing.** When many requests want the same uncached thing at once, sending one to the backend and letting the rest wait for its answer.

**Concurrency limit.** A cap on how many requests a route handles at the same time.

**Connection pool.** Open connections kept around for reuse, which avoids paying for a new handshake on every request.

**Consistent hashing.** Mapping keys to servers so that adding or removing a server moves only a small share of keys. Prism's `ip_hash` uses it.

**Drain.** Finishing the work in progress while refusing new work, usually before a shutdown.

**End-to-end header.** A header meant for the final recipient, so a proxy forwards it. Compare *hop-by-hop header*.

**ETag.** A version label for a response. A client or cache can ask "has this changed?" by sending it back in `If-None-Match`.

**Fresh.** A cached response that's still within its allowed lifetime, so it can be served without asking the backend. Compare *stale*.

**GCRA.** Generic Cell Rate Algorithm. A rate-limiting algorithm that behaves like a token bucket, but stores just one timestamp per client.

**Graceful shutdown.** Stopping without cutting off requests that are in progress.

**h2c.** HTTP/2 without TLS.

**Half-open.** See *circuit breaker*.

**Health check.** A periodic request from the proxy to a backend, asking "are you OK?". Also called an active check, since it runs even with no traffic.

**Hit, miss.** A cache hit is a response served from the cache. A miss means going to the backend.

**Hit-for-pass.** Remembering for a short time that a URL can't be cached, so later requests don't queue up behind each other for nothing.

**Hop-by-hop header.** A header that only describes one connection, so a proxy must not forward it. `Connection` is the classic example.

**Hot path.** The code that runs on every request, where small costs add up.

**Hot reload.** Applying a new config without restarting or dropping connections.

**Idempotent.** Doing something twice has the same effect as doing it once, which makes it safe to retry.

**Inflight.** Requests that have started and haven't finished.

**Keep-alive.** Reusing a TCP connection for more than one request.

**Latency, p50, p99.** Latency is how long a request takes. p50 is the median, and p99 is the time that 99% of requests beat. High percentiles are called the tail.

**Least connections.** A balancer that prefers the backend with the fewest requests in flight.

**Listener.** An address and port Prism accepts connections on.

**LRU.** Least recently used. When the cache is full, it evicts whatever hasn't been touched for the longest.

**Outlier ejection.** Temporarily removing a backend that keeps failing while the others are fine.

**Peer.** The machine on the other end of a TCP connection. Unlike headers, it can't be forged.

**Pool.** A group of backends in memory, along with the balancer that picks among them.

**Power of two choices.** Pick two backends at random, then use the less loaded one. Nearly as good as checking every backend, at a fraction of the cost.

**Prometheus.** A widely used monitoring system that collects metrics by reading a `/metrics` page.

**Rate limit.** A cap on how many requests a client can make over time.

**Retry budget.** A cap on retries as a share of normal traffic, so retries can't multiply load during an outage.

**Reverse proxy.** A server that accepts requests on behalf of backends and forwards them.

**Revalidation.** Asking the backend whether a stale cached response is still good. A `304 Not Modified` means yes, and costs no body.

**Round robin.** Backends take turns.

**Route.** A rule that matches a request by host and path, and says how to handle it.

**Runtime.** In Prism, the complete set of live objects built from one config.

**Shadow traffic.** Sending a copy of real requests to a new version and throwing away its answers, to test it under real load at no risk to users.

**SNI.** Server Name Indication. The site name a client sends during the TLS handshake, so the server can pick the right certificate.

**Span, trace.** A trace follows one request across many services. A span is one step in it, such as Prism's handling of the request.

**Stale.** A cached response past its lifetime. It may still be served in some cases: see `stale-if-error` and `stale-while-revalidate` in the config example.

**Sticky.** Sending the same client to the same place every time.

**Streaming.** Passing data along in chunks as it arrives, rather than collecting all of it first.

**TLS termination.** Ending the encrypted connection at the proxy.

**Token bucket.** A rate limit modeled as a bucket that refills steadily. Each request takes a token, and an empty bucket means refusal. The bucket's size is the allowed burst.

**Trusted proxy.** A peer whose `X-Forwarded-*` headers Prism believes.

**Upstream.** A named group of backends that do the same job.

**Vary.** A response header listing which request headers change the response, so a cache keeps a separate copy for each variant.
