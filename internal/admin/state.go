package admin

import (
	"maps"
	"net/http"
	"slices"

	"github.com/MuditGarg007/prism/internal/config"
)

// Routes is the body of GET /routes.
type Routes struct {
	Routes []RouteInfo `json:"routes"`
}

// RouteInfo describes one route, in the order the config lists them. The
// rest of its settings are in GET /config.
type RouteInfo struct {
	Name       string `json:"name"`
	Host       string `json:"host"` // "" matches any host
	PathPrefix string `json:"path_prefix"`
	Upstream   string `json:"upstream"`
	Cache      bool   `json:"cache"`
	// Concurrency is set only for routes with max_concurrency.
	Concurrency *Concurrency   `json:"concurrency,omitempty"`
	Access      *config.Access `json:"access,omitempty"`
	// Auth lists the credentials the route takes: "basic", "api_keys" or
	// both. Users and keys are in GET /config, without their hashes.
	Auth      []string       `json:"auth,omitempty"`
	Canary    *CanaryInfo    `json:"canary,omitempty"`
	Shadow    *ShadowInfo    `json:"shadow,omitempty"`
	WebSocket *WebSocketInfo `json:"websocket,omitempty"`
}

type CanaryInfo struct {
	Upstream string  `json:"upstream"`
	Percent  float64 `json:"percent"`
	Header   string  `json:"header,omitempty"`
	Cookie   string  `json:"cookie,omitempty"`
}

// ShadowInfo is a route's shadow, with its copies in flight against its
// max_concurrency.
type ShadowInfo struct {
	Upstream string  `json:"upstream"`
	Percent  float64 `json:"percent"`
	InFlight int     `json:"in_flight"`
	Limit    int     `json:"limit"`
}

type WebSocketInfo struct {
	IdleTimeout string `json:"idle_timeout"`
}

// Concurrency is a route's live use of its max_concurrency.
type Concurrency struct {
	InUse  int `json:"in_use"`
	Limit  int `json:"limit"`
	Queued int `json:"queued"` // requests waiting for a slot
}

func (h *Handler) routes(w http.ResponseWriter, r *http.Request) {
	rt := h.cur.Load()
	out := Routes{Routes: make([]RouteInfo, len(rt.Routes))}
	for i, route := range rt.Routes {
		// Build makes rt.Routes from rt.Config.Routes in the same order.
		m := rt.Config.Routes[i].Match
		info := RouteInfo{
			Name:       route.Name,
			Host:       m.Host,
			PathPrefix: m.PathPrefix,
			Upstream:   route.Pool.Name,
			Cache:      route.Cache != nil,
		}
		if s := route.Concurrency; s != nil {
			info.Concurrency = &Concurrency{InUse: s.InUse(), Limit: s.Cap(), Queued: s.Waiting()}
		}
		cr := &rt.Config.Routes[i]
		info.Access = cr.Access
		if a := cr.Auth; a != nil {
			if a.Basic != nil {
				info.Auth = append(info.Auth, "basic")
			}
			if a.APIKeys != nil {
				info.Auth = append(info.Auth, "api_keys")
			}
		}
		if c := cr.Canary; c != nil {
			info.Canary = &CanaryInfo{Upstream: c.Upstream, Percent: c.Percent, Header: c.Header, Cookie: c.Cookie}
		}
		if sh := cr.Shadow; sh != nil {
			slots := route.Shadow.Slots
			info.Shadow = &ShadowInfo{Upstream: sh.Upstream, Percent: sh.Percent, InFlight: slots.InUse(), Limit: slots.Cap()}
		}
		if ws := cr.WebSocket; ws != nil {
			info.WebSocket = &WebSocketInfo{IdleTimeout: ws.IdleTimeout.String()}
		}
		out.Routes[i] = info
	}
	writeJSON(w, http.StatusOK, out)
}

// Upstreams is the body of GET /upstreams.
type Upstreams struct {
	Upstreams []UpstreamInfo `json:"upstreams"`
}

// UpstreamInfo is the live state of one upstream, sorted by name.
type UpstreamInfo struct {
	Name     string `json:"name"`
	Balancer string `json:"balancer"`
	// Available counts the backends the balancer may pick.
	Available int           `json:"available"`
	Backends  []BackendInfo `json:"backends"`
}

// BackendInfo is the live state of one backend, in config order.
type BackendInfo struct {
	URL      string `json:"url"` // without its password
	Weight   int    `json:"weight"`
	Inflight int64  `json:"inflight"`
	Healthy  bool   `json:"healthy"`
	Ejected  bool   `json:"ejected"`
	// Breaker is "closed", "half_open" or "open", or absent when the
	// upstream has no circuit breaker.
	Breaker string `json:"breaker,omitempty"`
	// Available is healthy, not ejected and breaker not open.
	Available bool `json:"available"`
}

func (h *Handler) upstreams(w http.ResponseWriter, r *http.Request) {
	rt := h.cur.Load()
	out := Upstreams{Upstreams: make([]UpstreamInfo, 0, len(rt.Pools))}
	for _, name := range slices.Sorted(maps.Keys(rt.Pools)) {
		p := rt.Pools[name]
		info := UpstreamInfo{
			Name:     name,
			Balancer: rt.Config.Upstreams[name].Balancer,
			Backends: make([]BackendInfo, len(p.Backends)),
		}
		for i, b := range p.Backends {
			bi := BackendInfo{
				URL:       b.String(),
				Weight:    b.Weight,
				Inflight:  b.Inflight(),
				Healthy:   b.Healthy(),
				Ejected:   b.Ejected(),
				Available: b.Available(),
			}
			if br := b.Breaker(); br != nil {
				bi.Breaker = br.State().String()
			}
			if bi.Available {
				info.Available++
			}
			info.Backends[i] = bi
		}
		out.Upstreams = append(out.Upstreams, info)
	}
	writeJSON(w, http.StatusOK, out)
}
