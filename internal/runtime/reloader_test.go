package runtime

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"

	"github.com/MuditGarg007/prism/internal/config"
	"github.com/MuditGarg007/prism/internal/observability"
)

// reloadFixture is a config file, the runtime it was built into, and a
// Reloader of it whose certificate loader the test controls.
type reloadFixture struct {
	t       *testing.T
	path    string
	cur     *Current
	metrics *observability.Metrics
	r       *Reloader
	logs    *bytes.Buffer

	certErr   error             // what the certificate loader returns
	certsSeen []config.Listener // what it was last given
	swaps     int               // how many of its swaps ran
}

func newReloadFixture(t *testing.T, yaml string) *reloadFixture {
	f := &reloadFixture{t: t, path: filepath.Join(t.TempDir(), "prism.yaml"), logs: &bytes.Buffer{}}
	f.write(yaml)
	cfg, err := config.Load(f.path)
	if err != nil {
		t.Fatal(err)
	}
	f.metrics = observability.NewMetrics()
	rt, err := Build(cfg, discard, Options{Metrics: f.metrics})
	if err != nil {
		t.Fatal(err)
	}
	f.cur = NewCurrent(rt)
	t.Cleanup(func() { f.cur.Load().Close() })
	certs := func(ls []config.Listener) (func(), error) {
		f.certsSeen = ls
		if f.certErr != nil {
			return nil, f.certErr
		}
		return func() { f.swaps++ }, nil
	}
	log := slog.New(slog.NewTextHandler(f.logs, nil))
	f.r = NewReloader(f.path, f.cur, f.metrics, certs, log)
	f.r.now = func() time.Time { return time.Unix(1000, 0) }
	return f
}

