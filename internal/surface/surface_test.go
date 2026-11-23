package surface

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

const fixture = "# Spec\n" +
	"## Reality Engine (RE) Surface\n" +
	"### Info & Health\n" +
	"| Method | Path | CPP | LSP | Scala |\n" +
	"|---|---|---|---|---|\n" +
	"| GET | `/api/health` | ✓ | ✓ | ✓ |\n" +
	"| POST | `/api/machines/:id/process` | ✓ | ✓ | ✓ |\n" +
	"\n" +
	"| GET | `/not/a/table/row` | x |\n" +
	"### Streaming\n" +
	"| Protocol | Path | CPP | LSP | Scala |\n" +
	"|---|---|---|---|---|\n" +
	"| SSE | `/api/engine/stream` | ✓ | ✓ | ✓ |\n" +
	"## Perception Engine (PE) Surface\n" +
	"### Streaming\n" +
	"| Protocol | Path | CPP | LSP | Scala |\n" +
	"| WebSocket | `/ws` | ✓ | ✓ | ✓ |\n" +
	"## K-lines and runtime snapshots\n" +
	"### Routes\n" +
	"| Method | Path | Does |\n" +
	"| POST | `/api/runtime/snapshot` | saves |\n" +
	"## Manager (Visualizer) Surface\n" +
	"| Method | Path | CPP | LSP | Scala |\n" +
	"| GET | `/api/manager/only` | ✓ | ✓ | ✓ |\n"

func TestParseTakesTheEngineTablesOnly(t *testing.T) {
	routes, err := Parse([]byte(fixture))
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, r := range routes {
		ids = append(ids, r.ID+"@"+r.Group)
	}
	want := []string{
		"RE GET /api/health@Info & Health",
		"RE POST /api/machines/:id/process@Info & Health",
		"RE SSE /api/engine/stream@Streaming",
		"PE WS /ws@Streaming",
		"RE-phase2 POST /api/runtime/snapshot@Routes",
	}
	if strings.Join(ids, "\n") != strings.Join(want, "\n") {
		t.Fatalf("Parse() =\n%s\nwant\n%s", strings.Join(ids, "\n"), strings.Join(want, "\n"))
	}
	if routes[0].SpecLine != 6 {
		t.Fatalf("specLine = %d, want 6", routes[0].SpecLine)
	}
}

func TestParseRefusesADuplicateRoute(t *testing.T) {
	dup := fixture + "## Reality Engine (RE) Surface\n### Again\n| Method | Path | CPP | LSP | Scala |\n| GET | `/api/health` | ✓ | ✓ | ✓ |\n"
	if _, err := Parse([]byte(dup)); err == nil || !strings.Contains(err.Error(), "RE GET /api/health") {
		t.Fatalf("Parse(dup) error = %v", err)
	}
}

