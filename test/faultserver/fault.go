package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Faults is how badly the server behaves. It can be changed while serving.
type Faults struct {
	Latency     time.Duration `json:"latency"`      // added before answering
	ErrorRate   float64       `json:"error_rate"`   // share of requests answered with ErrorStatus
	ErrorStatus int           `json:"error_status"` // 503 by default
	DropRate    float64       `json:"drop_rate"`    // share of requests cut off mid-body
	BodySize    int           `json:"body_size"`    // bytes in a normal response
	// CacheControl, when set, is sent on normal responses, so a caching
	// proxy can store them.
	CacheControl string `json:"cache_control"`
}

// faultServer serves every path with the current faults, except two control
// paths: /_fault changes the faults and /_stats reports the request count.
// /healthz gets the faults too, but is not counted, so health checks do not
// show up as load.
type faultServer struct {
	name     string
	mu       sync.Mutex
	faults   Faults
	requests atomic.Int64
	randF    func() float64 // replaced by tests
}

func newFaultServer(name string, f Faults) *faultServer {
	if f.ErrorStatus == 0 {
		f.ErrorStatus = http.StatusServiceUnavailable
	}
	return &faultServer{name: name, faults: f, randF: rand.Float64}
}

func (s *faultServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/_fault":
		s.control(w, r)
		return
	case "/_stats":
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"name":%q,"requests":%d}`+"\n", s.name, s.requests.Load())
		return
	}
	if r.URL.Path != "/healthz" {
		s.requests.Add(1)
	}
	s.mu.Lock()
	f := s.faults
	s.mu.Unlock()

	io.Copy(io.Discard, r.Body)
	if f.Latency > 0 {
		select {
		case <-time.After(f.Latency):
		case <-r.Context().Done():
			return
		}
	}
	w.Header().Set("X-Backend", s.name)
	if f.ErrorRate > 0 && s.randF() < f.ErrorRate {
		http.Error(w, s.name+": injected error", f.ErrorStatus)
		return
	}
	body := make([]byte, f.BodySize)
	for i := range body {
		body[i] = 'x'
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	if f.CacheControl != "" {
		w.Header().Set("Cache-Control", f.CacheControl)
	}
	if f.DropRate > 0 && s.randF() < f.DropRate {
		// Promise the whole body, send half, and abort: the client sees
		// a 200 whose body ends early.
		w.Write(body[:len(body)/2])
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	}
	w.Write(body)
}

// control applies the query parameters of r to the faults, then answers
// with the faults now in force. Unknown or malformed parameters are
// refused, so a typo in a chaos script is not silently ignored.
func (s *faultServer) control(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	f := s.faults
	s.mu.Unlock()
	for k, vs := range r.URL.Query() {
		v := vs[len(vs)-1]
		var err error
		switch k {
		case "latency":
			f.Latency, err = time.ParseDuration(v)
		case "error_rate":
			f.ErrorRate, err = strconv.ParseFloat(v, 64)
		case "error_status":
			f.ErrorStatus, err = strconv.Atoi(v)
		case "drop_rate":
			f.DropRate, err = strconv.ParseFloat(v, 64)
		case "body_size":
			f.BodySize, err = strconv.Atoi(v)
		case "cache_control":
			f.CacheControl = v
		default:
			err = fmt.Errorf("unknown fault")
		}
		if err != nil {
			http.Error(w, fmt.Sprintf("%s=%s: %v", k, v, err), http.StatusBadRequest)
			return
		}
	}
	s.mu.Lock()
	s.faults = f
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(f)
}
