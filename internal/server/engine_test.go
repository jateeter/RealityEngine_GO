package server

import (
	"strings"
	"testing"
)

func TestEngineProcessHistoryAndReset(t *testing.T) {
	t.Setenv("VECTOR_DIMENSION", "16")
	h := machinesHandler(t)
	_, out := send(t, h, "POST", "/api/machines", fooMachine) // input region at 10, length 1
	id := out["machine"].(map[string]any)["id"].(string)

	// Machine-space: the vector goes to every machine whole, so the corpus's
	// Zed (one-cell events) fires alongside Foo: a, then b.
	send(t, h, "POST", "/api/engine/process", `{"vector": [1]}`)
	_, p := send(t, h, "POST", "/api/engine/process", `{"vector": [0]}`)
	result := p["result"].(map[string]any)
	if outs := result["outputs"].([]any); len(outs) != 2 || result["inputEvent"].([]any)[0] != 0.0 {
		t.Fatalf("machine-space: %v", p)
	}

	// The audit trail is newest first, enveloped, and ?limit takes the newest.
	_, hist := send(t, h, "GET", "/api/engine/history", "")
	entries := hist["history"].([]any)
	first := entries[0].(map[string]any)
	if len(entries) != 2 || first["type"] != "engine-process" || first["result"].(map[string]any)["inputEvent"].([]any)[0] != 0.0 {
		t.Fatalf("%v", hist)
	}
	if _, hist := send(t, h, "GET", "/api/engine/history?limit=1", ""); len(hist["history"].([]any)) != 1 {
		t.Fatalf("%v", hist)
	}

	// Active events: Foo's a is active again after b's hand-off back.
	_, act := send(t, h, "GET", "/api/engine/active", "")
	found := false
	for _, e := range act["activeEvents"].([]any) {
		e := e.(map[string]any)
		if e["machineId"] == id && e["sequenceId"] == "s" && e["vector"].(map[string]any)["id"] == "a" {
			found = true
		}
	}
	if !found {
		t.Fatalf("a is not listed active: %v", act)
	}

	// Universal: a 16-cell vector is decomposed; Foo reads cell 10.
	cells := make([]string, 16)
	for i := range cells {
		cells[i] = "0"
	}
	cells[10] = "1"
	send(t, h, "POST", "/api/engine/process", `{"vector": [`+strings.Join(cells, ",")+`]}`)
	cells[10] = "0"
	_, u := send(t, h, "POST", "/api/engine/process", `{"vector": [`+strings.Join(cells, ",")+`]}`)
	if outs := u["result"].(map[string]any)["outputs"].([]any); len(outs) != 1 {
		t.Fatalf("universal: %v", u)
	}

	// Reset: Foo back to its initial events; the audit trail survives.
	send(t, h, "POST", "/api/engine/process", `{"vector": [1]}`) // activates b
	if _, r := send(t, h, "POST", "/api/engine/reset?clearAudit=1", ""); r["success"] != true || r["auditCleared"] != true {
		t.Fatalf("%v", r)
	}
	if _, r := send(t, h, "POST", "/api/engine/reset", `{"clearAudit": false`); r["auditCleared"] != false {
		t.Fatalf("a malformed body must not refuse the reset: %v", r)
	}
	if _, p := send(t, h, "POST", "/api/engine/process", `{"vector": [0]}`); len(p["result"].(map[string]any)["outputs"].([]any)) != 0 {
		t.Fatalf("b survived the reset: %v", p)
	}
	if _, hist := send(t, h, "GET", "/api/engine/history", ""); len(hist["history"].([]any)) != 6 {
		t.Fatalf("%v", hist)
	}
}

func TestEngineClockAndStats(t *testing.T) {
	h := machinesHandler(t)
	_, c := send(t, h, "GET", "/api/engine/clock", "")
	if len(c["instance"].(string)) != 36 || c["lamport"] != 0.0 || c["step"] != -1.0 {
		t.Fatalf("%v", c)
	}
	_, s := send(t, h, "GET", "/api/engine/stats", "")
	stats := s["stats"].(map[string]any)
	if stats["machineCount"] != 2.0 || stats["mappingVersion"] != 2.0 {
		t.Fatalf("%v", s)
	}
}
