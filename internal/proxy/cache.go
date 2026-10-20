package proxy

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/MuditGarg007/prism/internal/cache"
	"github.com/MuditGarg007/prism/internal/resilience"
)

// X-Cache values. They go into the access log and the stats too, so they
// must stay stable.
const (
	cacheHit         = "HIT"
	cacheMiss        = "MISS"
	cacheStale       = "STALE"
	cacheRevalidated = "REVALIDATED"
	cacheBypass      = "BYPASS"
)

// Cache is a route's response caching. Store, Flight and Stats are shared
// by every route that caches; the rest is the route's own.
type Cache struct {
	Store        *cache.Store
	Flight       *cache.Flight
	Stats        *cache.Stats
	MaxObject    int64            // larger responses are passed through unstored
	LockTimeout  time.Duration    // the longest wait for another request's fetch
	BypassHeader string           // canonical name of a header that skips the lookup; "" for none
	StaleIfError time.Duration    // the longest an entry is served stale when the backend fails
	Now          func() time.Time // nil for time.Now; tests set a fake clock
}

func (c *Cache) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Cache) count(label string) {
	s := c.Stats
	if s == nil {
		return
	}
	switch label {
	case cacheHit:
		s.Hit.Add(1)
	case cacheMiss:
		s.Miss.Add(1)
	case cacheStale:
		s.Stale.Add(1)
	case cacheRevalidated:
		s.Revalidated.Add(1)
	case cacheBypass:
		s.Bypass.Add(1)
	}
}

// staleOK reports whether e, which is stale, may be served because the
// backend failed (stale-if-error, RFC 5861). The route's setting is the
// window, and a response's own stale-if-error can only narrow it.
// must-revalidate and no-cache forbid serving stale, and so does s-maxage,
// which implies proxy-revalidate (RFC 9111 §5.2.2.10).
func (c *Cache) staleOK(e *cache.Entry, now time.Time) bool {
	cc := e.CC
	if c.StaleIfError <= 0 || cc.MustRevalidate || cc.NoCache || cc.SMaxAge != cache.Absent {
		return false
	}
	limit := c.StaleIfError
	if cc.StaleIfError != cache.Absent {
		limit = min(limit, cc.StaleIfError)
	}
	return e.Freshness.Staleness(now) <= limit
}

// cacheable reports whether r may use the cache. Besides what
// cache.RequestCacheable checks, a request with a body goes straight
// through, since its body could change the response, and so does one with
// If-Match or If-Unmodified-Since: a 412 for one client must not mark the
// key uncacheable for everyone.
func cacheable(r *http.Request) bool {
	if r.ContentLength != 0 {
		return false
	}
	if _, ok := r.Header["If-Match"]; ok {
		return false
	}
	if _, ok := r.Header["If-Unmodified-Since"]; ok {
		return false
	}
	return cache.RequestCacheable(r.Method, r.Header)
}

// serveCache answers r from the cache, or fetches it and stores the
// response. Concurrent misses for one key are coalesced: one request leads
// and fetches, and the rest wait for it and then look again.
func (h *Handler) serveCache(w http.ResponseWriter, r *http.Request, rt *Route, c *Cache, an *Annotations) {
	primary := cache.PrimaryKey(r)
	if c.BypassHeader != "" {
		if _, ok := r.Header[c.BypassHeader]; ok {
			h.fill(w, r, rt, c, an, fillOpts{primary: primary, store: true, label: cacheBypass})
			return
		}
	}
	// The lock timeout runs on the real clock, as Wait's timer does.
	deadline := time.Now().Add(c.LockTimeout)
	var waitedSince time.Time
	for {
		key, e, ok := c.Store.Lookup(primary, r.Header)
		now := c.now()
		// An entry stored while this request waited answers it even if it
		// is already stale, such as one with max-age=0: it is as new as a
		// fetch of its own would be, and otherwise the waiters would
		// revalidate one after another.
		if ok && (e.Freshness.Fresh(now) || !waitedSince.IsZero() && !e.Freshness.Received.Before(waitedSince)) {
			h.serveEntry(w, r, rt, c, an, e, now, cacheHit)
			return
		}
		if r.Method == http.MethodHead {
			// Only a GET's response is stored, so a HEAD that misses goes
			// to the backend as it is.
			h.fill(w, r, rt, c, an, fillOpts{primary: primary, label: cacheMiss})
			return
		}
		call, role := c.Flight.Join(key)
		switch role {
		case cache.Lead:
			h.fill(w, r, rt, c, an, fillOpts{primary: primary, stale: e, call: call, store: true, label: cacheMiss})
			return
		case cache.Pass:
			h.fill(w, r, rt, c, an, fillOpts{primary: primary, stale: e, store: true, label: cacheMiss})
			return
		}
		if waitedSince.IsZero() {
			waitedSince = now
		}
		switch call.Wait(r.Context(), time.Until(deadline)) {
		case cache.Stored:
			// Look again. A request for a different variant than the one
			// stored joins that variant's key next time round.
			continue
		case cache.Canceled:
			h.fail(w, r, rt, nil, an, r.Context().Err())
			return
		case cache.Failed:
			// The leader just found the backend failing, so do not send it
			// one more request per waiter if a stale entry will do.
			if now := c.now(); e != nil && c.staleOK(e, now) {
				h.serveEntry(w, r, rt, c, an, e, now, cacheStale)
				return
			}
		}
		// Timed out, or the leader had nothing to share. Storing is the
		// leader's job, so this fetch stores nothing.
		h.fill(w, r, rt, c, an, fillOpts{primary: primary, stale: e, label: cacheMiss})
		return
	}
}

