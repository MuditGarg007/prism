package main

import (
	"encoding/xml"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testCSV = `second,sent,ok,status_5xx,status_4xx,failed,error_pct,p50_ms,p99_ms,backend_reqs,amplification
0,300,300,0,0,0,0.00,20.50,21.25,300,1.000
1,300,290,10,0,0,3.33,20.47,40.10,330,1.100
2,300,300,0,0,0,0.00,20.49,21.29,301,1.003
`

const testEvents = "1\tinject\tkill b3 & <restart>\n1.4\tprism\t19103 circuit breaker state changed -> open\n"

func TestRender(t *testing.T) {
	dir := t.TempDir()
	csvPath, evPath, out := filepath.Join(dir, "load.csv"), filepath.Join(dir, "events.tsv"), filepath.Join(dir, "graph.svg")
	os.WriteFile(csvPath, []byte(testCSV), 0o644)
	os.WriteFile(evPath, []byte(testEvents), 0o644)
	if err := run(csvPath, evPath, "Test run", out); err != nil {
		t.Fatal(err)
	}
	svg, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	// Well-formed XML, which also proves the labels were escaped.
	d := xml.NewDecoder(strings.NewReader(string(svg)))
	lines := 0
	for {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("invalid SVG: %v", err)
		}
		if se, ok := tok.(xml.StartElement); ok && se.Name.Local == "polyline" {
			lines++
		}
	}
	want := 0
	for _, p := range panels {
		want += len(p.lines)
	}
	if lines != want {
		t.Errorf("%d polylines, want one per series (%d)", lines, want)
	}
	for _, s := range []string{"Test run", "kill b3 &amp; &lt;restart&gt;", "1.4s  19103 circuit breaker state changed -&gt; open"} {
		if !strings.Contains(string(svg), s) {
			t.Errorf("SVG lacks %q", s)
		}
	}
}

func TestReadErrors(t *testing.T) {
	if _, err := readCSV(strings.NewReader("second,sent\n")); err == nil {
		t.Error("a CSV without rows was accepted")
	}
	if _, err := readCSV(strings.NewReader("second,sent\n0,x\n")); err == nil {
		t.Error("a non-numeric cell was accepted")
	}
	if _, err := readEvents(strings.NewReader("1\tinject\n")); err == nil {
		t.Error("an event line with two fields was accepted")
	}
}

func TestNiceStep(t *testing.T) {
	for _, tt := range []struct{ raw, want float64 }{
		{0, 1}, {0.3, 0.5}, {1, 1}, {1.2, 2}, {3, 5}, {7, 10}, {80, 100}, {120, 200},
	} {
		if got := niceStep(tt.raw); got != tt.want {
			t.Errorf("niceStep(%v) = %v, want %v", tt.raw, got, tt.want)
		}
	}
}
