#!/usr/bin/env bash
# Makes every backend answer 503 for 20 seconds, with each request allowed 2
# retries, to show that the retry budget keeps retries from multiplying the
# load on backends that are already failing.
#
# Breakers and health checks are off here, so only the budget stands
# between the clients and the backends. Without it, each request would
# reach the backends 3 times. With budget_ratio 0.2 and budget_min_per_sec
# 10, retries over any 10 s are capped at 0.2 x requests + 100, an
# amplification of at most about 1.23 at 300 req/s.
#
# The budget counts the last 10 s, so the retries are not spread evenly.
# The first second of the outage spends what the healthy period before it
# earned, and after that a retry is allowed again only as an old one leaves
# the window, so the retries come back as one burst every 10 s. That is how
# Finagle's budget behaves too: the 10 s average is bounded, a single
# second is not.
#
#   scripts/chaos/outage.sh                 # BUDGET_RATIO=0.2 by default
#   BUDGET_RATIO=1 scripts/chaos/outage.sh  # a looser budget to compare
SCENARIO=outage
source "$(dirname "$0")/common.sh"
BUDGET_RATIO=${BUDGET_RATIO:-0.2}

all() {
	for i in 1 2 3; do fault "$i" "$1"; done
}

setup
for i in 1 2 3; do start_backend "$i"; done
start_prism <<YAML
listeners: [{ addr: "127.0.0.1:$PRISM_PORT" }]
ready_path: /readyz
upstreams:
  app:
    backends:
BACKENDS
routes:
  - match: { path_prefix: / }
    upstream: app
    retry:
      attempts: 2
      on: ["503"]
      budget_ratio: $BUDGET_RATIO
      budget_min_per_sec: 10
YAML
start_load
at 10 "all backends 503" all error_rate=1
at 30 "all backends recover" all error_rate=0
finish "All backends failing, budget_ratio $BUDGET_RATIO"
