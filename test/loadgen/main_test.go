package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAggregate(t *testing.T) {
	ms := time.Millisecond
	results := []result{
		{0, 1 * ms, 200}, {0, 3 * ms, 200}, {0, 2 * ms, 503}, {0, 9 * ms, 0},
		{1, 5 * ms, 404},
		{7, ms, 200}, // past the end
	}
	rows := aggregate(results, []int64{6}, 2)
	want := []row{
		{second: 0, sent: 4, ok: 2, status5xx: 1, failed: 1, p50: 2 * ms, p99: 9 * ms, backendReqs: 6},
		{second: 1, sent: 1, other: 1, p50: 5 * ms, p99: 5 * ms},
	}
	if len(rows) != len(want) {
		t.Fatalf("got %d rows", len(rows))
	}
	for i := range want {
		if rows[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, rows[i], want[i])
		}
	}
}

func TestWriteCSV(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.csv")
	err := writeCSV(path, []row{{second: 0, sent: 4, ok: 2, status5xx: 1, failed: 1, p50: 2 * time.Millisecond, backendReqs: 6}, {second: 1}})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	want := "second,sent,ok,status_5xx,status_4xx,failed,error_pct,p50_ms,p99_ms,backend_reqs,amplification\n" +
		"0,4,2,1,0,1,50.00,2.00,0.00,6,1.500\n" +
		"1,0,0,0,0,0,0.00,0.00,0.00,0,0.000\n"
	if string(b) != want {
		t.Errorf("CSV:\n%s\nwant:\n%s", b, want)
	}
}

func TestSummarize(t *testing.T) {
	var sb strings.Builder
	summarize(&sb, []row{{sent: 10, ok: 9, status5xx: 1, backendReqs: 12}, {sent: 10, ok: 10, backendReqs: 10}}, true)
	for _, want := range []string{"sent 20, ok 19, errors 1 (5.00%)", "amplification 1.100 overall, 1.200 at worst"} {
		if !strings.Contains(sb.String(), want) {
			t.Errorf("summary %q lacks %q", sb.String(), want)
		}
	}
}