// The digest must equal Python's json.dumps(sort_keys=True,
// separators=(",", ":")) over the same rows, as RealityEngine_Rust computes it.
// The reference value was computed with CPython for these rows.
func TestDigestMatchesThePythonCanonicalForm(t *testing.T) {
	got, err := Digest([]Route{
		{ID: "RE GET /a", Layer: "RE", Group: "Info & Health — ü 😀", Method: "GET", Path: "/a"},
		{ID: "PE SSE /api/events", Layer: "PE", Group: "Streaming", Method: "SSE", Path: "/api/events"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := "781b02874b0f8c920d4a89c9baacba719778c25b2155dc0ccf328c9698d35515"; got != want {
		t.Fatalf("Digest = %s, want %s", got, want)
	}
}

func TestEmbeddedInventoryIsIntact(t *testing.T) {
	inv, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	counts := Counts(inv.Routes)
	for layer, n := range inv.Counts {
		if counts[layer] != n {
			t.Fatalf("counts[%s] = %d, routes say %d", layer, n, counts[layer])
		}
	}
	if len(inv.Routes) == 0 || inv.Source.Commit == "" {
		t.Fatal("empty inventory")
	}
}

func TestEveryUnimplementedRouteHasAnIssueAndTheStatusFileIsCurrent(t *testing.T) {
	inv := MustLoad()
	statuses, err := Statuses(inv)
	if err != nil {
		t.Fatal(err)
	}
	current, err := os.ReadFile("../../conformance/SURFACE_STATUS.md")
	if err != nil {
		t.Fatal(err)
	}
	if string(current) != StatusMarkdown(inv, statuses) {
		t.Fatal("conformance/SURFACE_STATUS.md is stale: go run ./cmd/refresh-surface")
	}
}

// Classes of a probed route.
const (
	classImplemented = "implemented"
	classUnsupported = "unsupported"
	classBadEnvelope = "bad-envelope"
	classMissing     = "missing"
)

// classify reads one probe's response. Presence is all "implemented" means.
func classify(r Route, status int, body []byte) string {
	switch status {
	case http.StatusNotFound:
		return classMissing
	case http.StatusNotImplemented:
		var got Unsupported
		if json.Unmarshal(body, &got) != nil || got != NewUnsupported(r) {
			return classBadEnvelope
		}
		var keys map[string]any
		if json.Unmarshal(body, &keys) != nil || len(keys) != 3 {
			return classBadEnvelope
		}
		return classUnsupported
	}
	return classImplemented
}

func TestClassify(t *testing.T) {
	r := Route{Method: "GET", Path: "/api/arbitration"}
	good, _ := json.Marshal(NewUnsupported(r))
	for _, tc := range []struct {
		status int
		body   string
		want   string
	}{
		{200, `{}`, classImplemented},
		{400, `{"error":"x"}`, classImplemented},
		{404, `{"error":"x"}`, classMissing},
		{501, string(good), classUnsupported},
		{501, `{"error":"x"}`, classBadEnvelope},
		{501, `{"error":"GET /api/arbitration is not implemented by this runtime","participation":"unsupported","surface":"GET /api/arbitration?x"}`, classBadEnvelope},
		{501, strings.TrimSuffix(string(good), "}") + `,"extra":1}`, classBadEnvelope},
		{501, `not json`, classBadEnvelope},
	} {
		if got := classify(r, tc.status, []byte(tc.body)); got != tc.want {
			t.Errorf("classify(%d, %s) = %s, want %s", tc.status, tc.body, got, tc.want)
		}
	}
}

// probePath fills each :param with a placeholder value.
func probePath(r Route) string {
	parts := strings.Split(r.Path, "/")
	for i, p := range parts {
		if m := param.FindStringSubmatch(p); m != nil {
			parts[i] = "probe-" + m[1]
		}
	}
	return strings.Join(parts, "/")
}

// Every specified route of a layer is probed through a real HTTP server, with
// a client timeout so a stream route can never hang the test. Implemented
// routes are stubbed here; the server package tests their handlers.
func TestEverySpecifiedRouteAnswersAsDeclared(t *testing.T) {
	inv := MustLoad()
	for _, component := range []string{RE, PE} {
		t.Run(component, func(t *testing.T) {
			handlers := map[string]http.Handler{}
			for _, r := range inv.Routes {
				if Implemented[r.ID] && containsLayer(Layers(component), r.Layer) {
					handlers[r.ID] = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						w.WriteHeader(http.StatusOK)
					})
				}
			}
			router, err := NewRouter(inv, Layers(component), Implemented, handlers)
			if err != nil {
				t.Fatal(err)
			}
			srv := httptest.NewServer(router)
			defer srv.Close()
			client := &http.Client{Timeout: 2 * time.Second}

			probed := 0
			for _, r := range inv.Routes {
				if !containsLayer(Layers(component), r.Layer) {
					continue
				}
				req, err := http.NewRequest(r.HTTPMethod(), srv.URL+probePath(r), strings.NewReader("{}"))
				if err != nil {
					t.Fatal(err)
				}
				resp, err := client.Do(req)
				if err != nil {
					t.Fatalf("%s: %v", r.ID, err)
				}
				body, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				want := classUnsupported
				if Implemented[r.ID] {
					want = classImplemented
				}
				if got := classify(r, resp.StatusCode, body); got != want {
					t.Errorf("%s: %s (status %d, body %s), want %s", r.ID, got, resp.StatusCode, body, want)
				}
				if resp.StatusCode == http.StatusNotImplemented && resp.Header.Get("Content-Type") != "application/json" {
					t.Errorf("%s: Content-Type %q", r.ID, resp.Header.Get("Content-Type"))
				}
				probed++
			}
			if probed != inv.Counts[component]+map[string]int{RE: inv.Counts[REPhase2]}[component] {
				t.Fatalf("probed %d routes", probed)
			}
		})
	}
}

func containsLayer(layers []string, layer string) bool {
	for _, l := range layers {
		if l == layer {
			return true
		}
	}
	return false
}

func TestRouterPrefersALiteralSegmentAndSetsPathValues(t *testing.T) {
	inv := Inventory{Routes: []Route{
		{ID: "RE GET /api/machines/:id/checkpoints", Layer: RE, Method: "GET", Path: "/api/machines/:id/checkpoints"},
		{ID: "RE GET /api/machines/json/:name", Layer: RE, Method: "GET", Path: "/api/machines/json/:name"},
		{ID: "RE GET /api/machines/:id", Layer: RE, Method: "GET", Path: "/api/machines/:id"},
		{ID: "RE GET /api/machines/cloneable", Layer: RE, Method: "GET", Path: "/api/machines/cloneable"},
		{ID: "RE GET /", Layer: RE, Method: "GET", Path: "/"},
	}}
	var got string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.PathValue("id") + "|" + r.PathValue("name")
	})
	router, err := NewRouter(inv, []string{RE}, map[string]bool{"RE GET /api/machines/:id": true}, map[string]http.Handler{"RE GET /api/machines/:id": h})
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		"/api/machines/json/checkpoints": "GET /api/machines/json/:name",
		"/api/machines/m1/checkpoints":   "GET /api/machines/:id/checkpoints",
		"/api/machines/cloneable":        "GET /api/machines/cloneable",
		"/api/machines/m1":               "GET /api/machines/:id",
		"/":                              "GET /",
	} {
		r, ok := router.Route("GET", path)
		if !ok || r.Surface() != want {
			t.Errorf("Route(%s) = %v %v, want %s", path, r.Surface(), ok, want)
		}
	}
	for _, path := range []string{"/api/machines/", "/api/machines//checkpoints", "/api", "/api/machines/m1/x/y"} {
		if r, ok := router.Route("GET", path); ok {
			t.Errorf("Route(%s) = %s, want no route", path, r.Surface())
		}
	}
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/machines/m-7", nil))
	if got != "m-7|" {
		t.Fatalf("path values = %q", got)
	}
}

func TestNewRouterChecksHandlersAgainstImplemented(t *testing.T) {
	inv := Inventory{Routes: []Route{{ID: "RE GET /a", Layer: RE, Method: "GET", Path: "/a"}}}
	ok := http.NotFoundHandler()
	for name, tc := range map[string]struct {
		implemented map[string]bool
		handlers    map[string]http.Handler
	}{
		"implemented without handler": {map[string]bool{"RE GET /a": true}, nil},
		"handler not implemented":     {nil, map[string]http.Handler{"RE GET /a": ok}},
		"handler outside the layers":  {map[string]bool{"PE GET /b": true}, map[string]http.Handler{"PE GET /b": ok}},
	} {
		if _, err := NewRouter(inv, []string{RE}, tc.implemented, tc.handlers); err == nil {
			t.Errorf("%s: NewRouter accepted it", name)
		}
	}
}
