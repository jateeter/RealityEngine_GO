package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const producer = `{"type": "published-domain-bus-producer", "targetMachineFile": "Bus.json", "targetInputElements": [0, 1, 2]}`

// cloneCorpus is one machine per rule of SURFACE_SPEC "Cloneable machines",
// each failing only the rule its name gives, under INSTANCE_ID go-1.
func cloneCorpus(t *testing.T, instance string) http.Handler {
	t.Helper()
	dir := t.TempDir()
	agentic := func(extra, edges string) string {
		if extra != "" {
			extra += ", "
		}
		return fmt.Sprintf(`{"agentBinding": {"agent": "a"}, %s"interconnections": [%s]}`, extra, edges)
	}
	write := func(stem, metadata string, mapping string) {
		if mapping != "" {
			mapping = `"perceptualMapping": ` + mapping + `,`
		}
		body := fmt.Sprintf(`{"version": "1.0.0", "machine": {"name": %q, %s "metadata": %s,
		  "sequences": [{"id": "s", "events": [
		    {"id": "a", "isInitial": true, "elements": [{"value": 1}], "nextEventIds": ["b"]},
		    {"id": "b", "elements": [{"value": 1}], "outputEvents": [{"id": "o", "vector": [1]}]}]}]}}`, stem, mapping, metadata)
		if err := os.WriteFile(filepath.Join(dir, stem+".json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	four := `{"input": {"offset": 0, "length": 4}, "output": {"offset": 100, "length": 4}}`
	handoff := `{"interconnections": [{"type": "document-workflow-handoff", "sourceMachine": "Handoff"}]}`
	write("Ok", agentic("", producer), four)
	write("NoAgent", `{"interconnections": [`+producer+`]}`, four)
	write("Handoff", handoff, four)
	write("HandoffElsewhere", strings.Replace(handoff, `"Handoff"`, `"Other"`, 1), four)
	write("NoFanout", agentic("", ""), four)
	write("Bus", agentic("", producer), four)
	write("Safety", agentic(`"severity": "life-safety"`, producer), four)
	write("Fixture", agentic(`"tags": ["x", "test-fixture"]`, producer), four)
	write("Out", agentic(`"cloneable": false`, producer), four)
	write("WideIn", agentic("", producer), `{"input": {"offset": 0, "length": 17}, "output": {"offset": 100, "length": 4}}`)
	write("WideOut", agentic("", producer), `{"input": {"offset": 0, "length": 16}, "output": {"offset": 100, "length": 17}}`)
	write("NoMapping", agentic("", producer), "")
	write("WideFan", agentic("", strings.Repeat(producer+", ", 5)+producer), four) // 18 folded cells
	t.Setenv("MACHINES_DIR", dir)
	t.Setenv("SEMANTICS_MANIFEST", filepath.Join(dir, "no-manifest.json"))
	t.Setenv("INSTANCE_ID", instance)
	return mustHandler(t, "RE")
}

func TestCloneableListingHasEachRulesReason(t *testing.T) {
	h := cloneCorpus(t, "go-1")
	code, body, raw := get(t, h, "/api/machines/cloneable")
	if code != 200 {
		t.Fatalf("%d", code)
	}
	rows := map[string]map[string]any{}
	var ids []string
	for _, r := range body["machines"].([]any) {
		row := r.(map[string]any)
		ids = append(ids, row["id"].(string))
		rows[row["id"].(string)] = row
	}
	for i := 1; i < len(ids); i++ {
		if ids[i-1] >= ids[i] {
			t.Fatalf("not sorted by id: %v", ids)
		}
	}
	if !strings.Contains(raw, `{"cloneable":true,"id":"machine-ok","n":7,"reason":null}`) {
		t.Fatalf("Ok row: %s", raw)
	}
	if r := rows["machine-handoff"]; r["cloneable"] != true || r["n"] != 4.0 {
		t.Fatalf("a handoff folds no cells: %v", r)
	}
	for stem, reason := range map[string]string{
		"noagent": "no-agentic-action", "handoffelsewhere": "no-agentic-action", "nofanout": "no-fanout",
		"bus": "bus-target", "safety": "life-safety", "fixture": "fixture", "out": "opted-out",
		"widein": "input-too-long", "wideout": "output-too-long", "nomapping": "input-too-long", "widefan": "fanout-too-wide",
	} {
		r := rows["machine-"+stem]
		if r["cloneable"] != false || r["reason"] != reason {
			t.Fatalf("%s: %v, want %s", stem, r, reason)
		}
	}
	if rows["machine-widefan"]["n"] != 22.0 || rows["machine-bus"]["n"] != nil {
		t.Fatalf("n is present once rule 9 is evaluated: %v %v", rows["machine-widefan"], rows["machine-bus"])
	}
	// A machine registered from a body has no file: no handoff names it.
	send(t, h, "POST", "/api/machines", `{"name": "Posted", "metadata": `+
		`{"interconnections": [{"type": "document-workflow-handoff", "sourceMachine": "Posted"}]}, "sequences": []}`)
	_, body, _ = get(t, h, "/api/machines/cloneable")
	for _, r := range body["machines"].([]any) {
		if row := r.(map[string]any); row["id"] != nil && strings.HasPrefix(row["id"].(string), "machine-0") && row["reason"] != "no-agentic-action" {
			t.Fatalf("a posted machine is no handoff source: %v", row)
		}
	}
}

func TestCloneAndRecycle(t *testing.T) {
	h := cloneCorpus(t, "go-1")
	clone := func(q string) string { return "/api/engine/clone/machine/machine-ok" + q }

	// The refusals, in the specified order.
	if code, _ := send(t, h, "POST", clone("?asset=a"), ""); code != 400 {
		t.Fatalf("missing envelope: %d", code)
	}
	if code, _ := send(t, h, "POST", "/api/engine/clone/machine/nope?asset=a&envelope=e", ""); code != 404 {
		t.Fatalf("no machine: %d", code)
	}
	if code, b := send(t, h, "POST", "/api/engine/clone/machine/machine-bus?asset=a&envelope=e", ""); code != 400 || b["reason"] != "bus-target" {
		t.Fatalf("%d %v", code, b)
	}

	// 201: block 0, the nest layout, percent-decoded asset and envelope.
	rec := raw(t, h, "POST", clone("?asset=asset%2F42&envelope=env-1"), "")
	want := `{"assetId":"asset/42","block":{"index":0,"length":64,"offset":20480},"bus":{"length":4,"offset":20528},` +
		`"cloneId":"machine-ok.go-1.env-1","engineInstance":"go-1","envelopeId":"env-1","fanout":{"length":3,"offset":20512},` +
		`"generation":0,"input":{"length":4,"offset":20480},"machineId":"machine-ok","output":{"length":4,"offset":20496}}`
	if rec.Code != 201 || rec.Body.String() != want {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	// A retried dispatch is idempotent; another machine on the envelope is not.
	if again := raw(t, h, "POST", clone("?asset=asset%2F42&envelope=env-1"), ""); again.Code != 200 || again.Body.String() != want {
		t.Fatalf("%d %s", again.Code, again.Body.String())
	}
	if code, _ := send(t, h, "POST", "/api/engine/clone/machine/machine-handoff?asset=a&envelope=env-1", ""); code != 409 {
		t.Fatalf("envelope held by another machine: %d", code)
	}
	// Registered with its three fields, the template's name, its regions moved.
	id := "machine-ok.go-1.env-1"
	_, m, _ := get(t, h, "/api/machines/"+id)
	mach := m["machine"].(map[string]any)
	if mach["cloneOf"] != "machine-ok" || mach["assetId"] != "asset/42" || mach["envelopeId"] != "env-1" || mach["name"] != "Ok" {
		t.Fatalf("%v", mach)
	}
	if in := mach["perceptualMapping"].(map[string]any)["input"].(map[string]any); in["offset"] != 20480.0 || in["length"] != 4.0 {
		t.Fatalf("%v", in)
	}
	if _, tm, _ := get(t, h, "/api/machines/machine-ok"); tm["machine"].(map[string]any)["cloneOf"] != nil {
		t.Fatal("only clones carry cloneOf")
	}
	if code, b := send(t, h, "POST", "/api/engine/clone/machine/"+id+"?asset=a&envelope=x", ""); code != 400 || b["reason"] != "clone" {
		t.Fatalf("%d %v", code, b)
	}
	if _, _, listing := get(t, h, "/api/machines/cloneable"); strings.Contains(listing, "env-1") {
		t.Fatal("clones are not listed")
	}
	for _, method := range []string{"PUT", "PATCH", "DELETE"} {
		if code, _ := send(t, h, method, "/api/machines/"+id, `{"name": "x", "sequences": []}`); code != 409 {
			t.Fatalf("%s on a clone: %d", method, code)
		}
	}

	// Lowest free first; clone.maxActive bounds the template.
	if _, c2 := send(t, h, "POST", clone("?asset=a&envelope=env-2"), ""); c2["block"].(map[string]any)["index"] != 1.0 {
		t.Fatalf("%v", c2)
	}
	if code, _ := send(t, h, "PUT", "/api/engine/config/clone.maxActive", `{"value": 2}`); code != 200 {
		t.Fatalf("%d", code)
	}
	if code, _ := send(t, h, "PUT", "/api/engine/config/clone.maxActive", `{"value": 1025}`); code != 400 {
		t.Fatalf("out of range: %d", code)
	}
	if code, b := send(t, h, "POST", clone("?asset=a&envelope=env-3"), ""); code != 409 || b["error"] != "template is at clone.maxActive" {
		t.Fatalf("%d %v", code, b)
	}
	if _, cl := send(t, h, "GET", "/api/engine/clones", ""); len(cl["clones"].([]any)) != 2 {
		t.Fatalf("%v", cl)
	}

	// Recycle: 400, 404, 409 with the current generation, then 200.
	recycle := func(id, q string) string { return "/api/engine/recycle/machine/" + id + q }
	for _, q := range []string{"", "?generation=x", "?generation=1.0"} {
		if code, _ := send(t, h, "POST", recycle(id, q), ""); code != 400 {
			t.Fatalf("%q: %d", q, code)
		}
	}
	if code, _ := send(t, h, "POST", recycle("nope", "?generation=0"), ""); code != 404 {
		t.Fatalf("%d", code)
	}
	if r := raw(t, h, "POST", recycle(id, "?generation=1"), ""); r.Code != 409 || r.Body.String() != `{"error":"generation is not the clone's","generation":0}` {
		t.Fatalf("%d %s", r.Code, r.Body.String())
	}
	if r := raw(t, h, "POST", recycle(id, "?generation=0"), ""); r.Code != 200 ||
		r.Body.String() != `{"block":{"index":0,"length":64,"offset":20480},"cloneId":"`+id+`","generation":1}` {
		t.Fatalf("%d %s", r.Code, r.Body.String())
	}
	if code, _, _ := get(t, h, "/api/machines/"+id); code != 404 {
		t.Fatalf("recycled clone is unregistered: %d", code)
	}
	// A replayed recycle finds no clone; block 0 is reused at generation 1.
	if code, _ := send(t, h, "POST", recycle(id, "?generation=0"), ""); code != 404 {
		t.Fatalf("%d", code)
	}
	if code, c3 := send(t, h, "POST", clone("?asset=a&envelope=env-4"), ""); code != 201 ||
		c3["block"].(map[string]any)["index"] != 0.0 || c3["generation"] != 1.0 {
		t.Fatalf("%d %v", code, c3)
	}
	// DELETE restores the default bound.
	if _, c := send(t, h, "DELETE", "/api/engine/config/clone.maxActive", ""); c["value"] != 8.0 {
		t.Fatalf("%v", c)
	}
}

// Concurrent clones of one template take distinct blocks, lowest first, up to
// clone.maxActive, and no request sees a half-made clone.
func TestConcurrentClonesTakeDistinctBlocks(t *testing.T) {
	h := cloneCorpus(t, "go-1")
	var wg sync.WaitGroup
	codes := make([]int, 12)
	blocks := make([]any, 12)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			code, b := send(t, h, "POST", fmt.Sprintf("/api/engine/clone/machine/machine-ok?asset=a&envelope=e%d", i), "")
			codes[i], blocks[i] = code, nil
			if code == 201 {
				blocks[i] = b["block"].(map[string]any)["index"]
			}
		}(i)
	}
	wg.Wait()
	seen, created, refused := map[any]bool{}, 0, 0
	for i, code := range codes {
		switch code {
		case 201:
			created++
			if seen[blocks[i]] {
				t.Fatalf("block %v allocated twice", blocks[i])
			}
			seen[blocks[i]] = true
		case 409:
			refused++
		default:
			t.Fatalf("unexpected %d", code)
		}
	}
	if created != 8 || refused != 4 {
		t.Fatalf("created %d refused %d", created, refused)
	}
	for k := 0; k < 8; k++ {
		if !seen[float64(k)] {
			t.Fatalf("blocks are lowest-free-first: %v", seen)
		}
	}
}

func TestCloneWithoutInstanceIDIsRefused(t *testing.T) {
	h := cloneCorpus(t, "")
	if code, b := send(t, h, "POST", "/api/engine/clone/machine/machine-ok?asset=a&envelope=e", ""); code != 409 ||
		b["error"] != "engine launched without an INSTANCE_ID" {
		t.Fatalf("%d %v", code, b)
	}
}

// raw is a request's recorded response, for byte comparisons.
func raw(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
	return rec
}
