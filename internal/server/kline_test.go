package server

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const twoStep = `{"version": "1.0.0", "machine": {"name": "two-step",
  "perceptualMapping": {"input": {"offset": 0, "length": 1}, "output": {"offset": 1, "length": 1}},
  "sequences": [{"id": "sequence", "events": [
    {"id": "start", "isInitial": true, "elements": [{"value": 1, "comparatorType": "equals"}], "nextEventIds": ["finish"]},
    {"id": "finish", "elements": [{"value": 0, "comparatorType": "equals"}], "outputEvents": [{"id": "out", "vector": [1]}]}
  ]}]}}`

// K-lines (RealityEngine_CI#603, #17): off by default; with capture on, a
// departure on start and the final finish close one completed K-line with
// its path and sustain; an abandoned path names its first failed comparison;
// a reset closes what is open interrupted; an export is a bundle whose header
// hashes its record lines.
func TestKlinesAreCapturedAtTheirBoundaries(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "TwoStep.json"), []byte(twoStep), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MACHINES_DIR", dir)
	t.Setenv("SEMANTICS_MANIFEST", filepath.Join(dir, "no-manifest.json"))
	h := mustHandler(t, "RE")
	push := func(v string) { send(t, h, "POST", "/api/perceive", `{"vector": [`+v+`]}`) }

	push("1")
	push("0")
	if _, body := send(t, h, "GET", "/api/klines", ""); len(body["klines"].([]any)) != 0 || body["next"] != nil {
		t.Fatalf("off: %v", body)
	}
	if code, _ := send(t, h, "PUT", "/api/engine/config/klineCapture", `{"value": true}`); code != 200 {
		t.Fatalf("capture on: %d", code)
	}
	push("1")
	if _, open := send(t, h, "GET", "/api/klines/open", ""); encodeJSON(open["open"].([]any)[0].(map[string]any)["held"]) != `["finish"]` {
		t.Fatalf("open: %v", open)
	}
	push("0")
	_, body := send(t, h, "GET", "/api/klines", "")
	klines := body["klines"].([]any)
	if len(klines) != 1 {
		t.Fatalf("%v", body)
	}
	k := klines[0].(map[string]any)
	if k["outcome"] != "completed" || encodeJSON(k["focal"]) != `{"machine":"two-step","sequence":"sequence"}` {
		t.Fatalf("%v", k)
	}
	var path []string
	for _, p := range k["path"].([]any) {
		path = append(path, p.(map[string]any)["event"].(string))
	}
	if strings.Join(path, ",") != "start,finish" || encodeJSON(k["sustain"]) != `{"ended":false,"through":3}` {
		t.Fatalf("path %v sustain %v", path, k["sustain"])
	}
	id := k["id"].(string)
	if code, one := send(t, h, "GET", "/api/klines/"+id, ""); code != 200 || one["id"] != id {
		t.Fatalf("%d %v", code, one)
	}

	push("1")
	push("0.5")
	_, body = send(t, h, "GET", "/api/klines?outcome=abandoned", "")
	d := body["klines"].([]any)[0].(map[string]any)["difference"].(map[string]any)
	if encodeJSON(d["expected"]) != `["finish"]` || d["comparator"] != "equals" || d["value"] != 0.5 {
		t.Fatalf("%v", d)
	}
	if _, one := send(t, h, "GET", "/api/klines/"+id, ""); one["sustain"].(map[string]any)["ended"] != true {
		t.Fatalf("sustain did not end: %v", one["sustain"])
	}

	push("1")
	send(t, h, "POST", "/api/engine/reset", `{}`)
	_, body = send(t, h, "GET", "/api/klines?outcome=interrupted", "")
	if body["klines"].([]any)[0].(map[string]any)["reason"] != "reset" {
		t.Fatalf("%v", body)
	}
	if _, stats := send(t, h, "GET", "/api/klines/stats", ""); encodeJSON(stats["byOutcome"]) != `{"abandoned":1,"completed":1,"interrupted":1}` {
		t.Fatalf("%v", stats)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/klines/export", strings.NewReader("{}")))
	text := rec.Body.String()
	header, records, _ := strings.Cut(text, "\n")
	if !strings.Contains(header, `"format":"re-kline-bundle/1"`) || !strings.Contains(header, `"sha256":"`+sha256Hex(records)+`"`) || !strings.HasSuffix(text, "\n") {
		t.Fatalf("bundle header %s", header)
	}
	if code, _ := send(t, h, "GET", "/api/klines?since=x", ""); code != 400 {
		t.Fatalf("since=x: %d", code)
	}
	if code, _ := send(t, h, "GET", "/api/klines/no-such", ""); code != 404 {
		t.Fatalf("no-such: %d", code)
	}
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
