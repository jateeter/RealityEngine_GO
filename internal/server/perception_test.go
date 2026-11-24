package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// peHandler is a PE whose RE is a test server answering /api/machines.
func peHandler(t *testing.T, machines string) http.Handler {
	t.Helper()
	re := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/machines" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(machines))
	}))
	t.Cleanup(re.Close)
	t.Setenv("RE_URL", re.URL)
	t.Setenv("PE_SOURCE_BOOTSTRAP", "")
	return mustHandler(t, "PE")
}

const reListing = `{"machines": [{"id": "machine-a", "name": "A",
  "perceptualMapping": {"input": {"offset": 2, "length": 2}, "output": {"offset": 4, "length": 1}},
  "metadata": {"inputSequences": [{"name": "warm", "events": [[1, 0], [0, 1]]}]}},
 {"id": "machine-b", "name": "B", "metadata": {}}]}`

func TestPEBootInternsTestSourcesAndResetKeepsThem(t *testing.T) {
	h := peHandler(t, reListing)
	_, list := send(t, h, "GET", "/api/sources", "")
	sources := list["sources"].([]any)
	if len(sources) != 1 {
		t.Fatalf("one machine with inputSequences, one test source: %v", list)
	}
	src := sources[0].(map[string]any)
	if src["id"] != "test-machine-a" || src["name"] != "A / warm" || src["active"] != true || src["type"] != "test" {
		t.Fatalf("%v", src)
	}
	_, again := send(t, h, "POST", "/api/sources/bootstrap-from-machines", "")
	if again["created"] != 0.0 || again["skipped"] != 2.0 || again["machinesSeen"] != 2.0 {
		t.Fatalf("skip-if-present: %v", again)
	}
	_, st := send(t, h, "GET", "/api/state", "")
	if v := st["assembledVector"].([]any); v[2] != 1.0 || v[3] != 0.0 {
		t.Fatalf("the test source writes its first event: %v", v[:5])
	}
	send(t, h, "POST", "/api/reset", "")
	if _, list := send(t, h, "GET", "/api/sources", ""); len(list["sources"].([]any)) != 1 {
		t.Fatal("reset keeps membership")
	}
}

func TestPESourcesSensorsAndSignals(t *testing.T) {
	h := peHandler(t, `{"machines": []}`)
	_, c := send(t, h, "POST", "/api/sources", `{"id": "s", "name": "sensor", "type": "sensor", "sensorId": "x", "region": {"offset": 0, "length": 2}}`)
	if c["source"].(map[string]any)["active"] != false {
		t.Fatalf("a sensor without a value is not active, whatever it asks: %v", c)
	}
	send(t, h, "POST", "/api/sensors/x", `{"values": [0.5, 3]}`)
	_, st := send(t, h, "GET", "/api/state", "")
	if v := st["assembledVector"].([]any); v[0] != 0.5 || v[1] != 1.0 {
		t.Fatalf("a live sensor writes, clamped to [0, 1]: %v", v[:2])
	}
	if code, out := send(t, h, "POST", "/api/sensors/nope", `{"values": [1]}`); code != 404 || !strings.Contains(out["error"].(string), `"nope"`) {
		t.Fatalf("%d %v", code, out)
	}
	if code, _ := send(t, h, "POST", "/api/sources", `{"type": "simulated", "pattern": "zigzag"}`); code != 500 {
		t.Fatalf("unknown pattern: %d", code)
	}
	for body, want := range map[string]int{`{"values": []}`: 400, `{"values": [1]}`: 400, `{"sensorId": "nope", "values": [1]}`: 404} {
		if code, _ := send(t, h, "POST", "/api/signals", body); code != want {
			t.Fatalf("%s: %d", body, code)
		}
	}
	_, sig := send(t, h, "POST", "/api/signals", `{"sensorId": "y", "region": {"offset": 3, "length": 1}, "values": [1]}`)
	if s := sig["source"].(map[string]any); s["id"] != "y" || s["active"] != true {
		t.Fatalf("a signal with a region registers its sensor: %v", sig)
	}
	_, p := send(t, h, "PATCH", "/api/sources/s", `{"name": "renamed", "active": false}`)
	if s := p["source"].(map[string]any); s["name"] != "renamed" || s["active"] != false {
		t.Fatalf("%v", p)
	}
	if code, _ := send(t, h, "DELETE", "/api/sources/s", ""); code != 200 {
		t.Fatal(code)
	}
	if code, _ := send(t, h, "DELETE", "/api/sources/s", ""); code != 404 {
		t.Fatal(code)
	}
	if code, _ := send(t, h, "PATCH", "/api/config", `{"matchAlgorithm": "fuzzy"}`); code != 500 {
		t.Fatal(code)
	}
}

func TestPEMachinesProxyWithoutAnRE(t *testing.T) {
	t.Setenv("RE_URL", "http://127.0.0.1:1")
	t.Setenv("PE_SOURCE_BOOTSTRAP", "off")
	h := mustHandler(t, "PE")
	if code, _ := send(t, h, "GET", "/api/machines", ""); code != 502 {
		t.Fatalf("an unavailable RE is a 502: %d", code)
	}
	_, b := send(t, h, "POST", "/api/sources/bootstrap-from-machines", "")
	if b["created"] != 0.0 || len(b["errors"].([]any)) != 1 {
		t.Fatalf("%v", b)
	}
}

func TestPESensorActivityExpiresWithItsTTL(t *testing.T) {
	h := peHandler(t, `{"machines": []}`)
	send(t, h, "POST", "/api/sources", `{"id": "s", "name": "s", "type": "sensor", "sensorId": "x", "ttlMs": 20, "region": {"offset": 0, "length": 1}}`)
	send(t, h, "POST", "/api/sensors/x", `{"values": [1]}`)
	if _, l := send(t, h, "GET", "/api/sources", ""); l["sources"].([]any)[0].(map[string]any)["active"] != true {
		t.Fatal("live inside its TTL")
	}
	time.Sleep(40 * time.Millisecond)
	_, l := send(t, h, "GET", "/api/sources", "")
	s := l["sources"].([]any)[0].(map[string]any)
	if s["active"] != false || s["lastValue"].([]any)[0] != 1.0 {
		t.Fatalf("past its TTL a sensor reads inactive and keeps its last value: %v", s)
	}
	if _, st := send(t, h, "GET", "/api/state", ""); st["assembledVector"].([]any)[0] != 0.0 {
		t.Fatal("an expired sensor writes nothing")
	}
}
