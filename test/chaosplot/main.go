// Command chaosplot draws a chaos run as an SVG graph: the load.csv the
// loadgen wrote and the events.tsv the chaos scripts wrote, one panel per
// measure on a shared time axis. It needs nothing beyond Go, so every chaos
// run gets its graph.
//
//	chaosplot -csv load.csv -events events.tsv -title "Killing a backend" -out graph.svg
package main

import (
	"bufio"
	"encoding/csv"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
)

// series is one column of load.csv, drawn as a line.
type series struct {
	column string
	label  string
	color  string
}

// panel is one chart of the graph.
type panel struct {
	title  string
	lines  []series
	minTop float64 // the y axis reaches at least this high
	ref    float64 // a dotted reference line, when non-zero
}

var panels = []panel{
	{title: "Requests per second", lines: []series{
		{"sent", "sent by clients", "#8a8f98"},
		{"ok", "answered ok", "#2e9e5b"},
		{"backend_reqs", "reached backends", "#3b6fd4"},
	}},
	{title: "Client error rate (%)", minTop: 5, lines: []series{
		{"error_pct", "5xx or no response", "#d64545"},
	}},
	{title: "Load amplification (backend requests per client request)", minTop: 2, ref: 1, lines: []series{
		{"amplification", "amplification", "#8a4fd4"},
	}},
	{title: "Latency (ms)", minTop: 10, lines: []series{
		{"p50_ms", "p50", "#3b6fd4"},
		{"p99_ms", "p99", "#d68a1f"},
	}},
}

// event is a line of events.tsv: an injected fault or Prism's reaction.
type event struct {
	at     float64
	inject bool
	label  string
}

// Layout, in SVG user units.
const (
	width       = 1000
	marginLeft  = 70
	marginRight = 20
	panelHeight = 200
	panelGap    = 60
	top         = 60
	plotWidth   = width - marginLeft - marginRight
)

func main() {
	csvPath := flag.String("csv", "load.csv", "the loadgen's CSV")
	eventsPath := flag.String("events", "", "events.tsv from a chaos script (optional)")
	title := flag.String("title", "Chaos run", "graph title")
	out := flag.String("out", "graph.svg", "SVG file to write")
	flag.Parse()
	if err := run(*csvPath, *eventsPath, *title, *out); err != nil {
		fmt.Fprintln(os.Stderr, "chaosplot:", err)
		os.Exit(1)
	}
}

func run(csvPath, eventsPath, title, out string) error {
	f, err := os.Open(csvPath)
	if err != nil {
		return err
	}
	defer f.Close()
	data, err := readCSV(f)
	if err != nil {
		return fmt.Errorf("%s: %w", csvPath, err)
	}
	var events []event
	if eventsPath != "" {
		ef, err := os.Open(eventsPath)
		if err != nil {
			return err
		}
		defer ef.Close()
		if events, err = readEvents(ef); err != nil {
			return fmt.Errorf("%s: %w", eventsPath, err)
		}
	}
	w, err := os.Create(out)
	if err != nil {
		return err
	}
	bw := bufio.NewWriter(w)
	render(bw, title, data, events)
	if err := bw.Flush(); err != nil {
		w.Close()
		return err
	}
	return w.Close()
}

// readCSV returns each column of a CSV with a header row, by name.
func readCSV(r io.Reader) (map[string][]float64, error) {
	recs, err := csv.NewReader(r).ReadAll()
	if err != nil {
		return nil, err
	}
	if len(recs) < 2 {
		return nil, errors.New("no rows")
	}
	cols := make(map[string][]float64, len(recs[0]))
	for i, rec := range recs[1:] {
		for j, name := range recs[0] {
			v, err := strconv.ParseFloat(rec[j], 64)
			if err != nil {
				return nil, fmt.Errorf("row %d, %s: %w", i+1, name, err)
			}
			cols[name] = append(cols[name], v)
		}
	}
	if _, ok := cols["second"]; !ok {
		return nil, errors.New(`no "second" column`)
	}
	return cols, nil
}

// readEvents reads "seconds<TAB>kind<TAB>label" lines.
func readEvents(r io.Reader) ([]event, error) {
	var events []event
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		if sc.Text() == "" {
			continue
		}
		parts := strings.SplitN(sc.Text(), "\t", 3)
		if len(parts) != 3 {
			return nil, fmt.Errorf("line %d: want 3 tab-separated fields", n)
		}
		at, err := strconv.ParseFloat(parts[0], 64)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", n, err)
		}
		events = append(events, event{at, parts[1] == "inject", parts[2]})
	}
	return events, sc.Err()
}

