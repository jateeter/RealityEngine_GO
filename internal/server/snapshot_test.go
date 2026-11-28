package server

import (
	"os"
	"path/filepath"
	"testing"
)

// Runtime snapshots (RealityEngine_CI#287, #17): a snapshot over the retained
// steps round-trips through a reset (same digest, the same next step as an
// engine that was never reset); a tampered body is refused 422, a span
// outside retention 410, and an unknown id 404.
func TestSnapshotRoundTripsThroughAReset(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "TwoStep.json"), []byte(twoStep), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MACHINES_DIR", dir)
	t.Setenv("SEMANTICS_MANIFEST", filepath.Join(dir, "no-manifest.json"))
	h := mustHandler(t, "RE")
	push := func(v string) map[string]any {
		_, body := send(t, h, "POST", "/api/perceive", `{"vector": [`+v+`]}`)
		return body
	}
	if code, body := send(t, h, "POST", "/api/runtime/snapshot", `{}`); code != 410 || body["error"] != "no step is retained" {
		t.Fatalf("%d %v", code, body)
	}
	push("1")
	code, taken := send(t, h, "POST", "/api/runtime/snapshot", `{}`)
	if code != 200 || taken["digest"] != "sha256:"+taken["id"].(string) || encodeJSON(taken["span"]) != `{"from":0,"to":0}` {
		t.Fatalf("%d %v", code, taken)
	}
	id := taken["id"].(string)
	_, snap := send(t, h, "GET", "/api/runtime/snapshot/"+id, "")
	if snap["format"] != "re-snapshot/1" {
		t.Fatalf("%v", snap)
	}
	reference := push("0")
	send(t, h, "POST", "/api/engine/reset", `{}`)
	code, restored := send(t, h, "POST", "/api/runtime/restore", `{"id": "`+id+`"}`)
	if code != 200 || restored["digest"] != snap["digest"] || restored["step"] != 0.0 {
		t.Fatalf("%d %v", code, restored)
	}
	replayed := push("0")
	values := func(s map[string]any) string {
		batch, _ := s["mergeBatch"].([]any)
		if len(batch) == 0 {
			return ""
		}
		m := batch[0].(map[string]any)
		return encodeJSON([]any{m["region"], m["values"], m["sequenceIds"]})
	}
	if replayed["stepNumber"] != reference["stepNumber"] || values(replayed) == "" || values(replayed) != values(reference) {
		t.Fatalf("replayed %v, reference %v", values(replayed), values(reference))
	}
	tampered := encodeJSON(map[string]any{"snapshot": func() map[string]any {
		snap["kline"].(map[string]any)["span"] = map[string]any{"from": 0, "to": 9}
		return snap
	}()})
	if code, _ := send(t, h, "POST", "/api/runtime/restore", tampered); code != 422 {
		t.Fatalf("tampered: %d", code)
	}
	if code, _ := send(t, h, "POST", "/api/runtime/snapshot", `{"from": 99}`); code != 410 {
		t.Fatalf("from 99: %d", code)
	}
	if code, _ := send(t, h, "GET", "/api/runtime/snapshot/abc", ""); code != 404 {
		t.Fatalf("unknown: %d", code)
	}
	if code, body := send(t, h, "DELETE", "/api/runtime/snapshot/"+id, ""); code != 200 || body["deleted"] != id {
		t.Fatalf("%d %v", code, body)
	}
}
