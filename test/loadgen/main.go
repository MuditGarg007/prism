// Command loadgen sends GET requests at a fixed rate and writes one CSV row
// per second: what was sent, how it was answered, latency percentiles and,
// when given the backends' /_stats URLs, how many requests reached the
// backends. backend_reqs over sent is the load amplification from retries.
//
// The rate is open loop: a slow or failing proxy does not slow the sender
// down, as real clients would not.
package main

import (
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// result is how one request went.
type result struct {
	second int // seconds from the start when it was sent
	dur    time.Duration
	status int // 0 when no response arrived
}

// row is one second of the CSV.
type row struct {
	second                     int
	sent, ok, status5xx, other int
	failed                     int // no response at all
	p50, p99                   time.Duration
	backendReqs                int64
}

func main() {
	url := flag.String("url", "http://127.0.0.1:8090/", "URL to load")
	rate := flag.Int("rate", 200, "requests per second")
	duration := flag.Duration("duration", 30*time.Second, "how long to send for")
	timeout := flag.Duration("timeout", 5*time.Second, "per-request timeout")
	out := flag.String("out", "load.csv", "CSV file to write")
	stats := flag.String("stats", "", "comma-separated faultserver /_stats URLs to poll for backend request counts")
	flag.Parse()
	if *rate < 1 {
		fmt.Fprintln(os.Stderr, "loadgen: -rate must be at least 1")
		os.Exit(2)
	}

	client := &http.Client{
		Timeout:   *timeout,
		Transport: &http.Transport{MaxIdleConnsPerHost: 512, DisableCompression: true},
	}
	var statURLs []string
	if *stats != "" {
		statURLs = strings.Split(*stats, ",")
	}
	seconds := int((*duration + time.Second - 1) / time.Second)

	start := time.Now()
	poller := newPoller(statURLs, seconds)
	go poller.run(start)

	var (
		mu      sync.Mutex
		results []result
		wg      sync.WaitGroup
	)
	interval := time.Second / time.Duration(*rate)
	for i := 0; ; i++ {
		at := start.Add(time.Duration(i) * interval)
		if at.Sub(start) >= *duration {
			break
		}
		time.Sleep(time.Until(at))
		wg.Go(func() {
			r := result{second: int(at.Sub(start) / time.Second)}
			t := time.Now()
			resp, err := client.Get(*url)
			if err == nil {
				_, err = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				r.status = resp.StatusCode
				if err != nil {
					r.status = 0 // the body broke off
				}
			}
			r.dur = time.Since(t)
			mu.Lock()
			results = append(results, r)
			mu.Unlock()
		})
	}
	wg.Wait()
	poller.stop()

	rows := aggregate(results, poller.deltas(), seconds)
	if err := writeCSV(*out, rows); err != nil {
		fmt.Fprintln(os.Stderr, "loadgen:", err)
		os.Exit(1)
	}
	summarize(os.Stderr, rows, len(statURLs) > 0)
}

// aggregate turns results and per-second backend request counts into rows.
func aggregate(results []result, backend []int64, seconds int) []row {
	rows := make([]row, seconds)
	durs := make([][]time.Duration, seconds)
	for i := range rows {
		rows[i].second = i
		if i < len(backend) {
			rows[i].backendReqs = backend[i]
		}
	}
	for _, r := range results {
		if r.second >= seconds {
			continue
		}
		w := &rows[r.second]
		w.sent++
		switch {
		case r.status == 0:
			w.failed++
		case r.status < 400:
			w.ok++
		case r.status >= 500:
			w.status5xx++
		default:
			w.other++
		}
		durs[r.second] = append(durs[r.second], r.dur)
	}
	for i, d := range durs {
		slices.Sort(d)
		rows[i].p50, rows[i].p99 = percentile(d, 0.50), percentile(d, 0.99)
	}
	return rows
}

// percentile returns the nearest-rank percentile p of sorted.
func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := int(p*float64(len(sorted))+0.5) - 1
	return sorted[min(max(i, 0), len(sorted)-1)]
}