func render(w io.Writer, title string, data map[string][]float64, events []event) {
	secs := data["second"]
	xmax := secs[len(secs)-1] + 1
	x := func(s float64) float64 { return marginLeft + s/xmax*plotWidth }

	// Prism's events are numbered markers explained in a list below the
	// panels; several at the same time share a marker.
	var reactions []event
	for _, e := range events {
		if !e.inject {
			reactions = append(reactions, e)
		}
	}
	height := top + len(panels)*(panelHeight+panelGap) + 20*len(reactions) + 20

	fmt.Fprintf(w, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d" font-family="sans-serif" font-size="12">`+"\n", width, height, width, height)
	fmt.Fprintf(w, `<rect width="100%%" height="100%%" fill="#ffffff"/>`+"\n")
	fmt.Fprintf(w, `<text x="%d" y="30" font-size="18" font-weight="bold">%s</text>`+"\n", marginLeft, esc(title))

	for i, p := range panels {
		y0 := float64(top + i*(panelHeight+panelGap) + 20) // top of the plot area
		ymax := p.minTop
		for _, s := range p.lines {
			for _, v := range data[s.column] {
				ymax = max(ymax, v)
			}
		}
		step := niceStep(ymax / 4)
		ymax = math.Ceil(ymax/step) * step
		y := func(v float64) float64 { return y0 + panelHeight - v/ymax*panelHeight }

		fmt.Fprintf(w, `<text x="%d" y="%.1f" font-weight="bold">%s</text>`+"\n", marginLeft, y0-8, esc(p.title))
		for k := 0; float64(k)*step <= ymax+step/2; k++ {
			v := float64(k) * step
			fmt.Fprintf(w, `<line x1="%d" x2="%d" y1="%.1f" y2="%.1f" stroke="#e5e7eb"/>`, marginLeft, marginLeft+plotWidth, y(v), y(v))
			fmt.Fprintf(w, `<text x="%d" y="%.1f" text-anchor="end" fill="#555">%s</text>`+"\n", marginLeft-6, y(v)+4, num(v))
		}
		for s := 0.0; s <= xmax; s += 5 {
			fmt.Fprintf(w, `<text x="%.1f" y="%.1f" text-anchor="middle" fill="#555">%gs</text>`+"\n", x(s), y0+panelHeight+16, s)
		}
		if p.ref != 0 {
			fmt.Fprintf(w, `<line x1="%d" x2="%d" y1="%.1f" y2="%.1f" stroke="#111" stroke-dasharray="2 3"/>`+"\n", marginLeft, marginLeft+plotWidth, y(p.ref), y(p.ref))
		}
		for _, e := range events {
			if e.inject {
				fmt.Fprintf(w, `<line x1="%.1f" x2="%.1f" y1="%.1f" y2="%.1f" stroke="#888" stroke-dasharray="5 4"/>`+"\n", x(e.at), x(e.at), y0, y0+panelHeight)
				if i == 0 {
					fmt.Fprintf(w, `<text x="%.1f" y="%.1f" fill="#444">%s</text>`+"\n", x(e.at)+4, y0+12, esc(e.label))
				}
			}
		}
		for n, e := range reactions {
			fmt.Fprintf(w, `<line x1="%.1f" x2="%.1f" y1="%.1f" y2="%.1f" stroke="#d68a1f" stroke-width="2"/>`+"\n", x(e.at), x(e.at), y0+panelHeight-8, y0+panelHeight)
			if i == 0 {
				fmt.Fprintf(w, `<text x="%.1f" y="%.1f" text-anchor="middle" fill="#d68a1f" font-size="10">%d</text>`+"\n", x(e.at), y0+panelHeight-10-float64(n%3)*10, n+1)
			}
		}
		for _, s := range p.lines {
			vals := data[s.column]
			var pts strings.Builder
			for j, v := range vals {
				// A row covers its whole second, so it is drawn at the middle.
				fmt.Fprintf(&pts, "%.1f,%.1f ", x(secs[j]+0.5), y(v))
			}
			fmt.Fprintf(w, `<polyline fill="none" stroke="%s" stroke-width="2" points="%s"/>`+"\n", s.color, strings.TrimSpace(pts.String()))
		}
		lx := float64(marginLeft + plotWidth)
		for j := len(p.lines) - 1; j >= 0; j-- {
			s := p.lines[j]
			lx -= 10 + 7*float64(len(s.label)) + 24
			fmt.Fprintf(w, `<line x1="%.1f" x2="%.1f" y1="%.1f" y2="%.1f" stroke="%s" stroke-width="3"/>`, lx, lx+18, y0-12, y0-12, s.color)
			fmt.Fprintf(w, `<text x="%.1f" y="%.1f">%s</text>`+"\n", lx+22, y0-8, esc(s.label))
		}
		fmt.Fprintf(w, `<rect x="%d" y="%.1f" width="%d" height="%d" fill="none" stroke="#999"/>`+"\n", marginLeft, y0, plotWidth, panelHeight)
	}

	ly := top + len(panels)*(panelHeight+panelGap)
	for n, e := range reactions {
		fmt.Fprintf(w, `<text x="%d" y="%d"><tspan fill="#d68a1f" font-weight="bold">%d</tspan>  %.1fs  %s</text>`+"\n", marginLeft, ly+20*n, n+1, e.at, esc(e.label))
	}
	fmt.Fprintln(w, "</svg>")
}

// niceStep rounds a raw axis step up to 1, 2 or 5 times a power of ten.
func niceStep(raw float64) float64 {
	if raw <= 0 {
		return 1
	}
	p := math.Pow(10, math.Floor(math.Log10(raw)))
	for _, m := range []float64{1, 2, 5, 10} {
		if raw <= m*p {
			return m * p
		}
	}
	return 10 * p
}

func num(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

func esc(s string) string {
	var b strings.Builder
	xml.EscapeText(&b, []byte(s))
	return b.String()
}