// fillOpts says how fill fetches a response.
type fillOpts struct {
	primary string
	stale   *cache.Entry // an entry to revalidate or fall back on, or nil
	call    *cache.Call  // set when leading a Flight
	store   bool         // store the response if it may be
	label   string       // X-Cache for a response from the backend
}

// fill fetches r from a backend, sends the response to the client and,
// with o.store, stores it, buffering the body while it streams. With
// o.stale it revalidates that entry, and serves it if the backend fails
// and stale-if-error allows.
func (h *Handler) fill(w http.ResponseWriter, r *http.Request, rt *Route, c *Cache, an *Annotations, o fillOpts) {
	if o.call != nil {
		// The waiters are released however this ends, a panic included.
		defer o.call.Done(cache.Failed)
	}
	if !h.acquire(w, r, rt, an) {
		return
	}
	defer release(rt)

	up, revalidating := r, false
	if o.store || o.stale != nil {
		ctx := r.Context()
		if o.call != nil {
			// The waiters depend on this fetch, so it goes on if this
			// client leaves.
			ctx = context.WithoutCancel(ctx)
		}
		up = r.Clone(ctx)
		// The client's own conditionals are answered from the entry, not
		// by the backend: a 304 has no body to store.
		delete(up.Header, "If-None-Match")
		delete(up.Header, "If-Modified-Since")
		if e := o.stale; e != nil {
			if v := e.Header.Get("Etag"); v != "" {
				up.Header["If-None-Match"] = []string{v}
				revalidating = true
			}
			if v := e.Header.Get("Last-Modified"); v != "" {
				up.Header["If-Modified-Since"] = []string{v}
				revalidating = true
			}
		}
	}

	requested := c.now()
	f, fe := h.fetch(up, rt, an)
	if fe != nil {
		if now := c.now(); o.stale != nil && c.staleOK(o.stale, now) {
			_, kind := classify(r.Context(), fe.err)
			an.Error = string(kind)
			h.logError(r, slog.LevelWarn, "upstream request failed, serving stale", an, fe.b, 0, kind, fe.err)
			h.serveEntry(w, r, rt, c, an, o.stale, now, cacheStale)
			return
		}
		// Still a miss, like a backend's own 5xx, so the access log shows
		// the cache was consulted.
		an.Cache = o.label
		c.count(o.label)
		w.Header()["X-Cache"] = []string{o.label}
		h.fetchFailed(w, r, rt, an, fe)
		return
	}
	defer f.release()
	received := c.now()
	resp := f.resp

	if e := o.stale; e != nil {
		switch {
		case resp.StatusCode >= 500 && c.staleOK(e, received):
			an.Error = string(kindUpstream)
			h.logError(r, slog.LevelWarn, "upstream returned an error, serving stale", an, f.b, resp.StatusCode, kindUpstream,
				fmt.Errorf("status %d", resp.StatusCode))
			h.serveEntry(w, r, rt, c, an, e, received, cacheStale)
			return
		case resp.StatusCode == http.StatusNotModified && revalidating:
			ne := cache.Revalidated(e, resp.Header, requested, received)
			if o.store {
				res := cache.Uncacheable
				why := cache.Storable(r.Header, ne.Status, ne.Header, ne.CC)
				switch {
				case why == "" && c.Store.Put(ne):
					res = cache.Stored
				case why == "":
					why = "size"
					fallthrough
				default:
					h.notStored(r, an, why)
					c.Store.Remove(e)
				}
				if o.call != nil {
					o.call.Done(res)
				}
			}
			h.serveEntry(w, r, rt, c, an, ne, received, cacheRevalidated)
			return
		}
	}

	var (
		tee    *teeBody
		stored http.Header
		cc     cache.Directives
	)
	if o.store {
		cc = cache.ParseCacheControl(resp.Header["Cache-Control"])
		if why := unstorable(r, resp, cc, c.MaxObject); why == "" {
			// Taken before the route's response rules, which are applied
			// each time the entry is served.
			stored = resp.Header.Clone()
			tee = newTeeBody(resp, c.MaxObject, o.call)
		} else {
			h.notStored(r, an, why)
			// A backend failure says nothing about whether the URL can be
			// cached, or about the stale entry, so only an answer that
			// is not a failure marks the key and replaces the entry.
			res := cache.Failed
			if resp.StatusCode < 500 {
				res = cache.Uncacheable
				if o.stale != nil {
					c.Store.Remove(o.stale)
				}
			}
			if o.call != nil {
				o.call.Done(res) // now, rather than after the body
			}
		}
	}
	an.Cache = o.label
	c.count(o.label)
	resp.Header["X-Cache"] = []string{o.label}
	if tee == nil {
		h.write(w, r, rt, an, &f, resp.Body)
		return
	}
	h.write(w, r, rt, an, &f, tee)
	if !tee.eof && tee.err == nil && !tee.over {
		if o.call == nil {
			return // the client left, and nobody else wants the body
		}
		// The client left, but the waiters want the entry, so read the
		// rest of the body for them.
		if _, err := io.Copy(io.Discard, tee); err != nil && f.out == resilience.Success {
			f.b.Overturn(f.adm)
		}
	}
	if !tee.eof || tee.over {
		return // nothing to store; the deferred Done releases the waiters
	}
	stored["Content-Length"] = []string{strconv.Itoa(len(tee.buf))}
	e := cache.NewEntry(o.primary, r.Header, resp.StatusCode, stored, tee.buf, cc,
		cache.NewFreshness(stored, cc, requested, received))
	res := cache.Uncacheable // larger than the whole store
	if c.Store.Put(e) {
		res = cache.Stored
	}
	if o.call != nil {
		o.call.Done(res)
	}
}

