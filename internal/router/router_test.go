package router

import (
	"fmt"
	"strings"
	"testing"
)

// testEntries uses each entry's index as its value.
var testEntries = []Entry[int]{
	0: {Host: "api.example.com", PathPrefix: "/v1/"},
	1: {Host: "api.example.com", PathPrefix: "/v1/admin/"},
	2: {Host: "api.example.com", PathPrefix: "/"},
	3: {Host: "*.example.com", PathPrefix: "/"},
	4: {Host: "*.eu.example.com", PathPrefix: "/"},
	5: {Host: "*.example.com", PathPrefix: "/static/"},
	6: {Host: "", PathPrefix: "/"},
	7: {Host: "", PathPrefix: "/api"},
	8: {Host: "Docs.Example.COM", PathPrefix: "/guide/"},
}

func compileTest(t testing.TB) *Table[int] {
	t.Helper()
	entries := make([]Entry[int], len(testEntries))
	for i, e := range testEntries {
		e.Value = i
		entries[i] = e
	}
	tbl, err := Compile(entries)
	if err != nil {
		t.Fatal(err)
	}
	return tbl
}

func TestMatch(t *testing.T) {
	tbl := compileTest(t)
	tests := []struct {
		host, path string
		want       int // -1 means no match
	}{
		// Exact host, longest prefix first.
		{"api.example.com", "/v1/users", 0},
		{"api.example.com", "/v1/admin/keys", 1},
		{"api.example.com", "/v1", 2},
		{"api.example.com", "/", 2},
		// Host normalization.
		{"API.Example.com", "/v1/x", 0},
		{"api.example.com:8443", "/v1/x", 0},
		{"api.example.com.", "/v1/x", 0},
		// Wildcards: any depth, most specific suffix wins, never the apex.
		{"www.example.com", "/", 3},
		{"a.b.example.com", "/", 3},
		{"www.example.com", "/static/app.js", 5},
		{"x.eu.example.com", "/", 4},
		{"x.eu.example.com", "/static/app.js", 4},
		{"eu.example.com", "/", 3},
		{"example.com", "/", 6},
		{".example.com", "/", 6},
		// Any host.
		{"other.org", "/", 6},
		{"", "/", 6},
		{"other.org", "/api/x", 7},
		{"other.org", "/apiv2", 7}, // plain string prefix
		{"[::1]:8080", "/", 6},
		{"10.0.0.1:80", "/api", 7},
		// No fallback from a picked group: docs has only /guide/.
		{"docs.example.com", "/guide/intro", 8},
		{"docs.example.com", "/", -1},
	}
	for _, tt := range tests {
		got, ok := tbl.Match(tt.host, tt.path)
		if !ok {
			got = -1
		}
		if got != tt.want {
			t.Errorf("Match(%q, %q) = %d, want %d", tt.host, tt.path, got, tt.want)
		}
	}
}

func TestMatchEmptyTable(t *testing.T) {
	tbl, err := Compile[string](nil)
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := tbl.Match("example.com", "/"); ok {
		t.Errorf("empty table matched %q", v)
	}
}

func TestCompileErrors(t *testing.T) {
	tests := []struct {
		name    string
		entries []Entry[int]
		want    string
	}{
		{"relative prefix", []Entry[int]{{PathPrefix: "api"}}, `must start with "/"`},
		{"empty prefix", []Entry[int]{{PathPrefix: ""}}, `must start with "/"`},
		{"wildcard in middle", []Entry[int]{{Host: "a.*.com", PathPrefix: "/"}}, `"*" is only allowed`},
		{"bare star", []Entry[int]{{Host: "*", PathPrefix: "/"}}, `"*" is only allowed`},
		{"star dot", []Entry[int]{{Host: "*.", PathPrefix: "/"}}, `"*" is only allowed`},
		{"duplicate", []Entry[int]{{Host: "a.com", PathPrefix: "/x"}, {Host: "A.com", PathPrefix: "/x"}}, "route 1: duplicate"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Compile(tt.entries)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("got %v, want an error containing %q", err, tt.want)
			}
		})
	}
}

