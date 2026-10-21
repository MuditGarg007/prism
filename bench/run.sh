#!/usr/bin/env bash
# Compares a backend reached directly, through nginx and through Prism, on
# this machine, with vegeta as the load generator. Each scenario gets fresh
# processes, a warm-up, then REPEATS runs at each fixed rate in RATES (open
# loop, so a slow proxy cannot slow the sender down and hide its latency)
# and REPEATS closed-loop runs with MAX_WORKERS workers for throughput.
#
# The load, the proxy and the backends run on separate cores (LOAD_CPUS,
# PROXY_CPUS, BACKEND_CPUS), and the proxy gets PROXY_THREADS of them:
# GOMAXPROCS for Prism, worker_processes for nginx. Both write an access
# log line per request.
#
# Results go to $OUT/runs, one set of files per run:
#   <scenario>_<rate>_<n>.json   vegeta's report (rate "max" for closed loop)
#   <scenario>_<rate>_<n>.hdr    vegeta's latency percentile distribution
#   <scenario>_<rate>_<n>.cpu    CPU microseconds the proxy used
#   <scenario>_<rate>_<n>.mallocs  heap allocations Prism made
# and bench/report turns them into $OUT/results.md and $OUT/latency.svg.
#
#   bench/run.sh                         # about 25 minutes
#   QUICK=1 bench/run.sh                 # one short run of each, to try it
#   SCENARIOS="direct prism-bare" bench/run.sh
#
# nginx runs in Docker (NGINX_IMAGE) with host networking.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
OUT=${OUT:-$ROOT/bin/bench}
SCENARIOS=${SCENARIOS:-"direct nginx prism-bare prism-full nginx-cache prism-cache"}
RATES=${RATES:-"5000 20000"}
DURATION=${DURATION:-30s}
WARMUP=${WARMUP:-5s}
REPEATS=${REPEATS:-3}
MAX_DURATION=${MAX_DURATION:-15s}
MAX_WORKERS=${MAX_WORKERS:-64}
BODY_SIZE=${BODY_SIZE:-1024}
LOAD_CPUS=${LOAD_CPUS:-0-3}
PROXY_CPUS=${PROXY_CPUS:-4-7}
BACKEND_CPUS=${BACKEND_CPUS:-8-11}
PROXY_THREADS=${PROXY_THREADS:-4}
NGINX_IMAGE=${NGINX_IMAGE:-nginx:1.28.0-alpine}
if [[ ${QUICK:-} == 1 ]]; then
	RATES=${QUICK_RATES:-2000} DURATION=3s WARMUP=1s REPEATS=1 MAX_DURATION=3s
fi

NGINX_NAME=prism-bench-nginx
ADMIN=http://127.0.0.1:9901
declare -A PIDS=()
PROXY_PID="" # Prism's pid, for its CPU time
NGINX_CGROUP=""

cleanup() {
	for pid in "${PIDS[@]}"; do
		kill "$pid" 2>/dev/null || true
	done
	PIDS=()
	docker rm -f "$NGINX_NAME" >/dev/null 2>&1 || true
	wait 2>/dev/null || true
}
trap cleanup EXIT

wait_http() {
	for _ in $(seq 200); do
		curl -fsS -o /dev/null "$1" 2>/dev/null && return 0
		sleep 0.05
	done
	echo "timed out waiting for $1" >&2
	return 1
}

# start_backends CACHE_CONTROL starts the two faultservers.
start_backends() {
	for port in 9101 9102; do
		taskset -c "$BACKEND_CPUS" env GOMAXPROCS=2 "$OUT/faultserver" -addr "127.0.0.1:$port" \
			-body-size "$BODY_SIZE" -cache-control "$1" 2>>"$OUT/logs/backends.log" &
		PIDS[b$port]=$!
		wait_http "http://127.0.0.1:$port/_stats"
	done
}

start_prism() {
	taskset -c "$PROXY_CPUS" env GOMAXPROCS="$PROXY_THREADS" "$OUT/prism" \
		-config "$ROOT/bench/$1.yaml" -log-format json 2>>"$OUT/logs/prism.log" &
	PIDS[prism]=$!
	PROXY_PID=$!
	wait_http "$ADMIN/routes"
}

start_nginx() {
	# Created here, so the log belongs to us and can be truncated.
	touch "$OUT/logs/nginx-access.log"
	chmod 666 "$OUT/logs/nginx-access.log"
	local id
	id=$(docker run -d --rm --name "$NGINX_NAME" --network host --cpuset-cpus "$PROXY_CPUS" \
		-v "$ROOT/bench/nginx.conf:/etc/nginx/nginx.conf:ro" -v "$OUT/logs:/logs" "$NGINX_IMAGE")
	NGINX_CGROUP=/sys/fs/cgroup/system.slice/docker-$id.scope/cpu.stat
	[[ -r $NGINX_CGROUP ]] || NGINX_CGROUP=""
	wait_http "http://127.0.0.1:8090/"
}

