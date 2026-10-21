// Command report turns the runs bench/run.sh recorded into a Markdown
// table (results.md) and a latency percentile graph (latency.svg).
//
// Each scenario's numbers at a rate are the median over its repeats, taken
// metric by metric. Added latency is a scenario's median minus the direct
// backend's at the same rate. The graph draws, at the highest fixed rate,
// the run whose p99 is the median one.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// vegetaReport is the part of vegeta's JSON report used here. Latencies
// are in nanoseconds.
type vegetaReport struct {
	Latencies struct {
		P50  int64 `json:"50th"`
		P90  int64 `json:"90th"`
		P99  int64 `json:"99th"`
		Max  int64 `json:"max"`
		Mean int64 `json:"mean"`
	} `json:"latencies"`
	Requests   int64    `json:"requests"`
	Throughput float64  `json:"throughput"` // successful requests per second
	Success    float64  `json:"success"`    // share of 2xx and 3xx
	Errors     []string `json:"errors"`
}

// run is one attack: its report and what the proxy used during it.
type run struct {
	scenario string
	rate     string // requests per second, or "max" for closed loop
	n        int
	report   vegetaReport
	cpu      float64 // proxy CPU microseconds; NaN when not measured
	mallocs  float64 // Prism heap allocations; NaN when not measured
	hdr      []hdrPoint
}

// hdrPoint is one line of vegeta's hdrplot report.
type hdrPoint struct {
	latency    time.Duration
	percentile float64 // 0 to 1
}

// row is a scenario at a rate, as the table shows it.
type row struct {
	scenario, rate         string
	p50, p90, p99, max     time.Duration
	throughput, success    float64
	cpuPerReq, allocPerReq float64 // NaN when not measured
	runs                   []run
}

func main() {
	in := flag.String("in", "bin/bench", "directory bench/run.sh wrote")
	flag.Parse()
	runs, err := load(filepath.Join(*in, "runs"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "report:", err)
		os.Exit(1)
	}
	if len(runs) == 0 {
		fmt.Fprintln(os.Stderr, "report: no runs in", *in)
		os.Exit(1)
	}
	env, _ := os.ReadFile(filepath.Join(*in, "env.txt"))
	rows := summarize(runs)
	if err := writeFile(filepath.Join(*in, "results.md"), func(w io.Writer) error {
		return writeMarkdown(w, string(env), rows)
	}); err != nil {
		fmt.Fprintln(os.Stderr, "report:", err)
		os.Exit(1)
	}
	if err := writeFile(filepath.Join(*in, "latency.svg"), func(w io.Writer) error {
		return writeSVG(w, rows)
	}); err != nil {
		fmt.Fprintln(os.Stderr, "report:", err)
		os.Exit(1)
	}
}

func writeFile(path string, write func(io.Writer) error) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	if err := write(w); err != nil {
		f.Close()
		return err
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// load reads every run in dir, named <scenario>_<rate>_<n>.json with
// optional .hdr, .cpu and .mallocs files beside it.
func load(dir string) ([]run, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	var runs []run
	for _, p := range paths {
		base := strings.TrimSuffix(p, ".json")
		r, err := parseName(filepath.Base(base))
		if err != nil {
			return nil, err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(b, &r.report); err != nil {
			return nil, fmt.Errorf("%s: %v", p, err)
		}
		r.cpu = readNumber(base + ".cpu")
		r.mallocs = readNumber(base + ".mallocs")
		if f, err := os.Open(base + ".hdr"); err == nil {
			r.hdr, err = parseHDR(f)
			f.Close()
			if err != nil {
				return nil, fmt.Errorf("%s.hdr: %v", base, err)
			}
		}
		runs = append(runs, r)
	}
	return runs, nil
}

func parseName(name string) (run, error) {
	parts := strings.Split(name, "_")
	if len(parts) != 3 {
		return run{}, fmt.Errorf("run name %q is not <scenario>_<rate>_<n>", name)
	}
	n, err := strconv.Atoi(parts[2])
	if err != nil {
		return run{}, fmt.Errorf("run name %q: bad repeat number", name)
	}
	if parts[1] != "max" {
		if _, err := strconv.Atoi(parts[1]); err != nil {
			return run{}, fmt.Errorf("run name %q: bad rate", name)
		}
	}
	return run{scenario: parts[0], rate: parts[1], n: n}, nil
}

// readNumber returns the number in path, or NaN if there is none.
func readNumber(path string) float64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return math.NaN()
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(string(b)), 64)
	if err != nil {
		return math.NaN()
	}
	return v
}

