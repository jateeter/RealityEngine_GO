package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Two machines writing the same output cell: Lo writes 2, Hi writes 3 (join
// keeps a chain member; the default or would binarise it).
func contender(name, value string) string {
	return `{"version": "1.0.0", "machine": {"name": "` + name + `", "outputMergeTransformation": "join",
  "perceptualMapping": {"input": {"offset": 20, "length": 1}, "output": {"offset": 30, "length": 1}},
  "sequences": [{"id": "s-` + name + `", "name": "s", "events": [
    {"id": "e", "isInitial": true, "elements": [{"value": 1}], "outputEvents": [{"id": "o", "vector": [` + value + `]}]}]}]}}`
}

func TestPerceiveCommitsAPairAndArbitratesAContendedCell(t *testing.T) {
	t.Setenv("VECTOR_DIMENSION", "40")
	t.Setenv("ARBITRATION_REGISTRY", "/nonexistent")
	h := machinesHandler(t)
	send(t, h, "POST", "/api/machines", contender("Lo", "2"))
	send(t, h, "POST", "/api/machines", contender("Hi", "3"))

	code, s := send(t, h, "POST", "/api/perceive", `{"sparseVector": [{"index": 20, "value": 1}]}`)
	if code != 200 || s["stepNumber"] != 0.0 {
		t.Fatalf("%d %v", code, s)
	}
	merges := s["mergeBatch"].([]any)
	if len(merges) != 2 || merges[0].(map[string]any)["machineName"] != "Hi" {
		t.Fatalf("mergeBatch is ordered by machineName: %v", merges)
	}
	space := s["perceptualSpace"].([]any)
	if space[30] != 3.0 || space[20] != 1.0 {
		t.Fatalf("PRECEDENCE among machines keeps the larger value: %v %v", space[20], space[30])
	}
	_, arb := send(t, h, "GET", "/api/arbitration", "")
	if arb["count"] != 1.0 || arb["registrySource"] != nil {
		t.Fatalf("%v", arb)
	}
	rec := arb["records"].([]any)[0].(map[string]any)
	if rec["cell"] != 30.0 || rec["rule"] != "PRECEDENCE" || len(rec["suppressed"].([]any)) != 1 {
		t.Fatalf("%v", rec)
	}

	_, pair := send(t, h, "GET", "/api/engine/steps/0/pair", "")
	osre := pair["osre"].(map[string]any)["nonZero"].([]any)
	if pair["stepNumber"] != 0.0 || len(osre) != 1 || osre[0].(map[string]any)["index"] != 30.0 {
		t.Fatalf("%v", pair)
	}
	_, isre := send(t, h, "GET", "/api/engine/isre-history", "")
	if entries := isre["history"].([]any); len(entries) != 1 || entries[0].(map[string]any)["length"] != 40.0 {
		t.Fatalf("%v", isre)
	}
	if _, c := send(t, h, "GET", "/api/engine/clock", ""); c["step"] != 0.0 || c["lamport"] != 1.0 {
		t.Fatalf("the clock ticks once per committed step: %v", c)
	}

	// The selector: present, it selects; naming nothing selects nothing.
	_, sel := send(t, h, "POST", "/api/perceive", `{"vector": [], "only": {}, "includePerceptualSpace": false}`)
	if len(sel["mergeBatch"].([]any)) != 0 || len(sel["machineResults"].(map[string]any)) != 0 || sel["perceptualSpace"] != nil {
		t.Fatalf("%v", sel)
	}
	_, sel = send(t, h, "POST", "/api/perceive", `{"vector": [], "compact": true, "only": {"machineNames": ["Lo"]}}`)
	if _, has := sel["machineResults"]; has {
		t.Fatalf("compact omits machineResults: %v", sel)
	}
}

func TestStepPairWaitsAndRefuses(t *testing.T) {
	h := machinesHandler(t)
	for path, want := range map[string]int{
		"/api/engine/steps/x/pair":                 400,
		"/api/engine/steps/0/pair?timeoutMs=60001": 400,
		"/api/engine/steps/0/pair?timeoutMs=20":    408,
	} {
		if code, out := send(t, h, "GET", path, ""); code != want || out["error"] == "" {
			t.Fatalf("%s: %d %v", path, code, out)
		}
	}
	// A waiter for step 0 is woken by its commit.
	var wg sync.WaitGroup
	var code int
	wg.Add(1)
	go func() {
		defer wg.Done()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/engine/steps/0/pair?timeoutMs=5000", nil))
		code = rec.Code
	}()
	time.Sleep(50 * time.Millisecond)
	send(t, h, "POST", "/api/perceive", `{"vector": [1]}`)
	wg.Wait()
	if code != 200 {
		t.Fatalf("waiter: %d", code)
	}
	// A reset restarts numbering and the histories: step 0 is gone until
	// the next step 0.
	send(t, h, "POST", "/api/engine/reset", "")
	if code, _ := send(t, h, "GET", "/api/engine/steps/0/pair?timeoutMs=0", ""); code != 408 {
		t.Fatalf("after reset: %d", code)
	}
	if _, c := send(t, h, "GET", "/api/engine/clock", ""); c["step"] != -1.0 || c["lamport"] != 1.0 {
		t.Fatalf("a reset keeps lamport: %v", c)
	}
	for _, body := range []string{`{}`, `{"vector": ["x"]}`, `{"sparseVector": [{"index": -1, "value": 1}]}`} {
		if code, out := send(t, h, "POST", "/api/perceive", body); code != 400 || !strings.Contains(out["error"].(string), "") {
			t.Fatalf("%s: %d %v", body, code, out)
		}
	}
	if code, _ := send(t, h, "POST", "/api/perceive", `{"vector": [1], "matchAlgorithmOverride": "fuzzy"}`); code != 500 {
		t.Fatalf("unknown override: %d", code)
	}
}
