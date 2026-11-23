package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const storedSequence = `{"id": "seq-1", "name": "probe", "events": [
  {"id": "a", "isInitial": true, "elements": [{"value": 1, "comparatorType": "EQUALS"}], "nextEventIds": ["b"]},
  {"id": "b", "elements": [{"value": 0}], "outputEvents": [{"vector": [1, "x"]}]}]}`

func TestSequenceStore(t *testing.T) {
	h := machinesHandler(t)
	code, created := send(t, h, "POST", "/api/sequences", storedSequence)
	seq, _ := created["sequence"].(map[string]any)
	if code != 200 || created["success"] != true || seq["id"] != "seq-1" {
		t.Fatalf("%d %v", code, created)
	}
	events := seq["events"].([]any)
	a, b := events[0].(map[string]any), events[1].(map[string]any)
	if a["elements"].([]any)[0].(map[string]any)["comparatorType"] != "equals" || a["matchAlgorithm"] != "gte" {
		t.Fatalf("a: %v", a)
	}
	out := b["outputEvents"].([]any)[0].(map[string]any)
	if !strings.HasPrefix(out["id"].(string), "output-") || out["vector"].([]any)[1] != 0.0 {
		t.Fatalf("output: %v", out)
	}

	if _, list := send(t, h, "GET", "/api/sequences", ""); len(list["sequences"].([]any)) != 1 {
		t.Fatalf("%v", list)
	}
	if code, got := send(t, h, "GET", "/api/sequences/seq-1", ""); code != 200 || got["sequence"].(map[string]any)["name"] != "probe" {
		t.Fatalf("%d %v", code, got)
	}
	code, added := send(t, h, "POST", "/api/sequences/seq-1/vectors", `{"elements": [{"value": 1}]}`)
	if v := added["vector"].(map[string]any); code != 200 || !strings.HasPrefix(v["id"].(string), "vector-") {
		t.Fatalf("%d %v", code, added)
	}
	if code, out := send(t, h, "POST", "/api/sequences/seq-1/reset", ""); code != 200 || out["success"] != true {
		t.Fatalf("%d %v", code, out)
	}
	if code, out := send(t, h, "POST", "/api/sequences/persist", ""); code != 200 || out["success"] != true {
		t.Fatalf("%d %v", code, out)
	}
	if code, out := send(t, h, "DELETE", "/api/sequences/seq-1", ""); code != 200 || out["success"] != true {
		t.Fatalf("%d %v", code, out)
	}
	for _, c := range []struct{ method, path string }{
		{"GET", "/api/sequences/seq-1"}, {"DELETE", "/api/sequences/seq-1"},
		{"POST", "/api/sequences/seq-1/reset"}, {"POST", "/api/sequences/seq-1/vectors"},
	} {
		if code, out := send(t, h, c.method, c.path, ""); code != 404 || out["error"] != "Sequence not found" {
			t.Fatalf("%s %s: %d %v", c.method, c.path, code, out)
		}
	}
}

func TestSequenceRefusals(t *testing.T) {
	h := machinesHandler(t)
	code, out := send(t, h, "POST", "/api/sequences", `{"events": [{"id": "a"}]}`)
	errs, _ := out["error"].([]any)
	if code != 400 || len(errs) != 2 {
		t.Fatalf("%d %v", code, out)
	}
	code, out = send(t, h, "POST", "/api/sequences", `{"events": [{"elements": [{"value": 1, "comparatorType": "fuzzy"}]}]}`)
	if code != 500 || out["error"] != "Unknown comparator type: fuzzy" {
		t.Fatalf("%d %v", code, out)
	}
	if code, _ := send(t, h, "POST", "/api/sequences", `{`); code != 400 {
		t.Fatalf("bad JSON: %d", code)
	}
}

func TestSemanticBuses(t *testing.T) {
	registry := filepath.Join(t.TempDir(), "registry.json")
	if err := os.WriteFile(registry, []byte(`{"version": 1.0, "semanticBuses": [{"id": "bus-a", "width": 4}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SEMANTIC_BUS_REGISTRY", registry)
	h := machinesHandler(t)
	_, _, raw := get(t, h, "/api/buses/semantic")
	if !strings.Contains(raw, `"version":1.0`) {
		t.Fatalf("numbers must pass through as written: %s", raw)
	}
	if code, out := send(t, h, "GET", "/api/buses/semantic/bus-a", ""); code != 200 || out["bus"].(map[string]any)["width"] != 4.0 {
		t.Fatalf("%d %v", code, out)
	}
	if code, out := send(t, h, "GET", "/api/buses/semantic/nope", ""); code != 404 || out["error"] != "Semantic bus not found" {
		t.Fatalf("%d %v", code, out)
	}
	t.Setenv("SEMANTIC_BUS_REGISTRY", filepath.Join(t.TempDir(), "absent.json"))
	h = machinesHandler(t)
	if code, out := send(t, h, "GET", "/api/buses/semantic", ""); code != 404 || !strings.HasPrefix(out["error"].(string), "semantic bus registry unavailable: cannot open ") {
		t.Fatalf("%d %v", code, out)
	}
}
