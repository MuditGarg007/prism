#!/usr/bin/env bash
# Builds Prism and the test tools, then runs three test backends for the
# experiments in docs/concepts/: a on 9101, b on 9102 and c on 9103.
# Ctrl-C stops all three.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

go build -o bin/prism ./cmd/prism
go build -o bin/faultserver ./test/faultserver
go build -o bin/loadgen ./test/loadgen

trap 'kill $(jobs -p) 2>/dev/null' EXIT
./bin/faultserver -addr 127.0.0.1:9101 -name a &
./bin/faultserver -addr 127.0.0.1:9102 -name b &
./bin/faultserver -addr 127.0.0.1:9103 -name c &
wait
