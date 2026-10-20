package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func get(t *testing.T, url string) (*http.Response, string, error) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp, string(b), err
}

func TestFaultServer(t *testing.T) {
	fs := newFaultServer("b1", Faults{BodySize: 10})
	srv := httptest.NewServer(fs)
	defer srv.Close()

	resp, body, err := get(t, srv.URL+"/x")
	if err != nil || resp.StatusCode != 200 || body != "xxxxxxxxxx" || resp.Header.Get("X-Backend") != "b1" {
		t.Fatalf("healthy: %d %q %v", resp.StatusCode, body, err)
	}

	if resp, body, _ := get(t, srv.URL+"/_fault?error_rate=1&error_status=502&latency=20ms"); resp.StatusCode != 200 ||
		!strings.Contains(body, `"error_rate":1`) || !strings.Contains(body, `"error_status":502`) {
		t.Fatalf("control: %d %s", resp.StatusCode, body)
	}
	start := time.Now()
	if resp, _, _ := get(t, srv.URL+"/x"); resp.StatusCode != 502 {
		t.Errorf("with error_rate=1 got %d, want 502", resp.StatusCode)
	}
	if d := time.Since(start); d < 20*time.Millisecond {
		t.Errorf("answered after %v, under the latency", d)
	}

	get(t, srv.URL+"/_fault?error_rate=0&latency=0s&drop_rate=1")
	if _, body, err := get(t, srv.URL+"/x"); err == nil || body != "xxxxx" {
		t.Errorf("with drop_rate=1 read %q, %v; want half the body and an error", body, err)
	}

	if resp, _, _ := get(t, srv.URL+"/_fault?error_rte=1"); resp.StatusCode != 400 {
		t.Errorf("unknown fault got %d, want 400", resp.StatusCode)
	}
	if resp, _, _ := get(t, srv.URL+"/_fault?latency=fast"); resp.StatusCode != 400 {
		t.Errorf("malformed fault got %d, want 400", resp.StatusCode)
	}
	if _, body, _ := get(t, srv.URL+"/_stats"); body != `{"name":"b1","requests":3}`+"\n" {
		t.Errorf("stats = %q, want 3 requests, control calls not counted", body)
	}
}

func TestFaultServerErrorRate(t *testing.T) {
	fs := newFaultServer("b", Faults{ErrorRate: 0.5})
	draws := []float64{0.1, 0.9}
	fs.randF = func() float64 { v := draws[0]; draws = draws[1:]; return v }
	srv := httptest.NewServer(fs)
	defer srv.Close()
	for _, want := range []int{503, 200} {
		if resp, _, _ := get(t, srv.URL+"/"); resp.StatusCode != want {
			t.Errorf("got %d, want %d", resp.StatusCode, want)
		}
	}
}
