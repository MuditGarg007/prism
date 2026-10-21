# Benchmark results

```
date: 2026-10-21T16:08:11Z
commit: 2a87a3a-dirty
cpu: 13th Gen Intel(R) Core(TM) i7-13620H
kernel: 7.2.5-3-omarchy
go: go1.27.0-X:nodwarf5
nginx: nginx:1.28.0-alpine
governor: performance
cpus: load 0-3, proxy 4-7 (4 threads), backends 8-11
runs: 3 x 30s at 5000 20000 req/s, 3 x 15s closed loop with 64 workers
body: 1024 bytes
```

## Latency at a fixed rate

Added latency is the scenario's minus the direct backend's at the same rate. CPU is the proxy's own, per request; allocations are Prism's heap allocations per request, net/http's included.

| scenario | req/s | p50 | p90 | p99 | max | added p50 | added p99 | success | CPU/req | allocs/req |
|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|
| direct | 5000 | 78 µs | 109 µs | 220 µs | 1050 µs |  |  | 100% |  |  |
| direct | 20000 | 86 µs | 180 µs | 632 µs | 2469 µs |  |  | 100% |  |  |
| nginx | 5000 | 119 µs | 167 µs | 270 µs | 3129 µs | +41 µs | +51 µs | 100% | 22.4 µs |  |
| nginx | 20000 | 125 µs | 254 µs | 769 µs | 5206 µs | +40 µs | +136 µs | 100% | 20.6 µs |  |
| prism-bare | 5000 | 162 µs | 219 µs | 475 µs | 28.1 ms | +84 µs | +256 µs | 100% | 62.3 µs | 82 |
| prism-bare | 20000 | 61 µs | 148 µs | 700 µs | 13.2 ms | −24 µs | +68 µs | 100% | 42.1 µs | 82 |
| prism-full | 5000 | 162 µs | 217 µs | 460 µs | 24.6 ms | +84 µs | +240 µs | 100% | 63.9 µs | 86 |
| prism-full | 20000 | 65 µs | 174 µs | 834 µs | 25.7 ms | −21 µs | +202 µs | 100% | 44.0 µs | 86 |
| nginx-cache | 5000 | 73 µs | 102 µs | 215 µs | 1473 µs | −5 µs | −5 µs | 100% | 28.6 µs |  |
| nginx-cache | 20000 | 89 µs | 167 µs | 612 µs | 3254 µs | +4 µs | −20 µs | 100% | 19.3 µs |  |
| prism-cache | 5000 | 90 µs | 131 µs | 315 µs | 24.0 ms | +12 µs | +96 µs | 100% | 39.7 µs | 30 |
| prism-cache | 20000 | 100 µs | 197 µs | 939 µs | 24.3 ms | +14 µs | +307 µs | 100% | 24.2 µs | 31 |

## Closed loop

A fixed number of workers each send the next request as soon as the last one is answered, so requests per second is the most each setup sustained.

| scenario | req/s | p50 | p99 | success | CPU/req | allocs/req |
|---|--:|--:|--:|--:|--:|--:|
| direct | 114526 | 131 µs | 1218 µs | 100% |  |  |
| nginx | 98666 | 200 µs | 1480 µs | 100% | 18.8 µs |  |
| prism-bare | 68822 | 679 µs | 2814 µs | 100% | 45.1 µs | 82 |
| prism-full | 66145 | 723 µs | 2939 µs | 100% | 48.3 µs | 86 |
| nginx-cache | 100830 | 131 µs | 1130 µs | 100% | 17.5 µs |  |
| prism-cache | 99234 | 140 µs | 1734 µs | 100% | 22.1 µs | 31 |
