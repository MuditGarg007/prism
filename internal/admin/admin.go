// Package admin serves the admin API: cache statistics and purging. It has
// no authentication, so it belongs on a loopback listener.
package admin

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/MuditGarg007/prism/internal/runtime"
)

// maxBody bounds a request body. A purge is a few hundred bytes.
const maxBody = 64 << 10

// Handler serves the admin API from whichever Runtime is current when each
// request arrives, so it keeps working across reloads.
type Handler struct {
	cur *runtime.Current
	log *slog.Logger
	mux *http.ServeMux
}

// New returns the admin API for cur.
func New(cur *runtime.Current, log *slog.Logger) *Handler {
	h := &Handler{cur: cur, log: log, mux: http.NewServeMux()}
	h.mux.HandleFunc("GET /cache/stats", h.cacheStats)
	h.mux.HandleFunc("POST /cache/purge", h.cachePurge)
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	h.mux.ServeHTTP(w, r)
}

// CacheStats is the body of GET /cache/stats. The counters only grow; a
// Prometheus exporter in M5 maps them onto counters and gauges.
type CacheStats struct {
	Entries   int    `json:"entries"`
	Bytes     int64  `json:"bytes"`
	MaxBytes  int64  `json:"max_bytes"`
	Evictions uint64 `json:"evictions"`
	Waiting   int    `json:"waiting"` // requests blocked on another's fetch

	Hit         uint64 `json:"hit"`
	Miss        uint64 `json:"miss"`
	Stale       uint64 `json:"stale"`
	Revalidated uint64 `json:"revalidated"`
	Bypass      uint64 `json:"bypass"`
}

func (h *Handler) cacheStats(w http.ResponseWriter, r *http.Request) {
	c := h.cache(w)
	if c == nil {
		return
	}
	st, n := c.Store.Stats(), c.Stats.Snapshot()
	writeJSON(w, http.StatusOK, CacheStats{
		Entries:     st.Entries,
		Bytes:       st.Bytes,
		MaxBytes:    c.MaxSize,
		Evictions:   st.Evictions,
		Waiting:     c.Flight.Waiting(),
		Hit:         n.Hit,
		Miss:        n.Miss,
		Stale:       n.Stale,
		Revalidated: n.Revalidated,
		Bypass:      n.Bypass,
	})
}

// PurgeRequest is the body of POST /cache/purge. Path includes any query.
// With Prefix set, every path that starts with Path is purged.
type PurgeRequest struct {
	Host   string `json:"host"`
	Path   string `json:"path"`
	Prefix bool   `json:"prefix"`
}

// PurgeResponse says how many entries a purge deleted, every variant,
// method and scheme counted.
type PurgeResponse struct {
	Purged int `json:"purged"`
}

func (h *Handler) cachePurge(w http.ResponseWriter, r *http.Request) {
	c := h.cache(w)
	if c == nil {
		return
	}
	var req PurgeRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid purge request: "+err.Error())
		return
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid purge request: trailing data after the object")
		return
	}
	// Without a leading slash, a prefix purge of "a.com" + "" would also
	// match "a.company".
	if req.Host == "" || !strings.HasPrefix(req.Path, "/") {
		writeError(w, http.StatusBadRequest, `"host" is required and "path" must start with "/"`)
		return
	}
	host := normalizeHost(req.Host)
	n := c.Store.Purge(host, req.Path, req.Prefix)
	h.log.Info("cache purged", "host", host, "path", req.Path, "prefix", req.Prefix, "entries", n)
	writeJSON(w, http.StatusOK, PurgeResponse{Purged: n})
}

// normalizeHost matches host to the keys cache.PrimaryKey builds, which
// are lowercase and drop the scheme's default port. A purge covers both
// schemes, so both default ports are dropped.
func normalizeHost(host string) string {
	host = strings.ToLower(host)
	for _, port := range []string{":80", ":443"} {
		if h, ok := strings.CutSuffix(host, port); ok {
			return h
		}
	}
	return host
}

// cache returns the current runtime's cache, or answers 404 and returns nil
// when no route caches.
func (h *Handler) cache(w http.ResponseWriter) *runtime.Cache {
	c := h.cur.Load().Cache
	if c == nil {
		writeError(w, http.StatusNotFound, "no route has caching enabled")
	}
	return c
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
