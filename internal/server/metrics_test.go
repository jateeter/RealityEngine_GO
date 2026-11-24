package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func metricsText(t *testing.T, h http.Handler) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/metrics", nil))
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain; version=0.0.4") {
		t.Fatalf("%d %s", rec.Code, rec.Header())
	}
	return rec.Body.String()
}

func TestMetricsCountStepsAndPhaseLinesAreAbsentWhileOff(t *testing.T) {
	h := machinesHandler(t)
	text := metricsText(t, h)
	for _, want := range []string{
		`ces_machines_total{runtime="go"} 2`,
		`ces_machine_steps_total{runtime="go",machine="Alpha",machine_id="machine-alpha"} 0`,
		`re_step_phase_detail{runtime="go"} 0`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q", want)
		}
	}
	if strings.Contains(text, "re_step_phase_seconds_total") {
		t.Fatal("phase lines are absent, not zero, while phaseDetail is off")
	}
	send(t, h, "PUT", "/api/engine/config/phaseDetail", `{"value": true}`)
	send(t, h, "POST", "/api/perceive", `{"vector": [1]}`)
	send(t, h, "POST", "/api/engine/process", `{"vector": [1]}`) // not counted, as on C++
	send(t, h, "POST", "/api/engine/reset", "")                  // coverage survives a reset
	text = metricsText(t, h)
	for _, want := range []string{
		`ces_machine_steps_total{runtime="go",machine="Zed & Co",machine_id="machine-zed-machine"} 1`,
		`re_step_phase_detail_steps_total{runtime="go"} 1`,
		`re_step_phase_seconds_total{runtime="go",phase="step.compose"}`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in\n%s", want, text)
		}
	}
}
