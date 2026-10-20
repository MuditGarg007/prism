#!/usr/bin/env bash
# Kills one of three backends with SIGKILL under steady load, then starts
# it again.
#
# Requests in flight on the dead backend at the kill have their
# connections reset. New requests that pick it fail to connect and are
# retried on another backend, so clients see few errors, if any. Within
# about half a second the dead backend's breaker opens (its 1 s window fills
# with connect errors), and health checks mark it down a second or so
# later. After the restart, health checks mark it up again, and once
# open_for has passed, the half-open probes close the breaker.
#
# Outlier detection is left out: after 5 consecutive failures, which take
# a few tens of milliseconds at this rate, it would eject the backend
# before the breaker could see enough to open.
#
#   scripts/chaos/kill-backend.sh          # RATE=300 DURATION=40 by default
SCENARIO=kill-backend
source "$(dirname "$0")/common.sh"

setup
for i in 1 2 3; do start_backend "$i"; done
start_prism <<YAML
listeners: [{ addr: "127.0.0.1:$PRISM_PORT" }]
ready_path: /readyz
upstreams:
  app:
    backends:
BACKENDS
    health: { path: /healthz, interval: 1s, timeout: 500ms, unhealthy_after: 2, healthy_after: 2 }
    circuit_breaker: { failure_ratio: 0.5, min_requests: 10, open_for: 5s, window: 1s, half_open_probes: 3 }
    timeouts: { connect: 500ms, response_header: 2s }
routes:
  - match: { path_prefix: / }
    upstream: app
    retry: { attempts: 2, on: [connect_error, "502", "503"] }
YAML
start_load
at 10 "kill b3" kill_backend 3
at 25 "restart b3" start_backend 3
finish "Killing one of three backends"
