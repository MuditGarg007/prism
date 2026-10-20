package runtime

import (
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/MuditGarg007/prism/internal/config"
	"github.com/MuditGarg007/prism/internal/observability"
)

// CertLoader loads the certificates of listeners without using them yet,
// and returns a func that swaps them in. The server provides it, so a
// reload re-reads certificate files, which is the usual reason for one.
type CertLoader func(listeners []config.Listener) (swap func(), err error)

// Reloader replaces the current Runtime with one built from the config
// file, on SIGHUP or the admin API's POST /reload.
type Reloader struct {
	path    string
	cur     *Current
	metrics *observability.Metrics
	certs   CertLoader
	log     *slog.Logger
	now     func() time.Time

	// Reloads are serialized: two Builds from the same Prev would carry
	// the same state into two runtimes.
	mu sync.Mutex
}

// NewReloader returns a Reloader of the config file at path into cur. m,
// when set, must be the Metrics that cur's Runtime was built with; certs
// may be nil when nothing serves TLS.
func NewReloader(path string, cur *Current, m *observability.Metrics, certs CertLoader, log *slog.Logger) *Reloader {
	return &Reloader{path: path, cur: cur, metrics: m, certs: certs, log: log, now: time.Now}
}

// Reload loads, validates and builds the config file, then swaps it in.
// Requests already running finish on the Runtime they started with. On any
// error, nothing changes and the current config keeps serving.
func (r *Reloader) Reload() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	start := r.now()
	next, err := r.reload()
	if r.metrics != nil {
		r.metrics.Reloaded(err, r.now())
	}
	if err != nil {
		r.log.Error("reload failed, the previous config keeps serving", "path", r.path, "error", err)
		return err
	}
	r.log.Info("config reloaded",
		"path", r.path,
		"upstreams", len(next.Config.Upstreams),
		"routes", len(next.Config.Routes),
		"took", r.now().Sub(start))
	return nil
}

func (r *Reloader) reload() (*Runtime, error) {
	cfg, err := config.Load(r.path)
	if err != nil {
		return nil, err
	}
	old := r.cur.Load()
	keepRestartOnly(old.Config, cfg, r.log)
	swapCerts := func() {}
	if r.certs != nil {
		if swapCerts, err = r.certs(cfg.Listeners); err != nil {
			return nil, err
		}
	}
	next, err := Build(cfg, r.log, Options{Prev: old, Metrics: r.metrics})
	if err != nil {
		return nil, err
	}
	// Nothing can fail from here on.
	swapCerts()
	r.cur.Swap(next)
	// Right after the swap, to keep the gap without health checks short.
	// The health state is shared, so the old checkers running a moment
	// longer does no harm.
	next.Start()
	old.Retire(next)
	if r.metrics != nil {
		r.metrics.Prune(old.series(), next.series())
	}
	return next, nil
}

// keepRestartOnly copies into next the settings of cur that only a restart
// can change, warning about each one that differs, so that next.Config,
// which GET /config shows, says what is actually running. Listeners keep
// next's certificate paths when nothing else about them changed, since
// certificates are reloaded.
func keepRestartOnly(cur, next *config.Config, log *slog.Logger) {
	warn := func(setting string) {
		log.Warn("setting changed, restart to apply it; the rest of the config is reloaded", "setting", setting)
	}
	if !slices.EqualFunc(cur.Listeners, next.Listeners, sameListener) {
		warn("listeners")
		next.Listeners = cur.Listeners
	}
	if (cur.Admin == nil) != (next.Admin == nil) || cur.Admin != nil && *cur.Admin != *next.Admin {
		warn("admin")
		next.Admin = cur.Admin
	}
	if cur.Shutdown != next.Shutdown {
		warn("shutdown")
		next.Shutdown = cur.Shutdown
	}
	if cur.ReadyPath != next.ReadyPath {
		warn("ready_path")
		next.ReadyPath = cur.ReadyPath
	}
}

// sameListener reports whether a and b differ at most in their certificate
// pairs.
func sameListener(a, b config.Listener) bool {
	return a.Addr == b.Addr && (a.TLS == nil) == (b.TLS == nil) && a.Timeouts == b.Timeouts
}

// series lists the route and backend series that rt records into.
func (rt *Runtime) series() observability.Series {
	s := observability.Series{
		Routes:   map[observability.RouteSeries]bool{},
		Backends: map[observability.BackendSeries]bool{},
	}
	for _, r := range rt.Routes {
		s.Routes[observability.RouteSeries{Route: r.Name, Upstream: r.Pool.Name}] = true
	}
	for name, p := range rt.Pools {
		for _, b := range p.Backends {
			s.Backends[observability.BackendSeries{Upstream: name, Backend: b.String()}] = true
		}
	}
	return s
}
