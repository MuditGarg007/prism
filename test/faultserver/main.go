// Command faultserver is a backend for chaos tests. It answers every path
// with a fixed body, and can be told to be slow, to fail a share of requests
// with an error status, or to cut a share of responses off mid-body.
//
// The faults are set with flags at start, and changed while running with
// GET /_fault?error_rate=1&latency=50ms. GET /_stats returns the number of
// requests served, not counting /healthz, for measuring how much load
// reaches the backend.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:9101", "address to listen on")
	name := flag.String("name", "", "name sent in X-Backend (default: the address)")
	var f Faults
	flag.DurationVar(&f.Latency, "latency", 0, "delay before answering")
	flag.Float64Var(&f.ErrorRate, "error-rate", 0, "share of requests answered with -error-status")
	flag.IntVar(&f.ErrorStatus, "error-status", http.StatusServiceUnavailable, "status for injected errors")
	flag.Float64Var(&f.DropRate, "drop-rate", 0, "share of responses cut off mid-body")
	flag.IntVar(&f.BodySize, "body-size", 64, "bytes in a normal response")
	flag.StringVar(&f.CacheControl, "cache-control", "", "Cache-Control sent on normal responses")
	flag.Parse()
	if *name == "" {
		*name = *addr
	}

	srv := &http.Server{
		Addr:              *addr,
		Handler:           newFaultServer(*name, f),
		ReadHeaderTimeout: 10 * time.Second,
	}
	fmt.Fprintf(os.Stderr, "faultserver %s listening on %s\n", *name, *addr)
	log.Fatal(srv.ListenAndServe())
}