func writeCSV(path string, rows []row) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	w := csv.NewWriter(f)
	w.Write([]string{"second", "sent", "ok", "status_5xx", "status_4xx", "failed", "error_pct", "p50_ms", "p99_ms", "backend_reqs", "amplification"})
	for _, r := range rows {
		errPct, amp := 0.0, 0.0
		if r.sent > 0 {
			errPct = 100 * float64(r.status5xx+r.failed) / float64(r.sent)
			amp = float64(r.backendReqs) / float64(r.sent)
		}
		w.Write([]string{
			strconv.Itoa(r.second), strconv.Itoa(r.sent), strconv.Itoa(r.ok),
			strconv.Itoa(r.status5xx), strconv.Itoa(r.other), strconv.Itoa(r.failed),
			strconv.FormatFloat(errPct, 'f', 2, 64),
			strconv.FormatFloat(ms(r.p50), 'f', 2, 64), strconv.FormatFloat(ms(r.p99), 'f', 2, 64),
			strconv.FormatInt(r.backendReqs, 10), strconv.FormatFloat(amp, 'f', 3, 64),
		})
	}
	w.Flush()
	if err := w.Error(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

func summarize(w io.Writer, rows []row, haveBackend bool) {
	var sent, ok, bad int
	var backend int64
	maxAmp := 0.0
	for _, r := range rows {
		sent += r.sent
		ok += r.ok
		bad += r.status5xx + r.failed
		backend += r.backendReqs
		if r.sent > 0 {
			maxAmp = max(maxAmp, float64(r.backendReqs)/float64(r.sent))
		}
	}
	if sent == 0 {
		fmt.Fprintln(w, "loadgen: nothing sent")
		return
	}
	fmt.Fprintf(w, "sent %d, ok %d, errors %d (%.2f%%)\n", sent, ok, bad, 100*float64(bad)/float64(sent))
	if haveBackend {
		fmt.Fprintf(w, "backend requests %d, amplification %.3f overall, %.3f at worst\n",
			backend, float64(backend)/float64(sent), maxAmp)
	}
}

// pollEvery is how often the faultservers' counters are read. Requests a
// server answers after its last read are lost when it is killed, so reading
// often keeps that loss small.
const pollEvery = 100 * time.Millisecond

// poller samples the request counters of faultservers and adds up how many
// requests reached them in each second.
type poller struct {
	urls   []string
	client *http.Client
	last   []int64 // last count seen from each server
	mu     sync.Mutex
	perSec []int64
	done   chan struct{}
	exited chan struct{}
}

func newPoller(urls []string, seconds int) *poller {
	return &poller{
		urls:   urls,
		client: &http.Client{Timeout: 500 * time.Millisecond},
		last:   make([]int64, len(urls)),
		perSec: make([]int64, seconds),
		done:   make(chan struct{}),
		exited: make(chan struct{}),
	}
}

func (p *poller) run(start time.Time) {
	defer close(p.exited)
	if len(p.urls) == 0 {
		return
	}
	p.sample() // the baseline
	for i := 1; ; i++ {
		end := time.Duration(i) * pollEvery
		select {
		case <-p.done:
			return
		case <-time.After(time.Until(start.Add(end))):
		}
		d := p.sample()
		p.mu.Lock()
		// The sample covers the interval that ends at end.
		if sec := int((end - 1) / time.Second); sec < len(p.perSec) {
			p.perSec[sec] += d
		}
		p.mu.Unlock()
	}
}

// sample returns how many requests reached the servers since the last
// sample. A server that is down adds nothing; one that restarted counts
// from zero again.
func (p *poller) sample() int64 {
	var total int64
	for i, u := range p.urls {
		n, err := p.count(u)
		if err != nil {
			continue
		}
		if n < p.last[i] {
			p.last[i] = 0
		}
		total += n - p.last[i]
		p.last[i] = n
	}
	return total
}

func (p *poller) count(url string) (int64, error) {
	resp, err := p.client.Get(url)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	var s struct{ Requests int64 }
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return 0, err
	}
	return s.Requests, nil
}

func (p *poller) stop() {
	close(p.done)
	<-p.exited
}

func (p *poller) deltas() []int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.perSec)
}
