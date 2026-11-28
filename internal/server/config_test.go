package server

import (
	"testing"
)

func TestEngineConfigIsOneStateWithRuntimeOptions(t *testing.T) {
	h := machinesHandler(t)
	_, cfg := send(t, h, "GET", "/api/engine/config", "")
	controls := cfg["controls"].([]any)
	var names []string
	for _, c := range controls {
		names = append(names, c.(map[string]any)["name"].(string))
	}
	want := []string{"arbitrationRetention", "arbitrationWindow", "clone.maxActive", "historyLimit", "includeActiveRegions",
		"includeMachineResults", "includePerceptualSpace", "klineCapture", "klineFeedbackHops", "klineStoreMaxBytes", "machineJsonSummary", "phaseDetail", "transitionsInhibited"}
	if len(names) != len(want) {
		t.Fatalf("%v", names)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("controls are sorted by name and declared: %v", names)
		}
	}
	if _, c := send(t, h, "GET", "/api/engine/config/historyLimit", ""); c["value"] != 250.0 || c["default"] != 250.0 || c["scope"] != "engine" {
		t.Fatalf("%v", c)
	}

	// A write through either view is visible through the other.
	send(t, h, "PUT", "/api/engine/config/historyLimit", `{"value": 7}`)
	if _, o := send(t, h, "GET", "/api/runtime/options", ""); o["historyLimit"] != 7.0 {
		t.Fatalf("%v", o)
	}
	send(t, h, "PATCH", "/api/runtime/options", `{"includeMachineResults": false}`)
	if _, c := send(t, h, "GET", "/api/engine/config/includeMachineResults", ""); c["value"] != false {
		t.Fatalf("%v", c)
	}
	if _, c := send(t, h, "DELETE", "/api/engine/config/includeMachineResults", ""); c["value"] != true {
		t.Fatalf("DELETE restores the declared default: %v", c)
	}

	// A write that lands nowhere is an error, never 200.
	for path, body := range map[string]string{
		"/api/engine/config/historyLimit":         `{"value": -1}`,
		"/api/engine/config/phaseDetail":          `{"value": "yes"}`,
		"/api/engine/config/arbitrationWindow":    `{"value": 1025}`,
		"/api/engine/config/transitionsInhibited": `{"value": 1}`,
	} {
		if code, out := send(t, h, "PUT", path, body); code != 400 || out["error"] == "" {
			t.Fatalf("%s %s: %d %v", path, body, code, out)
		}
	}
	if code, _ := send(t, h, "PUT", "/api/engine/config/nope", `{"value": 1}`); code != 404 {
		t.Fatalf("unknown control: %d", code)
	}
	if code, _ := send(t, h, "PUT", "/api/engine/config/transitionsInhibited", `{"machine": "nope", "value": true}`); code != 404 {
		t.Fatalf("unknown machine: %d", code)
	}

	// transitionsInhibited is machine-scoped and acts: an inhibited machine
	// accepts the event and passes nothing forward.
	send(t, h, "PUT", "/api/engine/config/transitionsInhibited", `{"machine": "machine-zed-machine", "value": true}`)
	_, c := send(t, h, "GET", "/api/engine/config/transitionsInhibited", "")
	if v := c["value"].(map[string]any); v["machine-zed-machine"] != true || v["machine-alpha"] != false {
		t.Fatalf("%v", c)
	}
}

func TestArbitrationRetentionAndPhaseDetail(t *testing.T) {
	h := machinesHandler(t)
	if code, _ := send(t, h, "GET", "/api/arbitration?step=0", ""); code != 409 {
		t.Fatalf("retention off: %d", code)
	}
	send(t, h, "PUT", "/api/engine/config/arbitrationRetention", `{"value": true}`)
	send(t, h, "PUT", "/api/engine/config/arbitrationWindow", `{"value": 2}`)
	send(t, h, "PUT", "/api/engine/config/phaseDetail", `{"value": true}`)
	for i := 0; i < 3; i++ {
		send(t, h, "POST", "/api/perceive", `{"vector": [1]}`)
	}
	rec := httpGetList(t, h, "/api/arbitration")
	if len(rec) != 2 || rec[0].(map[string]any)["clock"].(map[string]any)["step"] != 1.0 {
		t.Fatalf("the window ending at the newest, oldest first: %v", rec)
	}
	for path, want := range map[string]int{
		"/api/arbitration?step=2": 200, "/api/arbitration?step=0": 410,
		"/api/arbitration?step=9": 404, "/api/arbitration?step=x": 400,
	} {
		if code, _ := sendAny(t, h, "GET", path); code != want {
			t.Fatalf("%s: %d, want %d", path, code, want)
		}
	}
	send(t, h, "PUT", "/api/engine/config/arbitrationRetention", `{"value": false}`)
	if _, legacy := send(t, h, "GET", "/api/arbitration", ""); legacy["records"] == nil {
		t.Fatalf("off is the legacy object: %v", legacy)
	}
}
