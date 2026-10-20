# Shared helpers for the chaos scenarios. Source it; don't run it.
#
# A scenario builds Prism, the faultserver and the loadgen, starts three
# backends and Prism on loopback ports, runs load while it injects faults at
# fixed times, and leaves everything in $OUT:
#   load.csv     one row per second from the loadgen
#   events.tsv   the injected faults and Prism's reactions, in seconds
#   prism.log    Prism's JSON log
#   graph.svg    the load, errors, amplification and latency over time

set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
SCENARIO=${SCENARIO:?set SCENARIO before sourcing common.sh}
OUT=${OUT:-$ROOT/bin/chaos/$SCENARIO}
RATE=${RATE:-300}
BACKEND_LATENCY=${BACKEND_LATENCY:-20ms} # so some requests are in flight
DURATION=${DURATION:-40}
PRISM_PORT=${PRISM_PORT:-19090}
BACKEND_PORTS=(19101 19102 19103)
declare -A PIDS=()

setup() {
	rm -rf "$OUT"
	mkdir -p "$OUT"
	echo "building into $OUT"
	(cd "$ROOT" && go build -o "$OUT/prism" ./cmd/prism &&
		go build -o "$OUT/faultserver" ./test/faultserver &&
		go build -o "$OUT/loadgen" ./test/loadgen &&
		go build -o "$OUT/chaosplot" ./test/chaosplot) 2> >(grep -v 'ignoring go.mod' >&2)
	: >"$OUT/events.tsv"
	trap cleanup EXIT
}

cleanup() {
	for pid in "${PIDS[@]}"; do
		kill "$pid" 2>/dev/null || true
	done
	wait 2>/dev/null || true
}

backend_url() { echo "http://127.0.0.1:${BACKEND_PORTS[$1 - 1]}"; }

# start_backend N starts backend N (1-based) and waits until it answers.
start_backend() {
	local n=$1
	"$OUT/faultserver" -addr "127.0.0.1:${BACKEND_PORTS[$n - 1]}" -name "b$n" \
		-latency "${BACKEND_LATENCY:-0s}" 2>>"$OUT/backends.log" &
	PIDS[b$n]=$!
	wait_http "$(backend_url "$n")/_stats"
}

# kill_backend N kills backend N with SIGKILL, so its connections are reset
# rather than closed cleanly.
kill_backend() {
	kill -9 "${PIDS[b$1]}"
	wait "${PIDS[b$1]}" 2>/dev/null || true
	unset "PIDS[b$1]"
}

# fault N QUERY changes the faults of backend N, for example
# "fault 2 error_rate=1".
fault() {
	curl -fsS "$(backend_url "$1")/_fault?$2" >/dev/null
}

wait_http() {
	for _ in $(seq 100); do
		curl -fsS -o /dev/null "$1" 2>/dev/null && return 0
		sleep 0.05
	done
	echo "timed out waiting for $1" >&2
	return 1
}

# start_prism writes $OUT/prism.yaml from stdin (with BACKENDS replaced by
# the backend list) and starts Prism with it.
start_prism() {
	local list=""
	for i in 1 2 3; do
		list+="      - { url: \"$(backend_url "$i")\" }"$'\n'
	done
	awk -v list="$list" '{ if ($0 ~ /^ *BACKENDS$/) printf "%s", list; else print }' >"$OUT/prism.yaml"
	"$OUT/prism" -config "$OUT/prism.yaml" -validate >/dev/null
	"$OUT/prism" -config "$OUT/prism.yaml" -log-format json 2>"$OUT/prism.log" &
	PIDS[prism]=$!
	wait_http "http://127.0.0.1:$PRISM_PORT/readyz"
}

# start_load runs the loadgen in the background for DURATION seconds and
# records when it started, which is second 0 of every event.
start_load() {
	local stats=""
	for i in 1 2 3; do
		stats+="${stats:+,}$(backend_url "$i")/_stats"
	done
	START=$(date +%s.%N)
	"$OUT/loadgen" -url "http://127.0.0.1:$PRISM_PORT/" -rate "$RATE" -duration "${DURATION}s" \
		-stats "$stats" -out "$OUT/load.csv" 2>"$OUT/summary.txt" &
	LOAD_PID=$!
}

# at SECOND LABEL CMD... waits until SECOND after the load started, runs CMD
# and records LABEL as an injected event.
at() {
	local t=$1 label=$2
	shift 2
	local wait
	wait=$(awk -v s="$START" -v t="$t" -v now="$(date +%s.%N)" 'BEGIN { w = s + t - now; print (w > 0 ? w : 0) }')
	sleep "$wait"
	"$@"
	printf '%s\tinject\t%s\n' "$t" "$label" >>"$OUT/events.tsv"
	echo "t=${t}s: $label"
}

# finish waits for the load to end, stops Prism gracefully, and writes the
# events, the summary and the graph.
finish() {
	local title=$1
	wait "$LOAD_PID"
	kill -TERM "${PIDS[prism]}"
	wait "${PIDS[prism]}" || true
	unset "PIDS[prism]"
	prism_events
	sort -s -n -k1,1 -o "$OUT/events.tsv" "$OUT/events.tsv"
	echo
	echo "== $title"
	cat "$OUT/summary.txt"
	echo
	echo "events (seconds from the start of the load):"
	column -t -s $'\t' "$OUT/events.tsv"
	plot "$title"
	echo
	echo "results in $OUT"
}

# prism_events adds Prism's breaker, ejection and health log lines to
# events.tsv, timed from the start of the load.
prism_events() {
	if ! command -v jq >/dev/null; then
		echo "jq not found; Prism's events are only in prism.log" >&2
		return
	fi
	jq -r 'select(.msg | test("circuit breaker|ejected|marked")) |
		[.time, .backend, .msg + (if .to then " -> " + .to else "" end)] | @tsv' "$OUT/prism.log" |
		while IFS=$'\t' read -r time backend msg; do
			local s
			s=$(date -d "$time" +%s.%N)
			printf '%.1f\tprism\t%s %s\n' "$(awk -v a="$s" -v b="$START" 'BEGIN { print a - b }')" \
				"${backend#http://127.0.0.1:}" "$msg" >>"$OUT/events.tsv"
		done
}

plot() {
	"$OUT/chaosplot" -csv "$OUT/load.csv" -events "$OUT/events.tsv" \
		-title "$1 ($RATE req/s)" -out "$OUT/graph.svg"
	echo "graph: $OUT/graph.svg"
}
