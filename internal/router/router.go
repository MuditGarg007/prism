// Package router maps a request's host and path to a route.
//
// Matching first picks a host group:
//
//  1. an exact host such as "api.example.com",
//  2. otherwise the most specific wildcard: "*.example.com" matches
//     subdomains at any depth, but not "example.com" itself,
//  3. otherwise the routes with no host.
//
// Inside the group, the longest matching path prefix wins. A path that no
// route in the picked group matches is a miss, with no fallback to a less
// specific group (the same rule as nginx server blocks and Envoy virtual
// hosts).
//
// Prefixes are plain string prefixes, so "/api" also matches "/apiv2". End a
// prefix with "/" to match whole path segments only. The router matches the
// path it is given; cleaning "/a/../b" style paths is the caller's job.
package router

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
)

// Entry is one route to compile. Value is returned when the route matches.
type Entry[T any] struct {
	Host       string // exact name, "*.suffix", or "" for any host
	PathPrefix string // must start with "/"
	Value      T
}

// Table is a compiled, read-only routing table. It is safe for concurrent
// use.
type Table[T any] struct {
	exact     map[string]*group[T]
	wildcards map[string]*group[T] // keyed by suffix with its leading dot, ".example.com"
	anyHost   *group[T]
}

type group[T any] struct {
	routes []route[T] // longest prefix first
}

type route[T any] struct {
	prefix string
	value  T
}

// Compile builds a table from entries. Hosts are matched case-insensitively.
func Compile[T any](entries []Entry[T]) (*Table[T], error) {
	t := &Table[T]{
		exact:     map[string]*group[T]{},
		wildcards: map[string]*group[T]{},
	}
	for i, e := range entries {
		if !strings.HasPrefix(e.PathPrefix, "/") {
			return nil, fmt.Errorf("route %d: path prefix %q must start with \"/\"", i, e.PathPrefix)
		}
		g, err := t.groupFor(strings.ToLower(e.Host))
		if err != nil {
			return nil, fmt.Errorf("route %d: %w", i, err)
		}
		for _, r := range g.routes {
			if r.prefix == e.PathPrefix {
				return nil, fmt.Errorf("route %d: duplicate host %q and path prefix %q", i, e.Host, e.PathPrefix)
			}
		}
		g.routes = append(g.routes, route[T]{prefix: e.PathPrefix, value: e.Value})
	}
	for _, g := range t.groups() {
		slices.SortStableFunc(g.routes, func(a, b route[T]) int {
			return cmp.Compare(len(b.prefix), len(a.prefix))
		})
	}
	return t, nil
}

func (t *Table[T]) groupFor(host string) (*group[T], error) {
	if host == "" {
		if t.anyHost == nil {
			t.anyHost = &group[T]{}
		}
		return t.anyHost, nil
	}
	m, key := t.exact, host
	if strings.HasPrefix(host, "*.") {
		m, key = t.wildcards, host[1:]
	}
	if strings.Contains(key, "*") || key == "." {
		return nil, fmt.Errorf("invalid host %q: \"*\" is only allowed as a leading \"*.\"", host)
	}
	g, ok := m[key]
	if !ok {
		g = &group[T]{}
		m[key] = g
	}
	return g, nil
}

func (t *Table[T]) groups() []*group[T] {
	var gs []*group[T]
	for _, g := range t.exact {
		gs = append(gs, g)
	}
	for _, g := range t.wildcards {
		gs = append(gs, g)
	}
	if t.anyHost != nil {
		gs = append(gs, t.anyHost)
	}
	return gs
}

// Match returns the value of the route for host and path. host may be a raw
// Host header value; its port and case are ignored.
func (t *Table[T]) Match(host, path string) (T, bool) {
	if g := t.lookup(normalizeHost(host)); g != nil {
		for _, r := range g.routes {
			if strings.HasPrefix(path, r.prefix) {
				return r.value, true
			}
		}
	}
	var zero T
	return zero, false
}

func (t *Table[T]) lookup(host string) *group[T] {
	if g, ok := t.exact[host]; ok {
		return g
	}
	if len(t.wildcards) > 0 {
		// Try suffixes from the leftmost dot, so the longest one wins.
		// i > 0 keeps ".example.com" from matching with an empty label.
		for i := 1; i < len(host); i++ {
			if host[i] != '.' {
				continue
			}
			if g, ok := t.wildcards[host[i:]]; ok {
				return g
			}
		}
	}
	return t.anyHost
}

// normalizeHost lowercases a Host header value and strips its port, the
// brackets around an IPv6 literal, and a trailing dot.
func normalizeHost(h string) string {
	if i := strings.LastIndexByte(h, ':'); i >= 0 && strings.IndexByte(h[i:], ']') < 0 {
		// "name:port" or "[v6]:port". A bare "::1" has no port to strip.
		if strings.HasPrefix(h, "[") || strings.Count(h, ":") == 1 {
			h = h[:i]
		}
	}
	if len(h) > 1 && h[0] == '[' && h[len(h)-1] == ']' {
		h = h[1 : len(h)-1]
	}
	h = strings.TrimSuffix(h, ".")
	return strings.ToLower(h)
}

// StripPrefix removes prefix from path and keeps the result rooted at "/",
// so "/v1/users" with prefix "/v1/" becomes "/users". path must start with
// prefix, as it does after a Match.
func StripPrefix(path, prefix string) string {
	rest := strings.TrimPrefix(path, prefix)
	if !strings.HasPrefix(rest, "/") {
		return "/" + rest
	}
	return rest
}
