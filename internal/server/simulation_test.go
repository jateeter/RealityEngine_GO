package server

import (
	"testing"
	"time"
)

func TestPerceptualSimulationWalksTheSequence(t *testing.T) {
	t.Setenv("VECTOR_DIMENSION", "16")
	h := machinesHandler(t)
	send(t, h, "POST", "/api/machines", fooMachine) // reads cell 10: a on 1, then b on 0
	if code, _ := send(t, h, "POST", "/api/perceptual-simulation/step", ""); code != 400 {
		t.Fatalf("unconfigured: %d", code)
	}
	if code, _ := send(t, h, "POST", "/api/perceptual-simulation/configure/commit", ""); code != 400 {
		t.Fatalf("no config buffered: %d", code)
	}
	send(t, h, "POST", "/api/perceptual-simulation/configure/chunk", `{"reset": true, "events": [[1]]}`)
	_, chunk := send(t, h, "POST", "/api/perceptual-simulation/configure/chunk", `{"events": [[0]], "config": {"inputRegion": {"offset": 10, "length": 1}, "stepDelayMs": 1}}`)
	if chunk["bufferedEvents"] != 2.0 {
		t.Fatalf("%v", chunk)
	}
	send(t, h, "POST", "/api/perceptual-simulation/configure/commit", "")
	_, first := send(t, h, "POST", "/api/perceptual-simulation/step", "")
	_, second := send(t, h, "POST", "/api/perceptual-simulation/step", "")
	if first["step"].(map[string]any)["stepNumber"] != 0.0 || len(second["step"].(map[string]any)["mergeBatch"].([]any)) == 0 {
		t.Fatalf("b fires on the second step: %v", second)
	}
	if _, done := send(t, h, "POST", "/api/perceptual-simulation/step", ""); done["done"] != true {
		t.Fatalf("past the end: %v", done)
	}
	_, hist := send(t, h, "GET", "/api/perceptual-simulation/history", "")
	if steps := hist["history"].([]any); len(steps) != 2 || steps[0].(map[string]any)["stepNumber"] != 1.0 {
		t.Fatalf("newest first: %v", hist)
	}

	// Auto-play runs the walk to its end and stops.
	send(t, h, "POST", "/api/perceptual-simulation/reset", "")
	send(t, h, "POST", "/api/perceptual-simulation/start", "")
	deadline := time.Now().Add(2 * time.Second)
	for {
		_, st := send(t, h, "GET", "/api/perceptual-simulation/state", "")
		s := st["state"].(map[string]any)
		if s["isRunning"] == false && s["currentStep"] == 2.0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("auto-play did not finish: %v", s["currentStep"])
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestSamplerIsBookkeeping(t *testing.T) {
	h := machinesHandler(t)
	_, start := send(t, h, "POST", "/api/sampler/start", `{"strategy": "periodic", "intervalMs": 250}`)
	if st := start["stats"].(map[string]any); st["isRunning"] != true || st["strategy"] != "periodic" || st["intervalMs"] != 250.0 {
		t.Fatalf("%v", start)
	}
	_, sample := send(t, h, "POST", "/api/sampler/sample", `{"data": [1, "x"]}`)
	if in := sample["result"].(map[string]any)["inputEvent"].([]any); len(in) != 2 || in[1] != 0.0 {
		t.Fatalf("%v", sample)
	}
	send(t, h, "POST", "/api/sampler/stop", "")
	send(t, h, "POST", "/api/engine/reset", "")
	if _, st := send(t, h, "GET", "/api/sampler/stats", ""); st["stats"].(map[string]any)["sampleCount"] != 1.0 {
		t.Fatalf("a reset keeps the sampler: %v", st)
	}
}
