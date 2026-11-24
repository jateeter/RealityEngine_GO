package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The PE's GET /api/metrics (RealityEngine_CI docs/PE_METRICS_CONTRACT.md):
// the zero state emits every single-series metric in the contract's order and
// no multi-series counter; a push emits one perception event per active
// source, attributed by kind.
func TestPEMetricsFollowTheContract(t *testing.T) {
	re := httptest.NewServer(machinesHandler(t))
	t.Cleanup(re.Close)
	t.Setenv("RE_URL", re.URL)
	t.Setenv("PE_SOURCE_BOOTSTRAP", "")
	t.Setenv("SEMANTICS_MANIFEST", t.TempDir()+"/absent.json")
	h := mustHandler(t, "PE")

	scrape := func() string {
		t.Helper()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/metrics", nil))
		if rec.Code != 200 || rec.Header().Get("Content-Type") != "text/plain; charset=utf-8" {
			t.Fatalf("%d %s", rec.Code, rec.Header())
		}
		return rec.Body.String()
	}
	zero := scrape()
	var names []string
	for _, line := range strings.Split(strings.TrimSuffix(zero, "\n"), "\n") {
		if strings.HasPrefix(line, "# TYPE ") {
			names = append(names, strings.Fields(line)[2])
		}
	}
	want := "perception_engine_sources_total perception_engine_global_step perception_engine_vector_size " +
		"perception_engine_last_push_ms semantic_manifest_available semantic_manifest_machines " +
		"semantic_audit_buffer_records semantic_dispatch_records_total semantic_dispatch_records_iri_joined_total " +
		"mqtt_bridge_enabled mqtt_bridge_connected"
	if got := strings.Join(names, " "); got != want {
		t.Fatalf("zero-state metrics:\n got %s\nwant %s", got, want)
	}
	if !strings.HasPrefix(zero, "# HELP perception_engine_sources_total Total sensor/test/simulated sources registered.\n"+
		"# TYPE perception_engine_sources_total gauge\nperception_engine_sources_total{runtime=\"go\"} 1\n") {
		t.Fatalf("head:\n%s", zero[:200])
	}
	if !strings.Contains(zero, "semantic_manifest_available{runtime=\"go\"} 0\n") ||
		!strings.HasSuffix(zero, "mqtt_bridge_connected{runtime=\"go\"} 0\n") {
		t.Fatalf("%s", zero)
	}

	if code, _ := send(t, h, "POST", "/api/push", `{"compact": true}`); code != 200 {
		t.Fatalf("push %d", code)
	}
	text := scrape()
	for _, line := range []string{
		"perception_engine_global_step{runtime=\"go\"} 1\n",
		"semantic_audit_buffer_records{runtime=\"go\"} 1\n",
		"# HELP semantic_perception_events_total re:PerceptionEvent records emitted, by originating integration.\n" +
			"# TYPE semantic_perception_events_total counter\n" +
			"semantic_perception_events_total{integration=\"test\",runtime=\"go\"} 1\n",
		"semantic_perception_events_iri_joined_total{integration=\"test\",runtime=\"go\"} 0\n",
	} {
		if !strings.Contains(text, line) {
			t.Fatalf("missing %q in\n%s", line, text)
		}
	}
	if strings.Contains(text, "perception_engine_last_push_ms{runtime=\"go\"} 0\n") {
		t.Fatal("last_push_ms is the step's timestamp after a push")
	}
}
