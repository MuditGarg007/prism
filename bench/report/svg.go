package main

import (
	"fmt"
	"html"
	"io"
	"math"
	"slices"
	"strings"
	"time"
)

var colors = []string{"#6b7280", "#16a34a", "#2563eb", "#9333ea", "#ea580c", "#db2777", "#0891b2"}

const (
	width, height = 760, 440
	marginLeft    = 70
	marginRight   = 170 // the legend
	marginTop     = 50
	marginBottom  = 50
	plotWidth     = width - marginLeft - marginRight
	plotHeight    = height - marginTop - marginBottom
	maxNines      = 4 // the x axis ends at 99.99%
)

// series is one scenario's line on the graph.
type series struct {
	name  string
	color string
	pts   []hdrPoint
}

// writeSVG draws the latency percentile distribution of every scenario at
// the highest fixed rate, with the percentile on a log scale of nines.
func writeSVG(w io.Writer, rows []row) error {
	rate := ""
	for _, r := range rows {
		if r.rate != "max" && rateOrder(r.rate) > rateOrder(rate) {
			rate = r.rate
		}
	}
	var ss []series
	for _, r := range rows {
		if r.rate != rate {
			continue
		}
		if rn := medianRun(r.runs); len(rn.hdr) > 0 {
			ss = append(ss, series{name: r.scenario, color: colors[len(ss)%len(colors)], pts: rn.hdr})
		}
	}

	// x is the number of nines: 1 for 90%, 2 for 99% and so on.
	nines := func(p float64) float64 { return math.Log10(1 / (1 - p)) }
	var top time.Duration
	for _, s := range ss {
		for _, p := range s.pts {
			if p.percentile < 1 && nines(p.percentile) <= maxNines {
				top = max(top, p.latency)
			}
		}
	}
	step := niceStep(top, 5)
	ymax := time.Duration(math.Ceil(float64(top)/float64(step))) * step
	if ymax == 0 {
		ymax = time.Millisecond
		step = ymax / 5
	}
	x := func(n float64) float64 { return marginLeft + n/maxNines*plotWidth }
	y := func(d time.Duration) float64 { return marginTop + plotHeight - float64(d)/float64(ymax)*plotHeight }

	fmt.Fprintf(w, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d" font-family="sans-serif" font-size="12">`+"\n", width, height, width, height)
	fmt.Fprintf(w, `<rect width="%d" height="%d" fill="#fff"/>`+"\n", width, height)
	fmt.Fprintf(w, `<text x="%d" y="30" font-size="16" font-weight="bold">Latency by percentile at %s req/s</text>`+"\n", marginLeft, html.EscapeString(rate))
	for v := time.Duration(0); v <= ymax; v += step {
		fmt.Fprintf(w, `<line x1="%d" x2="%d" y1="%.1f" y2="%.1f" stroke="#e5e7eb"/>`, marginLeft, marginLeft+plotWidth, y(v), y(v))
		fmt.Fprintf(w, `<text x="%d" y="%.1f" text-anchor="end">%s</text>`+"\n", marginLeft-6, y(v)+4, axisLabel(v, ymax, step))
	}
	for n := 0; n <= maxNines; n++ {
		label := "0%"
		if n > 0 {
			label = strings.TrimRight(fmt.Sprintf("%.*f", n, 100-100/math.Pow(10, float64(n))), "0") + "%"
			label = strings.Replace(label, ".%", "%", 1)
		}
		fmt.Fprintf(w, `<line x1="%.1f" x2="%.1f" y1="%d" y2="%d" stroke="#e5e7eb"/>`, x(float64(n)), x(float64(n)), marginTop, marginTop+plotHeight)
		fmt.Fprintf(w, `<text x="%.1f" y="%d" text-anchor="middle">%s</text>`+"\n", x(float64(n)), marginTop+plotHeight+18, label)
	}
	fmt.Fprintf(w, `<text x="%d" y="%d" text-anchor="middle">percentile</text>`+"\n", marginLeft+plotWidth/2, height-10)
	for i, s := range ss {
		var pts strings.Builder
		for _, p := range s.pts {
			if p.percentile >= 1 || nines(p.percentile) > maxNines {
				continue
			}
			fmt.Fprintf(&pts, "%.1f,%.1f ", x(nines(p.percentile)), y(min(p.latency, ymax)))
		}
		fmt.Fprintf(w, `<polyline fill="none" stroke="%s" stroke-width="2" points="%s"/>`+"\n", s.color, strings.TrimSpace(pts.String()))
		ly := marginTop + 10 + 22*i
		fmt.Fprintf(w, `<line x1="%d" x2="%d" y1="%d" y2="%d" stroke="%s" stroke-width="3"/>`, width-marginRight+16, width-marginRight+36, ly, ly, s.color)
		fmt.Fprintf(w, `<text x="%d" y="%d">%s</text>`+"\n", width-marginRight+42, ly+4, html.EscapeString(s.name))
	}
	fmt.Fprintf(w, `<rect x="%d" y="%d" width="%d" height="%d" fill="none" stroke="#999"/>`+"\n", marginLeft, marginTop, plotWidth, plotHeight)
	_, err := fmt.Fprintln(w, `</svg>`)
	return err
}

// medianRun returns the run whose p99 is the median one.
func medianRun(runs []run) run {
	rs := slices.Clone(runs)
	slices.SortFunc(rs, func(a, b run) int { return int(a.report.Latencies.P99 - b.report.Latencies.P99) })
	return rs[len(rs)/2]
}

// niceStep returns a step of 1, 2 or 5 times a power of ten that divides
// top into about n parts.
func niceStep(top time.Duration, n int) time.Duration {
	if top <= 0 {
		return time.Microsecond
	}
	raw := float64(top) / float64(n)
	pow := math.Pow(10, math.Floor(math.Log10(raw)))
	for _, m := range []float64{1, 2, 5, 10} {
		if m*pow >= raw {
			return time.Duration(m * pow)
		}
	}
	return time.Duration(10 * pow)
}

// axisLabel formats a y-axis tick. Every tick uses the unit dur would pick
// for ymax, so the axis never mixes µs and ms, with a decimal only when
// step needs one.
func axisLabel(v, ymax, step time.Duration) string {
	unit, name := time.Microsecond, "µs"
	if ymax >= 10*time.Millisecond {
		unit, name = time.Millisecond, "ms"
	}
	if step%unit == 0 {
		return fmt.Sprintf("%d %s", v/unit, name)
	}
	return fmt.Sprintf("%.1f %s", float64(v)/float64(unit), name)
}