// unstorable returns why resp, the response to r, must not be stored, or ""
// if it may be.
func unstorable(r *http.Request, resp *http.Response, cc cache.Directives, maxObject int64) string {
	if why := cache.Storable(r.Header, resp.StatusCode, resp.Header, cc); why != "" {
		return why
	}
	switch {
	case resp.ContentLength > maxObject:
		return "size"
	case len(resp.Trailer) > 0:
		// Trailers come after the body, and are not stored.
		return "trailers"
	case isEventStream(resp.Header):
		return "event-stream"
	}
	return ""
}

func (h *Handler) notStored(r *http.Request, an *Annotations, why string) {
	if !h.log.Enabled(r.Context(), slog.LevelDebug) {
		return
	}
	h.log.LogAttrs(r.Context(), slog.LevelDebug, "response not stored",
		slog.String("request_id", an.RequestID), slog.String("host", r.Host), slog.String("path", r.URL.Path),
		slog.String("route", an.Route.Name), slog.String("reason", why))
}

// serveEntry answers r from e, applying the route's response rules, which
// are not stored. A 200 whose validators match r's conditionals is answered
// with a 304.
func (h *Handler) serveEntry(w http.ResponseWriter, r *http.Request, rt *Route, c *Cache, an *Annotations, e *cache.Entry, now time.Time, label string) {
	an.Cache = label
	c.count(label)
	notModified := e.Status == http.StatusOK && cache.NotModified(r.Header, e.Header)
	wh := w.Header()
	for k, vv := range e.Header {
		// Entries are shared, but Clone caps its slices, so an append to
		// one of these values copies it rather than writing into e.
		if !notModified || cache.NotModifiedHeader(k) {
			wh[k] = vv
		}
	}
	wh["Age"] = []string{cache.AgeHeader(e.Freshness.Age(now))}
	wh["X-Cache"] = []string{label}
	rt.Response.Apply(wh)
	if notModified {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if _, ok := wh["Content-Type"]; !ok {
		wh["Content-Type"] = nil // stop net/http from sniffing one
	}
	w.WriteHeader(e.Status)
	if r.Method != http.MethodHead {
		w.Write(e.Body)
	}
}

// teeBody passes a response body through while keeping a copy of it for
// the cache, up to limit bytes. Past the limit it drops the copy and
// releases the waiters, which would otherwise wait out the whole transfer
// for nothing.
type teeBody struct {
	r     io.Reader
	limit int64
	call  *cache.Call // nil when not leading
	buf   []byte
	over  bool  // passed limit, so buf is gone
	eof   bool  // the whole body was read
	err   error // a read failed
}

func newTeeBody(resp *http.Response, limit int64, call *cache.Call) *teeBody {
	t := &teeBody{r: resp.Body, limit: limit, call: call}
	if n := resp.ContentLength; n > 0 {
		t.buf = make([]byte, 0, n) // unstorable refused n > limit
	}
	return t
}

func (t *teeBody) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if n > 0 && !t.over {
		if int64(len(t.buf)+n) > t.limit {
			t.over, t.buf = true, nil
			if t.call != nil {
				t.call.Done(cache.Uncacheable)
			}
		} else {
			t.buf = append(t.buf, p[:n]...)
		}
	}
	if err == io.EOF {
		t.eof = true
	} else if err != nil {
		t.err = err
	}
	return n, err
}