// parseHDR reads vegeta's hdrplot report: a header line, then latency in
// milliseconds, percentile, count and 1/(1-percentile) per line.
func parseHDR(r io.Reader) ([]hdrPoint, error) {
	var pts []hdrPoint
	sc := bufio.NewScanner(r)
	first := true
	for sc.Scan() {
		if first {
			first = false
			continue
		}
		f := strings.Fields(sc.Text())
		if len(f) < 2 {
			continue
		}
		ms, err1 := strconv.ParseFloat(f[0], 64)
		pct, err2 := strconv.ParseFloat(f[1], 64)
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("bad line %q", sc.Text())
		}
		pts = append(pts, hdrPoint{time.Duration(ms * float64(time.Millisecond)), pct})
	}
	return pts, sc.Err()
}

// summarize groups runs by scenario and rate. Scenarios come in a fixed
// order (unknown ones after, by name), fixed rates ascending, "max" last.
func summarize(runs []run) []row {
	type key struct{ scenario, rate string }
	groups := map[key][]run{}
	var keys []key
	for _, r := range runs {
		k := key{r.scenario, r.rate}
		if _, ok := groups[k]; !ok {
			keys = append(keys, k)
		}
		groups[k] = append(groups[k], r)
	}
	slices.SortFunc(keys, func(a, b key) int {
		if c := scenarioOrder(a.scenario) - scenarioOrder(b.scenario); c != 0 {
			return c
		}
		if c := strings.Compare(a.scenario, b.scenario); c != 0 {
			return c
		}
		return rateOrder(a.rate) - rateOrder(b.rate)
	})
	rows := make([]row, 0, len(keys))
	for _, k := range keys {
		g := groups[k]
		lat := func(f func(vegetaReport) int64) time.Duration {
			return time.Duration(median(g, func(r run) float64 { return float64(f(r.report)) }))
		}
		rows = append(rows, row{
			scenario:    k.scenario,
			rate:        k.rate,
			p50:         lat(func(v vegetaReport) int64 { return v.Latencies.P50 }),
			p90:         lat(func(v vegetaReport) int64 { return v.Latencies.P90 }),
			p99:         lat(func(v vegetaReport) int64 { return v.Latencies.P99 }),
			max:         lat(func(v vegetaReport) int64 { return v.Latencies.Max }),
			throughput:  median(g, func(r run) float64 { return r.report.Throughput }),
			success:     median(g, func(r run) float64 { return r.report.Success }),
			cpuPerReq:   median(g, func(r run) float64 { return r.cpu / float64(r.report.Requests) }),
			allocPerReq: median(g, func(r run) float64 { return r.mallocs / float64(r.report.Requests) }),
			runs:        g,
		})
	}
	return rows
}

// scenarioOrder puts the known scenarios in the order the table reads best.
func scenarioOrder(s string) int {
	order := []string{"direct", "nginx", "prism-bare", "prism-full", "nginx-cache", "prism-cache"}
	if i := slices.Index(order, s); i >= 0 {
		return i
	}
	return len(order)
}

func rateOrder(rate string) int {
	if rate == "max" {
		return math.MaxInt32
	}
	n, _ := strconv.Atoi(rate)
	return n
}

