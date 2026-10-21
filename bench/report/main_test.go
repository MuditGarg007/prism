package main

import (
	"bytes"
	"math"
	"strings"
	"testing"
	"time"
)

func TestParseName(t *testing.T) {
	for _, tt := range []struct {
		name string
		want run
		ok   bool
	}{
		{"prism-bare_5000_2", run{scenario: "prism-bare", rate: "5000", n: 2}, true},
		{"direct_max_1", run{scenario: "direct", rate: "max", n: 1}, true},
		{"direct_fast_1", run{}, false},
		{"direct_5000", run{}, false},
		{"direct_5000_x", run{}, false},
	} {
		got, err := parseName(tt.name)
		if (err == nil) != tt.ok || tt.ok && (got.scenario != tt.want.scenario || got.rate != tt.want.rate || got.n != tt.want.n) {
			t.Errorf("parseName(%q) = %+v, %v", tt.name, got, err)
		}
	}
}

func TestMedian(t *testing.T) {
	nan := math.NaN()
	for _, tt := range []struct {
		vs   []float64
		want float64
	}{
		{[]float64{3, 1, 2}, 2},
		{[]float64{4, 1, 3, 2}, 2.5},
		{[]float64{nan, 5, nan}, 5},
		{[]float64{nan}, nan},
	} {
		var runs []run
		for _, v := range tt.vs {
			runs = append(runs, run{cpu: v})
		}
		got := median(runs, func(r run) float64 { return r.cpu })
		if got != tt.want && !(math.IsNaN(got) && math.IsNaN(tt.want)) {
			t.Errorf("median(%v) = %v, want %v", tt.vs, got, tt.want)
		}
	}
}

func TestParseHDR(t *testing.T) {
	in := `Value(ms)  Percentile  TotalCount  1/(1-Percentile)
0.154019   0.000000    0           1.000000
0.400850   0.500000    10          2.000000
`
	pts, err := parseHDR(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 2 || pts[1].percentile != 0.5 || pts[1].latency != 400850*time.Nanosecond {
		t.Errorf("parseHDR = %+v", pts)
	}
	if _, err := parseHDR(strings.NewReader("header\nx y\n")); err == nil {
		t.Error("parseHDR accepted a bad line")
	}
}

func TestNiceStep(t *testing.T) {
	for _, tt := range []struct {
		top, want time.Duration
	}{
		{time.Millisecond, 200 * time.Microsecond},
		{730 * time.Microsecond, 200 * time.Microsecond},
		{4 * time.Millisecond, time.Millisecond},
		{0, time.Microsecond},
	} {
		if got := niceStep(tt.top, 5); got != tt.want {
			t.Errorf("niceStep(%v) = %v, want %v", tt.top, got, tt.want)
		}
	}
}

func TestAxisLabel(t *testing.T) {
	for _, tt := range []struct {
		v, ymax, step time.Duration
		want          string
	}{
		{5 * time.Millisecond, 25 * time.Millisecond, 5 * time.Millisecond, "5 ms"},
		{0, 25 * time.Millisecond, 5 * time.Millisecond, "0 ms"},
		{12500 * time.Microsecond, 25 * time.Millisecond, 2500 * time.Microsecond, "12.5 ms"},
		{400 * time.Microsecond, time.Millisecond, 200 * time.Microsecond, "400 µs"},
		{4 * time.Millisecond, 5 * time.Millisecond, time.Millisecond, "4000 µs"},
	} {
		if got := axisLabel(tt.v, tt.ymax, tt.step); got != tt.want {
			t.Errorf("axisLabel(%v, %v, %v) = %q, want %q", tt.v, tt.ymax, tt.step, got, tt.want)
		}
	}
}

// TestReport checks the added latency against the direct backend and that
// a closed-loop row lands in its own table.
func TestReport(t *testing.T) {
	mk := func(scenario, rate string, p50, p99 time.Duration, cpu, mallocs float64) run {
		r := run{scenario: scenario, rate: rate, n: 1, cpu: cpu, mallocs: mallocs}
		r.report.Latencies.P50 = int64(p50)
		r.report.Latencies.P99 = int64(p99)
		r.report.Requests = 1000
		r.report.Success = 1
		r.report.Throughput = 1000
		r.hdr = []hdrPoint{{p50, 0.5}, {p99, 0.99}}
		return r
	}
	nan := math.NaN()
	rows := summarize([]run{
		mk("prism-bare", "5000", 150*time.Microsecond, 400*time.Microsecond, 30000, 40000),
		mk("direct", "5000", 100*time.Microsecond, 300*time.Microsecond, nan, nan),
		mk("prism-bare", "max", 1*time.Millisecond, 3*time.Millisecond, 20000, 40000),
	})
	if len(rows) != 3 || rows[0].scenario != "direct" || rows[2].rate != "max" {
		t.Fatalf("rows in the wrong order: %+v", rows)
	}
	var md bytes.Buffer
	if err := writeMarkdown(&md, "", rows); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"| prism-bare | 5000 | 150 µs | 0 µs | 400 µs | 0 µs | +50 µs | +100 µs | 100% | 30.0 µs | 40 |",
		"| direct | 5000 | 100 µs | 0 µs | 300 µs | 0 µs |  |  | 100% |  |  |",
		"| prism-bare | 1000 | 1000 µs | 3000 µs | 100% | 20.0 µs | 40 |",
	} {
		if !strings.Contains(md.String(), want) {
			t.Errorf("results.md lacks %q:\n%s", want, md.String())
		}
	}
	var svg bytes.Buffer
	if err := writeSVG(&svg, rows); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(svg.String(), "<polyline"); n != 2 {
		t.Errorf("latency.svg has %d lines, want 2", n)
	}
}
