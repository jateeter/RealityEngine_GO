package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fixtureMachine = `{"version": "1.0.0", "machine": {"name": "Zed & Co", "description": "d",
  "arbiterRule": "passthrough", "metadata": {"domain": "health"},
  "inputSequences": [{"id": "in", "events": [[1, 0]]}],
  "perceptualMapping": {"input": {"offset": 4, "length": 2}, "output": {"offset": 6, "length": 1}, "bitsPerElement": 3},
  "sequences": [
    {"id": "s2", "name": "beta", "events": [{"id": "x", "isInitial": true, "elements": [{"value": 1}]}]},
    {"id": "s1", "name": "alpha", "schemaVersion": "2", "events": [
      {"id": "b", "elements": [{"value": 0, "comparatorType": "Equals", "threshold": 0.25}], "outputEvents": [{"id": "o", "vector": [1]}]},
      {"id": "a", "isInitial": true, "elements": [{"value": 1}], "nextEventIds": ["b"]}]}]}}`

func machinesHandler(t *testing.T) http.Handler {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "domains", "health"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(rel, body string) {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("domains/health/Zed_Machine.json", fixtureMachine)
	write("Alpha.json", `{"name": "Alpha", "sequences": []}`)
	t.Setenv("MACHINES_DIR", dir)
	t.Setenv("SEMANTICS_MANIFEST", filepath.Join(dir, "no-manifest.json"))
	return mustHandler(t, "RE")
}

func get(t *testing.T, h http.Handler, path string) (int, map[string]any, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	raw := rec.Body.String()
	var body map[string]any
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("%s: %v: %s", path, err, raw)
	}
	return rec.Code, body, raw
}

func TestMachineList(t *testing.T) {
	h := machinesHandler(t)
	code, body, raw := get(t, h, "/api/machines")
	if code != 200 || strings.HasSuffix(raw, "\n") || !strings.Contains(raw, `"Zed & Co"`) {
		t.Fatalf("%d %s", code, raw)
	}
	machines := body["machines"].([]any)
	// Canonical order: domain "" (Alpha) before "health" (Zed).
	if len(machines) != 2 || machines[0].(map[string]any)["id"] != "machine-alpha" {
		t.Fatalf("order: %v", machines)
	}
	zed := machines[1].(map[string]any)
	if zed["id"] != "machine-zed-machine" || zed["outputMergeLocked"] != true {
		t.Fatalf("zed: %v", zed)
	}
	// Sequences by name, then id: alpha (s1) before beta (s2), summarised.
	if ids := zed["sequenceIds"].([]any); ids[0] != "s1" || ids[1] != "s2" {
		t.Fatalf("sequenceIds %v", ids)
	}
	first := zed["sequences"].([]any)[0].(map[string]any)
	if len(first) != 3 || first["initialEventIds"].([]any)[0] != "a" {
		t.Fatalf("sequence summary %v", first)
	}
	meta := zed["metadata"].(map[string]any)
	if meta["domain"] != "health" || meta["inputSequences"] == nil {
		t.Fatalf("metadata %v", meta)
	}
	mapping := zed["perceptualMapping"].(map[string]any)
	if _, has := mapping["bitsPerElement"]; has || mapping["input"].(map[string]any)["offset"] != float64(4) {
		t.Fatalf("mapping %v (bitsPerElement 3 is not valid and is dropped)", mapping)
	}
}

func TestMachineFullAndExport(t *testing.T) {
	h := machinesHandler(t)
	code, body, _ := get(t, h, "/api/machines/machine-zed-machine")
	if code != 200 {
		t.Fatalf("%d", code)
	}
	seq := body["machine"].(map[string]any)["sequences"].([]any)[0].(map[string]any)
	if seq["schemaVersion"] != "2" || seq["outputEventIds"].([]any)[0] != "b" {
		t.Fatalf("full sequence %v", seq)
	}
	events := seq["events"].([]any)
	a, b := events[0].(map[string]any), events[1].(map[string]any)
	if a["id"] != "a" || a["state"] != "active" || b["state"] != "inactive" {
		t.Fatalf("events in id order with state: %v %v", a, b)
	}
	el := b["elements"].([]any)[0].(map[string]any)
	if el["comparatorType"] != "equals" || el["threshold"] != 0.25 {
		t.Fatalf("element %v", el)
	}

	code, body, _ = get(t, h, "/api/machines/machine-zed-machine/export")
	doc := body["machine"].(map[string]any)
	if code != 200 || body["version"] != "1.0.0" || doc["arbiterRule"] != "PASSTHROUGH" || doc["inputSequences"] == nil {
		t.Fatalf("export %d %v", code, body)
	}
	if _, stashed := doc["metadata"].(map[string]any)["inputSequences"]; stashed {
		t.Fatal("export leaves inputSequences in metadata")
	}

	for _, path := range []string{"/api/machines/nope", "/api/machines/nope/export"} {
		code, body, _ = get(t, h, path)
		if code != 404 || body["error"] != "Machine not found" {
			t.Fatalf("%s: %d %v", path, code, body)
		}
	}
}

func TestMachineCatalog(t *testing.T) {
	h := machinesHandler(t)
	code, body, _ := get(t, h, "/api/machines/json/list")
	rows := body["machines"].([]any)
	if code != 200 || len(rows) != 2 {
		t.Fatalf("%d %v", code, body)
	}
	deep := rows[1].(map[string]any)
	if deep["relFile"] != "domains/health/Zed_Machine.json" || deep["name"] != "Zed_Machine" || deep["filename"] != "Zed_Machine.json" {
		t.Fatalf("row %v", deep)
	}
}

func TestMachinesWithoutACorpus(t *testing.T) {
	t.Setenv("MACHINES_DIR", "")
	h := mustHandler(t, "RE")
	if _, body, _ := get(t, h, "/api/machines"); len(body["machines"].([]any)) != 0 {
		t.Fatalf("%v", body)
	}
	if _, body, _ := get(t, h, "/api/machines/json/list"); len(body["machines"].([]any)) != 0 {
		t.Fatalf("%v", body)
	}
}
