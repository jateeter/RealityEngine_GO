package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func send(t *testing.T, h http.Handler, method, path, body string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s %s: %v: %s", method, path, err, rec.Body)
	}
	return rec.Code, out
}

const fooMachine = `{"version": "1.0.0", "machine": {"name": "Foo",
  "perceptualMapping": {"input": {"offset": 10, "length": 1}, "output": {"offset": 11, "length": 1}},
  "sequences": [{"id": "s", "name": "s", "events": [
    {"id": "a", "isInitial": true, "elements": [{"value": 1}], "nextEventIds": ["b"]},
    {"id": "b", "elements": [{"value": 0}], "outputEvents": [{"id": "o", "vector": [1]}]}]}]}}`

func region(t *testing.T, m map[string]any, which string) map[string]any {
	t.Helper()
	return m["perceptualMapping"].(map[string]any)[which].(map[string]any)
}

// SURFACE_SPEC "POST /api/machines always ingests": versioning continues from
// the base, a versioned machine's regions are appended at the end of the
// space, and DELETE frees the name.
func TestPostVersionsAResidentNameAndDeleteFreesIt(t *testing.T) {
	t.Setenv("VECTOR_DIMENSION", "100")
	h := machinesHandler(t)
	code, first := send(t, h, "POST", "/api/machines", fooMachine)
	if code != 200 || first["versioned"] != false {
		t.Fatalf("%d %v", code, first)
	}
	m1 := first["machine"].(map[string]any)
	if m1["name"] != "Foo" || region(t, m1, "input")["offset"] != 10.0 || !strings.HasPrefix(m1["id"].(string), "machine-") {
		t.Fatalf("first: %v", m1)
	}
	_, second := send(t, h, "POST", "/api/machines", fooMachine)
	m2 := second["machine"].(map[string]any)
	if second["versioned"] != true || m2["name"] != "Foo v2" ||
		region(t, m2, "input")["offset"] != 100.0 || region(t, m2, "output")["offset"] != 101.0 {
		t.Fatalf("second: %v", second)
	}
	_, third := send(t, h, "POST", "/api/machines", strings.Replace(fooMachine, `"Foo"`, `"Foo v2"`, 1))
	if m3 := third["machine"].(map[string]any); m3["name"] != "Foo v3" || region(t, m3, "input")["offset"] != 102.0 {
		t.Fatalf("third: %v", third)
	}

	if code, del := send(t, h, "DELETE", "/api/machines/"+m1["id"].(string), ""); code != 200 || del["success"] != true {
		t.Fatalf("delete: %d %v", code, del)
	}
	if code, del := send(t, h, "DELETE", "/api/machines/"+m1["id"].(string), ""); code != 404 || del["error"] != "machine "+m1["id"].(string)+" not found" {
		t.Fatalf("delete again: %d %v", code, del)
	}
	_, again := send(t, h, "POST", "/api/machines", fooMachine)
	if m := again["machine"].(map[string]any); again["versioned"] != false || m["name"] != "Foo" || region(t, m, "input")["offset"] != 10.0 {
		t.Fatalf("after delete: %v", again)
	}
}

// SURFACE_SPEC "POST /api/machines keeps a declared id": kept when free,
// minted when held.
func TestPostKeepsAFreeDeclaredID(t *testing.T) {
	h := machinesHandler(t)
	declared := strings.Replace(fooMachine, `"machine": {"name": "Foo",`, `"machine": {"id": "machine-mine", "name": "Foo",`, 1)
	if _, out := send(t, h, "POST", "/api/machines", declared); out["machine"].(map[string]any)["id"] != "machine-mine" {
		t.Fatalf("%v", out)
	}
	if _, out := send(t, h, "POST", "/api/machines", declared); out["machine"].(map[string]any)["id"] == "machine-mine" {
		t.Fatalf("a held id was reused: %v", out)
	}
	// An id on the envelope is not the machine's declaration.
	onEnvelope := strings.Replace(fooMachine, `{"version": "1.0.0",`, `{"version": "1.0.0", "id": "machine-envelope",`, 1)
	if _, out := send(t, h, "POST", "/api/machines", onEnvelope); out["machine"].(map[string]any)["id"] == "machine-envelope" {
		t.Fatalf("an envelope id was taken as declared: %v", out)
	}
}

func TestPostRefusesABadBody(t *testing.T) {
	h := machinesHandler(t)
	for _, body := range []string{"", "{", `{"machine": {"name": ""}}`, `{"version": "2.0", "machine": {"name": "x"}}`} {
		if code, out := send(t, h, "POST", "/api/machines", body); code != 400 || out["error"] == "" {
			t.Fatalf("%q: %d %v", body, code, out)
		}
	}
}

