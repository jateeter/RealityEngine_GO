package server

import (
	"strings"
	"testing"
)

func TestConfigDimensionAndThreshold(t *testing.T) {
	t.Setenv("VECTOR_DIMENSION", "16")
	h := machinesHandler(t)
	if _, c := send(t, h, "GET", "/api/config", ""); c["eventDimension"] != 16.0 || c["matchThreshold"] != 0.5 || len(c) != 4 {
		t.Fatalf("%v", c)
	}
	for q, want := range map[string]int{
		"": 400, "?dimension=x": 400, "?dimension=6": 400, "?dimension=15": 400, "?dimension=32": 200,
	} {
		if code, out := send(t, h, "PUT", "/api/config/dimension"+q, ""); code != want {
			t.Fatalf("%s: %d %v", q, code, out)
		}
	}
	_, out := send(t, h, "PUT", "/api/config/dimension?dimension=20", "")
	if !strings.Contains(out["error"].(string), "the 32 this engine already holds") {
		t.Fatalf("the refusal names which bound applied: %v", out)
	}
	for q, want := range map[string]int{"?threshold=0.7": 200, "": 200, "?threshold=high": 400, "?threshold=0.7abc": 400, "?threshold=nan": 400} {
		if code, out := send(t, h, "PUT", "/api/config/threshold"+q, ""); code != want || (want == 200 && out["threshold"] != 0.7) {
			t.Fatalf("%s: %d %v", q, code, out)
		}
	}
	if _, c := send(t, h, "GET", "/api/config", ""); c["eventDimension"] != 32.0 || c["matchThreshold"] != 0.7 {
		t.Fatalf("%v", c)
	}
}

func TestVectorStore(t *testing.T) {
	h := machinesHandler(t)
	_, stored := send(t, h, "POST", "/api/vectors", `{"vector": [1, 0], "unknown": {"kept": true}}`)
	doc := stored["vector"].(map[string]any)
	id, _ := doc["id"].(string)
	if !strings.HasPrefix(id, "vector-") || doc["unknown"].(map[string]any)["kept"] != true {
		t.Fatalf("%v", stored)
	}
	send(t, h, "POST", "/api/vectors", `{"id": "a", "elements": [{"value": 0}, 1]}`)
	_, found := send(t, h, "POST", "/api/vectors/search", `{"vector": [1, 0], "threshold": 0.5}`)
	if r := found["results"].([]any); len(r) != 1 || r[0].(map[string]any)["score"] != 1.0 {
		t.Fatalf("first-k above threshold: %v", found)
	}
	if code, _ := send(t, h, "GET", "/api/vectors/"+id, ""); code != 200 {
		t.Fatal(code)
	}
	send(t, h, "DELETE", "/api/vectors/"+id, "")
	if code, out := send(t, h, "GET", "/api/vectors/"+id, ""); code != 404 || out["error"] != "Vector not found" {
		t.Fatalf("%d %v", code, out)
	}
	if code, _ := send(t, h, "POST", "/api/vectors", `[1]`); code != 400 {
		t.Fatalf("a vector must be an object: %d", code)
	}
}

func TestGraphGovernanceAndDemos(t *testing.T) {
	h := machinesHandler(t)
	_, g := send(t, h, "GET", "/api/machine-graph", "")
	if len(g["nodes"].([]any)) != 1 {
		t.Fatalf("only mapped machines are nodes: %v", g)
	}
	if code, _ := send(t, h, "GET", "/api/governance/route", ""); code != 400 {
		t.Fatal(code)
	}
	if code, out := send(t, h, "GET", "/api/governance/route?machineId=nope&sequenceId=s&values=1", ""); code != 404 || out["error"] != "Machine not found: nope" {
		t.Fatalf("%d %v", code, out)
	}
	if code, _ := send(t, h, "GET", "/api/governance/route?machineId=machine-zed-machine&sequenceId=s1&values=1", ""); code != 404 {
		t.Fatal("no triggerConfig rule matches")
	}
	if code, out := send(t, h, "GET", "/api/demo/kleene-star", ""); code != 404 || !strings.Contains(out["error"].(string), "KleeneStar.json") {
		t.Fatalf("%d %v", code, out)
	}
	if _, r := send(t, h, "GET", "/", ""); r["name"] != "Reality Engine" || r["status"] != "running" {
		t.Fatalf("%v", r)
	}
}