// median returns the median of f over runs, ignoring NaN, or NaN if every
// value is NaN.
func median(runs []run, f func(run) float64) float64 {
	var vs []float64
	for _, r := range runs {
		if v := f(r); !math.IsNaN(v) && !math.IsInf(v, 0) {
			vs = append(vs, v)
		}
	}
	if len(vs) == 0 {
		return math.NaN()
	}
	slices.Sort(vs)
	if len(vs)%2 == 1 {
		return vs[len(vs)/2]
	}
	return (vs[len(vs)/2-1] + vs[len(vs)/2]) / 2
}

func writeMarkdown(w io.Writer, env string, rows []row) error {
	fmt.Fprintf(w, "# Benchmark results\n\n")
	if env != "" {
		fmt.Fprintf(w, "```\n%s```\n\n", env)
	}
	direct := map[string]row{}
	for _, r := range rows {
		if r.scenario == "direct" {
			direct[r.rate] = r
		}
	}

	fmt.Fprintf(w, "## Latency at a fixed rate\n\n")
	fmt.Fprintf(w, "Added latency is the scenario's minus the direct backend's at the same rate. ")
	fmt.Fprintf(w, "CPU is the proxy's own, per request; allocations are Prism's heap allocations per request, net/http's included.\n\n")
	fmt.Fprintf(w, "| scenario | req/s | p50 | p90 | p99 | max | added p50 | added p99 | success | CPU/req | allocs/req |\n")
	fmt.Fprintf(w, "|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|\n")
	for _, r := range rows {
		if r.rate == "max" {
			continue
		}
		addP50, addP99 := "", ""
		if d, ok := direct[r.rate]; ok && r.scenario != "direct" {
			addP50, addP99 = signed(r.p50-d.p50), signed(r.p99-d.p99)
		}
		fmt.Fprintf(w, "| %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s |\n",
			r.scenario, r.rate, dur(r.p50), dur(r.p90), dur(r.p99), dur(r.max),
			addP50, addP99, pct(r.success), micros(r.cpuPerReq), count(r.allocPerReq))
	}

	fmt.Fprintf(w, "\n## Closed loop\n\n")
	fmt.Fprintf(w, "A fixed number of workers each send the next request as soon as the last one is answered, so requests per second is the most each setup sustained.\n\n")
	fmt.Fprintf(w, "| scenario | req/s | p50 | p99 | success | CPU/req | allocs/req |\n")
	fmt.Fprintf(w, "|---|--:|--:|--:|--:|--:|--:|\n")
	for _, r := range rows {
		if r.rate != "max" {
			continue
		}
		fmt.Fprintf(w, "| %s | %.0f | %s | %s | %s | %s | %s |\n",
			r.scenario, r.throughput, dur(r.p50), dur(r.p99), pct(r.success), micros(r.cpuPerReq), count(r.allocPerReq))
	}

	var errs []string
	for _, r := range rows {
		for _, rn := range r.runs {
			for _, e := range rn.report.Errors {
				errs = append(errs, fmt.Sprintf("%s_%s_%d: %s", rn.scenario, rn.rate, rn.n, e))
			}
		}
	}
	if len(errs) > 0 {
		fmt.Fprintf(w, "\n## Errors\n\n")
		for _, e := range errs {
			fmt.Fprintf(w, "- `%s`\n", e)
		}
	}
	return nil
}

// dur formats a latency in microseconds below 10 ms and milliseconds above.
func dur(d time.Duration) string {
	if d < 10*time.Millisecond {
		return fmt.Sprintf("%.0f µs", float64(d)/float64(time.Microsecond))
	}
	return fmt.Sprintf("%.1f ms", float64(d)/float64(time.Millisecond))
}

func signed(d time.Duration) string {
	if d >= 0 {
		return "+" + dur(d)
	}
	return "−" + dur(-d)
}

func pct(v float64) string {
	if v == 1 {
		return "100%"
	}
	return fmt.Sprintf("%.3f%%", v*100)
}

func micros(v float64) string {
	if math.IsNaN(v) {
		return ""
	}
	return fmt.Sprintf("%.1f µs", v)
}

func count(v float64) string {
	if math.IsNaN(v) {
		return ""
	}
	return fmt.Sprintf("%.0f", v)
}