func TestNormalizeHost(t *testing.T) {
	tests := map[string]string{
		"example.com":       "example.com",
		"EXAMPLE.com":       "example.com",
		"example.com:80":    "example.com",
		"example.com.":      "example.com",
		"example.com.:443":  "example.com",
		"[::1]:8080":        "::1",
		"[::1]":             "::1",
		"::1":               "::1",
		"[2001:DB8::1]:443": "2001:db8::1",
		"":                  "",
	}
	for in, want := range tests {
		if got := normalizeHost(in); got != want {
			t.Errorf("normalizeHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStripPrefix(t *testing.T) {
	tests := []struct{ path, prefix, want string }{
		{"/v1/users", "/v1/", "/users"},
		{"/v1/users", "/v1", "/users"},
		{"/v1/", "/v1/", "/"},
		{"/v1", "/v1", "/"},
		{"/x", "/", "/x"},
		{"/apiv2", "/api", "/v2"},
	}
	for _, tt := range tests {
		if got := StripPrefix(tt.path, tt.prefix); got != tt.want {
			t.Errorf("StripPrefix(%q, %q) = %q, want %q", tt.path, tt.prefix, got, tt.want)
		}
	}
}

func TestMatchDoesNotAllocate(t *testing.T) {
	tbl := compileTest(t)
	allocs := testing.AllocsPerRun(100, func() {
		tbl.Match("x.eu.example.com:443", "/static/app.js")
		tbl.Match("api.example.com", "/v1/admin/keys")
		tbl.Match("other.org", "/api/x")
	})
	if allocs != 0 {
		t.Errorf("Match allocated %v times per run, want 0", allocs)
	}
}

// refMatch is a brute-force version of Match used as the fuzzing oracle.
func refMatch(entries []Entry[int], host, path string) int {
	host = normalizeHost(host)
	hostKind := func(e Entry[int]) (rank, specificity int) {
		h := strings.ToLower(e.Host)
		switch {
		case h == "":
			return 1, 0
		case strings.HasPrefix(h, "*."):
			if suffix := h[1:]; len(host) > len(suffix) && strings.HasSuffix(host, suffix) {
				return 2, len(suffix)
			}
		case h == host:
			return 3, 0
		}
		return 0, 0
	}
	// Pick the best host group, then the longest prefix inside it.
	bestRank, bestSpec := 0, 0
	for _, e := range entries {
		r, s := hostKind(e)
		if r > bestRank || (r == bestRank && s > bestSpec) {
			bestRank, bestSpec = r, s
		}
	}
	best, bestLen := -1, -1
	for i, e := range entries {
		r, s := hostKind(e)
		if r == bestRank && s == bestSpec && r > 0 && strings.HasPrefix(path, e.PathPrefix) && len(e.PathPrefix) > bestLen {
			best, bestLen = i, len(e.PathPrefix)
		}
	}
	return best
}

func FuzzMatch(f *testing.F) {
	f.Add("api.example.com", "/v1/users")
	f.Add("x.eu.example.com:443", "/static/")
	f.Add("[::1]:80", "/api")
	f.Add("docs.example.com", "/")
	f.Add("..example.com", "/")
	f.Add("a.example.com.", "/v1/admin/x")
	tbl := compileTest(f)
	f.Fuzz(func(t *testing.T, host, path string) {
		got, ok := tbl.Match(host, path)
		if !ok {
			got = -1
		}
		if want := refMatch(testEntries, host, path); got != want {
			t.Errorf("Match(%q, %q) = %d, reference says %d", host, path, got, want)
		}
	})
}

func BenchmarkMatch(b *testing.B) {
	var entries []Entry[int]
	for i := range 20 {
		host := fmt.Sprintf("svc%d.example.com", i)
		for j := range 10 {
			entries = append(entries, Entry[int]{Host: host, PathPrefix: fmt.Sprintf("/api/v%d/", j)})
		}
	}
	entries = append(entries,
		Entry[int]{Host: "*.example.com", PathPrefix: "/"},
		Entry[int]{PathPrefix: "/"},
	)
	tbl, err := Compile(entries)
	if err != nil {
		b.Fatal(err)
	}
	cases := []struct{ name, host, path string }{
		{"exact", "svc7.example.com:443", "/api/v9/users/42"},
		{"wildcard", "a.b.example.com", "/index.html"},
		{"any", "other.org", "/"},
	}
	for _, c := range cases {
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				tbl.Match(c.host, c.path)
			}
		})
	}
}
