#!/usr/bin/env bash
# Reloads Prism with SIGHUP every INTERVAL seconds under steady load, the
# M5 exit criterion run against the real binary. The config cycles through
# a weight change, a third backend added and removed again, and a changed
# response header. Each version is written to a temporary file and renamed
# into place, so a reload never reads a half-written config.
#
# It fails unless every request succeeded and every reload was applied.
# The graph should show a flat error line at zero through the reloads.
#
#   scripts/reload-under-load.sh        # RATE=300 DURATION=20 INTERVAL=0.25 by default
SCENARIO=reload-under-load
DURATION=${DURATION:-20}
source "$(dirname "$0")/chaos/common.sh"
INTERVAL=${INTERVAL:-0.25}
ADMIN_PORT=${ADMIN_PORT:-19099}

# config N prints version N of the config.
config() {
	local w1=1 b3="" gen=a
	case $(($1 % 4)) in
	1) w1=3 ;;
	2) b3="      - { url: \"$(backend_url 3)\" }" ;;
	3) gen=b ;;
	esac
	cat <<YAML
listeners: [{ addr: "127.0.0.1:$PRISM_PORT" }]
ready_path: /readyz
admin: { listen: "127.0.0.1:$ADMIN_PORT" }
upstreams:
  app:
    balancer: least_conn
    backends:
      - { url: "$(backend_url 1)", weight: $w1 }
      - { url: "$(backend_url 2)" }
$b3
    health: { path: /healthz, interval: 1s, timeout: 500ms, unhealthy_after: 2, healthy_after: 2 }
    circuit_breaker: { failure_ratio: 0.5, min_requests: 10, open_for: 5s, window: 1s }
    max_idle_conns_per_host: 64
routes:
  - match: { path_prefix: / }
    upstream: app
    headers: { response_set: { X-Config: "$gen" } }
YAML
}

setup
for i in 1 2 3; do start_backend "$i"; done
start_prism < <(config 0) # not a pipe, which would run it in a subshell
start_load

at 2 "reloads start" true
reloads=$(awk -v d="$DURATION" -v i="$INTERVAL" 'BEGIN { print int((d - 4) / i) }')
for n in $(seq "$reloads"); do
	config "$n" >"$OUT/prism.yaml.tmp"
	mv "$OUT/prism.yaml.tmp" "$OUT/prism.yaml"
	kill -HUP "${PIDS[prism]}"
	sleep "$INTERVAL"
done
printf '%s\tinject\t%s\n' "$((DURATION - 2))" "$reloads reloads sent" >>"$OUT/events.tsv"
# The last reload must be done before it is counted.
sleep 1
curl -fsS "http://127.0.0.1:$ADMIN_PORT/metrics" | grep '^prism_config_reloads_total' >"$OUT/reloads.txt"

finish "Reloading every ${INTERVAL}s"
echo
cat "$OUT/reloads.txt"

errors=$(sed -n 's/.*errors \([0-9]*\).*/\1/p' "$OUT/summary.txt")
applied=$(grep -c '"msg":"config reloaded"' "$OUT/prism.log" || true)
failed=$(grep -c '"msg":"reload failed' "$OUT/prism.log" || true)
echo "signals sent $reloads, reloads applied $applied, failed $failed, request errors $errors"
if [[ $errors != 0 || $failed != 0 || $applied != "$reloads" ]]; then
	echo "FAIL" >&2
	exit 1
fi
echo "PASS"