func (f *reloadFixture) write(yaml string) {
	f.t.Helper()
	if err := os.WriteFile(f.path, []byte(yaml), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

// get sends a request through the current runtime and returns the body.
func (f *reloadFixture) get() string {
	f.t.Helper()
	rec := httptest.NewRecorder()
	f.cur.ServeHTTP(rec, httptest.NewRequest("GET", "http://a.test/x", nil))
	body, _ := io.ReadAll(rec.Body)
	return string(body)
}

// value returns the sample of the metric name whose labels include every
// "name=value" pair in labels.
func (f *reloadFixture) value(name string, labels ...string) float64 {
	f.t.Helper()
	mfs, err := f.metrics.Registry.Gather()
	if err != nil {
		f.t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
	metrics:
		for _, m := range mf.GetMetric() {
			for _, want := range labels {
				if !slices.ContainsFunc(m.GetLabel(), func(l *dto.LabelPair) bool { return l.GetName()+"="+l.GetValue() == want }) {
					continue metrics
				}
			}
			if c := m.GetCounter(); c != nil {
				return c.GetValue()
			}
			return m.GetGauge().GetValue()
		}
	}
	f.t.Fatalf("no %s%v", name, labels)
	return 0
}

// count returns how many series of the metric name there are.
func (f *reloadFixture) count(name string) int {
	f.t.Helper()
	n, err := testutil.GatherAndCount(f.metrics.Registry, name)
	if err != nil {
		f.t.Fatal(err)
	}
	return n
}

// reloadConfig is a config sending everything to backend, with top as its
// top-level settings besides the upstreams and routes.
func reloadConfig(top, backend string) string {
	return fmt.Sprintf(`%s
upstreams:
  web: { backends: [{ url: %q }] }
routes:
  - { match: { path_prefix: / }, upstream: web }
`, top, backend)
}

const reloadTop = `
listeners: [{ addr: ":8080" }]
admin: { listen: "127.0.0.1:9901" }
`

// TestReloader checks that a reload swaps in the new config with its
// certificates, keeps the settings that need a restart, and that every kind
// of failure leaves the serving config, certificates and metrics alone.
func TestReloader(t *testing.T) {
	a, b := namedBackend(t, "a"), namedBackend(t, "b")
	f := newReloadFixture(t, reloadConfig(reloadTop, a.URL))
	first := f.cur.Load()
	if got := f.get(); !strings.HasPrefix(got, "a ") {
		t.Fatalf("before reload: %q", got)
	}

	f.write(reloadConfig(`
listeners: [{ addr: ":9090" }]
admin: { listen: "127.0.0.1:9902" }
ready_path: /ready
shutdown: { timeout: 3s }
`, b.URL))
	if err := f.r.Reload(); err != nil {
		t.Fatal(err)
	}
	rt := f.cur.Load()
	if rt == first || !strings.HasPrefix(f.get(), "b ") {
		t.Fatalf("not swapped: %q", f.get())
	}
	c := rt.Config
	if c.Listeners[0].Addr != ":8080" || c.Admin.Listen != "127.0.0.1:9901" || c.ReadyPath != "" || c.Shutdown != first.Config.Shutdown {
		t.Errorf("a restart-only setting was applied: %+v %+v %q %+v", c.Listeners, c.Admin, c.ReadyPath, c.Shutdown)
	}
	for _, setting := range []string{"listeners", "admin", "ready_path", "shutdown"} {
		if !strings.Contains(f.logs.String(), "setting="+setting) {
			t.Errorf("no restart warning for %s", setting)
		}
	}
	if f.swaps != 1 || len(f.certsSeen) != 1 || f.certsSeen[0].Addr != ":8080" {
		t.Errorf("certificates: %d swaps, loaded for %+v", f.swaps, f.certsSeen)
	}
	if f.value("prism_config_last_reload_successful") != 1 || f.value("prism_config_last_reload_success_timestamp_seconds") != 1000 {
		t.Error("reload metrics not set")
	}

	// Every failure keeps rt, its certificates and its metrics.
	dir := filepath.Dir(f.path)
	if err := os.WriteFile(filepath.Join(dir, "bad.pem"), []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	failures := []struct {
		name    string
		yaml    string
		certErr error
	}{
		{"unreadable", "listeners: [", nil},
		{"invalid", reloadConfig(reloadTop, "ftp://x"), nil},
		{"certificates", reloadConfig(reloadTop, a.URL), errors.New("bad certificate")},
		{"build", reloadTop + `
upstreams:
  web: { backends: [{ url: "` + a.URL + `" }] }
  zz: { backends: [{ url: "https://127.0.0.1:1", tls: { ca_file: bad.pem } }] }
routes:
  - { match: { path_prefix: / }, upstream: web }
  - { match: { path_prefix: /tls/ }, upstream: zz }
`, nil},
	}
	for _, tt := range failures {
		f.logs.Reset()
		f.write(tt.yaml)
		f.certErr = tt.certErr
		if err := f.r.Reload(); err == nil {
			t.Errorf("%s: reload succeeded", tt.name)
		}
		if f.cur.Load() != rt || !strings.HasPrefix(f.get(), "b ") {
			t.Errorf("%s: the serving runtime changed", tt.name)
		}
		if f.swaps != 1 {
			t.Errorf("%s: certificates swapped", tt.name)
		}
		if f.value("prism_config_last_reload_successful") != 0 {
			t.Errorf("%s: last reload reported successful", tt.name)
		}
		if !strings.Contains(f.logs.String(), "reload failed") {
			t.Errorf("%s: failure not logged: %s", tt.name, f.logs)
		}
	}
	if n := f.value("prism_config_reloads_total", "result=failure"); n != float64(len(failures)) {
		t.Errorf("%v failures counted, want %d", n, len(failures))
	}
}

// TestReloaderPrunesMetrics checks that the series of a removed route and
// backend are gone after a reload.
func TestReloaderPrunesMetrics(t *testing.T) {
	a, b := namedBackend(t, "a"), namedBackend(t, "b")
	f := newReloadFixture(t, reloadConfig(reloadTop, a.URL))
	f.get()
	f.write(strings.Replace(reloadConfig(reloadTop, b.URL), "path_prefix: /", "path_prefix: /x", 1))
	if err := f.r.Reload(); err != nil {
		t.Fatal(err)
	}
	f.get()
	// The new route's 6 classes and the unmatched ones'. Requests are
	// counted by the access log, which this test doesn't run, but the
	// backend latency is observed by the proxy.
	if n := f.count("prism_requests_total"); n != 12 {
		t.Errorf("%d prism_requests_total series, want 12", n)
	}
	if f.value("prism_requests_total", "route=/x", "code=2xx") != 0 {
		t.Error("the new route's series are missing")
	}
	if n := f.count("prism_upstream_response_seconds"); n != 1 {
		t.Errorf("%d backend latency series, want only b's", n)
	}
}

// TestReloaderConcurrent checks that reloads racing each other are
// serialized, each one building from the runtime the previous one swapped
// in.
func TestReloaderConcurrent(t *testing.T) {
	a := namedBackend(t, "a")
	f := newReloadFixture(t, reloadConfig(reloadTop, a.URL))
	f.r.certs = nil
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if err := f.r.Reload(); err != nil {
				t.Error(err)
			}
			f.get()
		})
	}
	wg.Wait()
	if n := f.value("prism_config_reloads_total", "result=success"); n != 8 {
		t.Errorf("%v reloads counted, want 8", n)
	}
}