func TestProcessWhatIfAndUniversal(t *testing.T) {
	h := machinesHandler(t)
	_, out := send(t, h, "POST", "/api/machines", fooMachine)
	id := out["machine"].(map[string]any)["id"].(string)
	base := "/api/machines/" + id

	for body, want := range map[string]string{
		`{}`:                     "request must include inputEvent",
		`{"inputEvent": 1}`:      "inputEvent must be an array",
		`{"inputEvent": [1, 0]}`: "input vector length 2 does not match machine input length 1",
		`{"inputEvent": ["x"]}`:  "inputEvent values must be finite numbers",
	} {
		if code, out := send(t, h, "POST", base+"/process", body); code != 400 || out["error"] != want {
			t.Fatalf("%s: %d %v", body, code, out)
		}
	}
	if code, out := send(t, h, "POST", "/api/machines/nope/process", `{"inputEvent": [1]}`); code != 404 || out["error"] != "machine nope not found" {
		t.Fatalf("%d %v", code, out)
	}

	// What-if persists nothing: b is not activated by it.
	_, w := send(t, h, "POST", base+"/whatif", `{"inputEvent": [1]}`)
	if _, has := w["machineId"]; has {
		t.Fatalf("whatif carries machineId: %v", w)
	}
	if got := w["sequenceResults"].(map[string]any)["s"].(map[string]any)["activatedEvents"]; len(got.([]any)) != 1 {
		t.Fatalf("whatif activated %v", got)
	}
	_, p := send(t, h, "POST", base+"/process", `{"inputEvent": [0]}`)
	if p["machineOutput"] != nil {
		t.Fatalf("b fired without being activated (what-if persisted): %v", p)
	}

	// The universal space is sliced at the input region (offset 10).
	space := make([]string, 11)
	for i := range space {
		space[i] = "0"
	}
	space[10] = "1"
	universal := `{"universalInputSpace": [` + strings.Join(space, ",") + `]}`
	if code, p := send(t, h, "POST", base+"/process-universal", universal); code != 200 || len(p["sequenceResults"].(map[string]any)["s"].(map[string]any)["matchedEvents"].([]any)) != 1 {
		t.Fatalf("%d %v", code, p)
	}
	space[10] = "0"
	_, all := send(t, h, "POST", "/api/machines/process-universal/all", `{"universalInputSpace": [`+strings.Join(space, ",")+`]}`)
	result, ok := all["results"].(map[string]any)[id].(map[string]any)
	if !ok || result["machineOutput"] == nil {
		t.Fatalf("b should fire on the universal step: %v", all)
	}
	// Alpha has no mapping and is not driven; Zed is.
	if _, has := all["results"].(map[string]any)["machine-alpha"]; has {
		t.Fatalf("an unmapped machine was driven: %v", all)
	}
}

func TestPutPatchImportAndLoadByName(t *testing.T) {
	h := machinesHandler(t)
	id := "machine-zed-machine"
	code, put := send(t, h, "PUT", "/api/machines/"+id, fooMachine)
	if m := put["machine"].(map[string]any); code != 200 || m["id"] != id || m["name"] != "Foo" {
		t.Fatalf("%d %v", code, put)
	}
	code, patch := send(t, h, "PATCH", "/api/machines/"+id, `{"name": "Bar", "metadata": {"k": 1}}`)
	if m := patch["machine"].(map[string]any); code != 200 || m["name"] != "Bar" || m["metadata"].(map[string]any)["k"] != 1.0 {
		t.Fatalf("%d %v", code, patch)
	}
	if code, out := send(t, h, "PATCH", "/api/machines/nope", `{}`); code != 404 || out["error"] != "Machine not found" {
		t.Fatalf("%d %v", code, out)
	}

	_, imp := send(t, h, "POST", "/api/machines/json/import", fooMachine)
	if m := imp["machine"].(map[string]any); m["name"] != "Foo" || !strings.HasPrefix(m["id"].(string), "machine-") || len(m["id"].(string)) != len("machine-")+36 {
		t.Fatalf("%v", imp)
	}

	// Loading by name puts the corpus file back under its canonical id.
	code, byName := send(t, h, "GET", "/api/machines/json/Zed_Machine", "")
	if m := byName["machine"].(map[string]any); code != 200 || m["id"] != id || m["name"] != "Zed & Co" || byName["message"] != "Machine loaded successfully" {
		t.Fatalf("%d %v", code, byName)
	}
	if code, _ := send(t, h, "GET", "/api/machines/json/Absent", ""); code != 404 {
		t.Fatalf("absent: %d", code)
	}
	if code, _ := send(t, h, "GET", "/api/machines/json/..secret", ""); code != 400 {
		t.Fatalf("traversal: %d", code)
	}
}

func TestCheckpoints(t *testing.T) {
	h := machinesHandler(t)
	_, out := send(t, h, "POST", "/api/machines", fooMachine)
	id := out["machine"].(map[string]any)["id"].(string)
	base := "/api/machines/" + id

	_, cp := send(t, h, "POST", base+"/checkpoints", `{"label": "start"}`)
	cpID, _ := cp["checkpointId"].(string)
	if cp["success"] != true || !strings.HasPrefix(cpID, "checkpoint-") {
		t.Fatalf("%v", cp)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", base+"/checkpoints", nil))
	var list []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list) != 1 || list[0]["id"] != cpID || list[0]["label"] != "start" {
		t.Fatalf("%v %s", err, rec.Body)
	}

	// Advance the machine, restore, and b is no longer active.
	send(t, h, "POST", base+"/process", `{"inputEvent": [1]}`)
	if _, out := send(t, h, "POST", base+"/checkpoints/"+cpID+"/restore", ""); out["success"] != true {
		t.Fatalf("%v", out)
	}
	if _, p := send(t, h, "POST", base+"/process", `{"inputEvent": [0]}`); p["machineOutput"] != nil {
		t.Fatalf("restore did not return the machine to its checkpoint: %v", p)
	}
	if code, out := send(t, h, "POST", base+"/checkpoints/nope/restore", ""); code != 404 || out["error"] != "Checkpoint not found" {
		t.Fatalf("%d %v", code, out)
	}
	if _, out := send(t, h, "DELETE", base+"/checkpoints/"+cpID, ""); out["success"] != true {
		t.Fatalf("%v", out)
	}
	if _, out := send(t, h, "DELETE", base+"/checkpoints/"+cpID, ""); out["success"] != false {
		t.Fatalf("%v", out)
	}
}