# cpu_usec prints the CPU microseconds the proxy has used so far, or
# nothing when there is no proxy or it can't be read.
cpu_usec() {
	if [[ -n $PROXY_PID ]]; then
		# utime and stime, in clock ticks.
		awk -v hz="$(getconf CLK_TCK)" '{ printf "%d\n", ($14 + $15) * 1000000 / hz }' "/proc/$PROXY_PID/stat"
	elif [[ -n $NGINX_CGROUP ]]; then
		awk '$1 == "usage_usec" { print $2 }' "$NGINX_CGROUP"
	fi
}

mallocs() {
	[[ -n $PROXY_PID ]] || return 0
	curl -fsS "$ADMIN/metrics" | awk '$1 == "go_memstats_mallocs_total" { printf "%d\n", $2 }'
}

# attack NAME URL RATE runs one vegeta attack and records it as NAME. A
# RATE of "max" runs closed loop with MAX_WORKERS workers.
attack() {
	local name=$1 url=$2 rate=$3 args
	if [[ $rate == max ]]; then
		args=(-rate=0 -workers="$MAX_WORKERS" -max-workers="$MAX_WORKERS" -duration="$MAX_DURATION")
	else
		args=(-rate="$rate" -duration="$DURATION")
	fi
	local cpu0 m0 cpu1 m1
	cpu0=$(cpu_usec) m0=$(mallocs)
	echo "GET $url" | taskset -c "$LOAD_CPUS" "$OUT/vegeta" -cpus=4 attack -timeout=5s "${args[@]}" >"$OUT/runs/$name.bin"
	cpu1=$(cpu_usec) m1=$(mallocs)
	"$OUT/vegeta" report -type=json "$OUT/runs/$name.bin" >"$OUT/runs/$name.json"
	"$OUT/vegeta" report -type=hdrplot "$OUT/runs/$name.bin" >"$OUT/runs/$name.hdr"
	rm "$OUT/runs/$name.bin"
	if [[ -n $cpu0 ]]; then echo $((cpu1 - cpu0)) >"$OUT/runs/$name.cpu"; fi
	if [[ -n $m0 ]]; then echo $((m1 - m0)) >"$OUT/runs/$name.mallocs"; fi
	# Keep the access logs from filling the disk.
	: >"$OUT/logs/prism.log"
	[[ ! -e $OUT/logs/nginx-access.log ]] || : >"$OUT/logs/nginx-access.log"
}

run_scenario() {
	local s=$1 url cache=""
	[[ $s == *-cache ]] && cache=max-age=3600
	start_backends "$cache"
	PROXY_PID="" NGINX_CGROUP=""
	case $s in
	direct) url=http://127.0.0.1:9101/bench ;;
	nginx) start_nginx && url=http://127.0.0.1:8090/bench ;;
	nginx-cache) start_nginx && url=http://127.0.0.1:8091/bench ;;
	prism-*) start_prism "$s" && url=http://127.0.0.1:8090/bench ;;
	*)
		echo "unknown scenario $s" >&2
		return 1
		;;
	esac
	# The first request fills the cache; the warm-up opens connections.
	curl -fsS -o /dev/null "$url"
	local first=${RATES%% *}
	echo "GET $url" | taskset -c "$LOAD_CPUS" "$OUT/vegeta" -cpus=4 attack -rate="$first" -duration="$WARMUP" >/dev/null
	for rate in $RATES max; do
		for n in $(seq "$REPEATS"); do
			echo "  $s at $rate req/s, run $n"
			attack "${s}_${rate}_$n" "$url" "$rate"
		done
	done
	cleanup
}

rm -rf "$OUT"
mkdir -p "$OUT/runs" "$OUT/logs"
echo "building into $OUT"
(cd "$ROOT" && go build -o "$OUT/prism" ./cmd/prism &&
	go build -o "$OUT/faultserver" ./test/faultserver &&
	go build -o "$OUT/vegeta" github.com/tsenart/vegeta/v12 &&
	go build -o "$OUT/report" ./bench/report) 2> >(grep -v 'ignoring go.mod' >&2)
if [[ $SCENARIOS == *nginx* ]]; then
	docker image inspect "$NGINX_IMAGE" >/dev/null 2>&1 || docker pull -q "$NGINX_IMAGE"
fi

{
	echo "date: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
	echo "commit: $(git -C "$ROOT" describe --always --dirty)"
	echo "cpu: $(lscpu | sed -n 's/^Model name: *//p')"
	echo "kernel: $(uname -r)"
	echo "go: $(go version | cut -d' ' -f3)"
	echo "nginx: $NGINX_IMAGE"
	echo "governor: $(cat /sys/devices/system/cpu/cpu4/cpufreq/scaling_governor 2>/dev/null || echo unknown)"
	echo "cpus: load $LOAD_CPUS, proxy $PROXY_CPUS ($PROXY_THREADS threads), backends $BACKEND_CPUS"
	echo "runs: $REPEATS x $DURATION at $RATES req/s, $REPEATS x $MAX_DURATION closed loop with $MAX_WORKERS workers"
	echo "body: $BODY_SIZE bytes"
} >"$OUT/env.txt"
cat "$OUT/env.txt"

for s in $SCENARIOS; do
	echo "$s"
	run_scenario "$s"
done

"$OUT/report" -in "$OUT"
echo "results in $OUT/results.md and $OUT/latency.svg"
