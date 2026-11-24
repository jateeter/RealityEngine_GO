package server

import (
	"net/http/httptest"
	"testing"
	"time"
)

// A PE pushing to a real Go RE: the corpus fixture's Zed declares an input
// sequence ([1, 0] at cells 4-5), which the PE interns at boot and pushes.
func TestPushDrivesARealRE(t *testing.T) {
	re := httptest.NewServer(machinesHandler(t))
	t.Cleanup(re.Close)
	t.Setenv("RE_URL", re.URL)
	t.Setenv("PE_SOURCE_BOOTSTRAP", "")
	h := mustHandler(t, "PE")

	code, p := send(t, h, "POST", "/api/push", `{"compact": true}`)
	if code != 200 || p["success"] != true || p["globalStep"] != 1.0 {
		t.Fatalf("%d %v", code, p)
	}
	step := p["step"].(map[string]any)
	if _, has := step["machineResults"]; has {
		t.Fatal("compact omits exactly machineResults")
	}
	for _, key := range []string{"stepNumber", "perceptualSpace", "perceptualSpaceIsDebugProjection", "activeRegions", "mergeBatch", "eventBus"} {
		if _, ok := step[key]; !ok {
			t.Fatalf("step lacks %s", key)
		}
	}
	if space := step["perceptualSpace"].([]any); space[4] != 1.0 {
		t.Fatalf("the test source's first event reached the RE: %v", space[:6])
	}
	_, st := send(t, h, "GET", "/api/state", "")
	if st["globalStep"] != 1.0 || st["lastPush"] == nil {
		t.Fatalf("%v", st["globalStep"])
	}

	// Async: 202 and a job that completes.
	code, a := send(t, h, "POST", "/api/push", `{"async": true}`)
	if code != 202 || a["accepted"] != true {
		t.Fatalf("%d %v", code, a)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, j := send(t, h, "GET", "/api/push/"+a["jobId"].(string), "")
		if j["status"] == "completed" {
			if j["result"].(map[string]any)["globalStep"] != 2.0 {
				t.Fatalf("%v", j)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job never completed: %v", j)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if code, _ := send(t, h, "GET", "/api/push/nope", ""); code != 404 {
		t.Fatal(code)
	}

	// Auto push runs until stopped.
	send(t, h, "POST", "/api/auto/start", `{"intervalMs": 10}`)
	time.Sleep(100 * time.Millisecond)
	send(t, h, "POST", "/api/auto/stop", "")
	time.Sleep(50 * time.Millisecond)
	_, st = send(t, h, "GET", "/api/state", "")
	steps := st["globalStep"].(float64)
	if steps < 4 || st["auto"].(map[string]any)["running"] != false {
		t.Fatalf("auto pushed %v times", steps-2)
	}
	time.Sleep(50 * time.Millisecond)
	if _, again := send(t, h, "GET", "/api/state", ""); again["globalStep"] != steps {
		t.Fatal("auto push continued after stop")
	}

	// The PE's reset never resets the RE.
	send(t, h, "POST", "/api/reset", "")
	if _, c := send(t, re.Config.Handler, "GET", "/api/engine/clock", ""); c["step"] == -1.0 {
		t.Fatal("the PE reset reset the RE")
	}
}
