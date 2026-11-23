package machine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

var fixedClock = Clock{NowMS: func() int64 { return 1000 }, MintID: func() string { return "x" }}

func mustParse(t *testing.T, doc string) *Machine {
	t.Helper()
	m, err := Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// A two-event chain: a (initial) -> b, b asserts an output.
const chain = `{"machine": {"name": "Chain", "arbiterRule": "passthrough",
  "perceptualMapping": {"input": {"offset": 10, "length": 2}, "output": {"offset": 12, "length": 2}},
  "sequences": [{"id": "s1", "name": "one", "events": [
    {"id": "a", "isInitial": true, "elements": [{"value": 1}, {"value": 0}], "nextEventIds": ["b"]},
    {"id": "b", "elements": [{"value": 0}, {"value": 1}], "outputEvents": [{"id": "o", "vector": [1, 0]}]}
  ]}]}}`

func TestCanonicalID(t *testing.T) {
	for stem, want := range map[string]string{
		"AGX001_aquaculture-water-quality-stability": "machine-agx001-aquaculture-water-quality-stability",
		"HomeTransportationBarrierMonitor":           "machine-hometransportationbarriermonitor",
		"a.b c":                                      "machine-a-b-c",
		"é1":                                         "machine--1",
	} {
		if got := CanonicalID(stem); got != want {
			t.Errorf("CanonicalID(%q) = %q, want %q", stem, got, want)
		}
	}
}

func TestParseDefaultsAndRegion(t *testing.T) {
	m := mustParse(t, chain)
	if m.Name != "Chain" || m.InputOffset != 10 || m.InputLength != 2 {
		t.Fatalf("parsed %+v", m)
	}
	if m.MatchAlgorithm != "gte" || m.OutputMerge != "or" || m.ArbiterRule != "passthrough" {
		t.Fatalf("defaults: %s %s %s", m.MatchAlgorithm, m.OutputMerge, m.ArbiterRule)
	}
	// Without a declared input length, the first event's width.
	m = mustParse(t, strings.Replace(chain, `"input": {"offset": 10, "length": 2}`, `"input": {"offset": 3}`, 1))
	if m.InputLength != 2 {
		t.Fatalf("input length = %d, want the first event's width 2", m.InputLength)
	}
	if !m.Sequences["s1"].Events["a"].Active || m.Sequences["s1"].Events["b"].Active {
		t.Fatal("only the initial event starts active")
	}
}

func TestParseRefusals(t *testing.T) {
	for name, tc := range map[string]struct{ doc, want string }{
		"not json":          {`{`, "not valid JSON"},
		"not an object":     {`[1]`, "must be a JSON object"},
		"empty name":        {`{"name": " ", "sequences": []}`, "machine.name is required"},
		"version 2":         {`{"version": "2.0", "machine": {"name": "x"}}`, "unsupported machine version: 2.0"},
		"sequence id":       {`{"name": "x", "sequences": [{"events": [{"id": "a"}]}]}`, "non-empty id"},
		"no events":         {`{"name": "x", "sequences": [{"id": "s", "events": []}]}`, "sequence s has no events"},
		"retired vectors":   {`{"name": "x", "sequences": [{"id": "s", "vectors": [], "events": [{"id": "a"}]}]}`, "retired key `vectors`; the canonical key is `events`"},
		"retired outputs":   {`{"name": "x", "sequences": [{"id": "s", "events": [{"id": "a", "outputVectors": []}]}]}`, "retired key `outputVectors`"},
		"retired next ids":  {`{"name": "x", "sequences": [{"id": "s", "events": [{"id": "a", "nextVectorIds": []}]}]}`, "retired key `nextVectorIds`"},
		"retired input seq": {`{"name": "x", "inputSequences": [{"vectors": []}], "sequences": []}`, "inputSequences[0] uses the retired key"},
		"duplicate event":   {`{"name": "x", "sequences": [{"id": "s", "events": [{"id": "a"}, {"id": "a"}]}]}`, "duplicate event id a in sequence s"},
		"duplicate seq":     {`{"name": "x", "sequences": [{"id": "s", "events": [{"id": "a"}]}, {"id": "s", "events": [{"id": "a"}]}]}`, "duplicate sequence id s"},
	} {
		_, err := Parse([]byte(tc.doc))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error = %v, want %q", name, err, tc.want)
		}
		if e, ok := err.(*Error); err != nil && (!ok || e.Status != 400) {
			t.Errorf("%s: not a 400 *Error: %#v", name, err)
		}
	}
}

func TestComparators(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	for name, tc := range map[string]struct {
		el        Element
		algorithm string
		input     float64
		want      bool
	}{
		"gte on":                {Element{Value: 1}, "gte", 0.7, true},
		"gte off must be off":   {Element{Value: 0}, "gte", 0.7, false},
		"gte off matches off":   {Element{Value: 0}, "gte", 0.2, true},
		"gte own threshold":     {Element{Value: 1, Threshold: f(0.8)}, "gte", 0.7, false},
		"equals":                {Element{Value: 0.25, Comparator: "equals"}, "gte", 0.25, true},
		"equals misses":         {Element{Value: 0.25, Comparator: "equals"}, "gte", 0.2500001, false},
		"custom is equals":      {Element{Value: 3, Comparator: "custom"}, "gte", 3, true},
		"threshold within 0.1":  {Element{Value: 0.5, Comparator: "threshold"}, "gte", 0.6, true},
		"threshold outside":     {Element{Value: 0.5, Comparator: "threshold"}, "gte", 0.61, false},
		"pattern default":       {Element{Value: 1, Comparator: "pattern"}, "gte", 0.5, true},
		"pattern misses":        {Element{Value: 1, Comparator: "pattern"}, "gte", 0.4, false},
		"machine algorithm":     {Element{Value: 0.3}, "equals", 0.3, true},
		"element beats machine": {Element{Value: 1, Comparator: "gte"}, "equals", 0.9, true},
	} {
		e := &Event{Elements: []Element{tc.el}}
		if got := e.Matches([]float64{tc.input}, tc.algorithm, ""); got != tc.want {
			t.Errorf("%s: Matches = %v, want %v", name, got, tc.want)
		}
	}
	e := &Event{Elements: []Element{{Value: 1, Comparator: "gte"}}}
	if !e.Matches([]float64{1}, "gte", "") || e.Matches([]float64{0.99}, "gte", "equals") {
		t.Error("an override applies above the element's comparator")
	}
	if e.Matches([]float64{1, 1}, "gte", "") {
		t.Error("a wrong-width input never matches")
	}
}

func step(m *Machine, input ...float64) map[string]any {
	return m.Transition(input, "", fixedClock)
}

func seqResult(r map[string]any, seq string) map[string]any {
	return r["sequenceResults"].(map[string]any)[seq].(map[string]any)
}

func TestTransitionDefersActivationAndKeepsProvenance(t *testing.T) {
	m := mustParse(t, chain)
	m.ID = "machine-chain"

	// Step 1: a matches; b is activated after the sequence is tried, so the
	// same input does not also try b.
	r := step(m, 1, 0)
	s := seqResult(r, "s1")
	if !reflect.DeepEqual(s["matchedEvents"], []string{"a"}) || !reflect.DeepEqual(s["activatedEvents"], []string{"b"}) {
		t.Fatalf("step 1: %v", s)
	}
	if r["machineOutput"] != nil || r["mergedOutput"] != nil {
		t.Fatalf("step 1 asserted output: %v", r)
	}
	// a is initial, so it stays active; b now carries a as its predecessor.
	if !m.Sequences["s1"].Events["a"].Active || !reflect.DeepEqual(m.Sequences["s1"].Events["b"].PredecessorChain, []string{"a"}) {
		t.Fatal("after step 1")
	}

	// Step 2: b matches and asserts, with provenance a -> b.
	r = step(m, 0, 1)
	s = seqResult(r, "s1")
	out := s["assertedOutputs"].([]any)[0].(map[string]any)
	if !reflect.DeepEqual(out["provenance"], []string{"a", "b"}) || !reflect.DeepEqual(out["vector"], []float64{1, 0}) {
		t.Fatalf("step 2 output: %v", out)
	}
	pick := r["machineOutput"].(map[string]any)
	if pick["id"] != "machine-output-x" || !reflect.DeepEqual(pick["metadata"].(map[string]any)["sources"], []string{"o"}) {
		t.Fatalf("pick: %v", pick)
	}
	if !reflect.DeepEqual(r["mergedOutput"], []float64{1, 0}) {
		t.Fatalf("merged: %v", r["mergedOutput"])
	}
	if !m.Sequences["s1"].Events["b"].WasJustMatched {
		t.Fatal("an event that asserted is wasJustMatched")
	}

	// Step 3: nothing matches; the non-initial b is deactivated, a stays.
	step(m, 0, 0)
	if m.Sequences["s1"].Events["b"].Active || !m.Sequences["s1"].Events["a"].Active {
		t.Fatal("after an unmatched step")
	}

	// Reset returns every event to its loaded state.
	step(m, 1, 0)
	m.Reset()
	b := m.Sequences["s1"].Events["b"]
	if b.Active || b.WasJustMatched || b.PredecessorChain != nil {
		t.Fatalf("after reset: %+v", b)
	}
}

func TestHandOffDeactivatesAnEventWithoutOutputs(t *testing.T) {
	m := mustParse(t, `{"name": "Hand", "sequences": [{"id": "s", "events": [
	  {"id": "a", "isInitial": true, "elements": [{"value": 1}], "nextEventIds": ["b"]},
	  {"id": "b", "elements": [{"value": 0}], "nextEventIds": ["c"]},
	  {"id": "c", "elements": [{"value": 1}], "outputEvents": [{"vector": [1]}]}]}]}`)
	step(m, 1) // a matches: b is activated
	step(m, 0) // only b matches: it has no outputs, so it hands off to c and is deactivated
	ev := m.Sequences["s"].Events
	if ev["b"].Active || !ev["c"].Active || !ev["a"].Active {
		t.Fatalf("a %v b %v c %v", ev["a"].Active, ev["b"].Active, ev["c"].Active)
	}
	r := step(m, 1) // a and c match; c asserts with provenance a -> b -> c
	out := seqResult(r, "s")["assertedOutputs"].([]any)[0].(map[string]any)
	if out["id"] != "c-output-0" || !reflect.DeepEqual(out["provenance"], []string{"a", "b", "c"}) {
		t.Fatalf("output: %v", out)
	}
}

// An initial predecessor that matches again in the hand-off step re-activates
// its successor: activation is deferred until every active event was tried.
func TestDeferredActivationReactivatesAHandedOffSuccessor(t *testing.T) {
	m := mustParse(t, `{"name": "Re", "sequences": [{"id": "s", "events": [
	  {"id": "a", "isInitial": true, "elements": [{"value": 1}], "nextEventIds": ["b"]},
	  {"id": "b", "elements": [{"value": 1}], "nextEventIds": ["c"]},
	  {"id": "c", "elements": [{"value": 1}]}]}]}`)
	step(m, 1)
	r := step(m, 1)
	if got := seqResult(r, "s")["activatedEvents"]; !reflect.DeepEqual(got, []string{"b", "c"}) {
		t.Fatalf("activated %v, want [b c]", got)
	}
}

func TestArbiterRules(t *testing.T) {
	two := func(rule string) *Machine {
		return mustParse(t, `{"name": "Two", "arbiterRule": "`+rule+`", "sequences": [
		  {"id": "s1", "events": [{"id": "a", "isInitial": true, "elements": [{"value": 1}], "outputEvents": [{"id": "o1", "vector": [1, 0]}]}]},
		  {"id": "s2", "events": [{"id": "a", "isInitial": true, "elements": [{"value": 0}], "outputEvents": [{"id": "o2", "vector": [0, 1]}]}]}]}`)
	}
	for rule, want := range map[string]bool{"and": false, "or": true, "passthrough": true} {
		r := step(two(rule), 1)
		meta := r["arbiterMetadata"].(map[string]any)
		if meta["shouldOutput"] != want || meta["sequencesWithOutput"] != 1 || meta["totalInputs"] != 2 {
			t.Errorf("%s: %v", rule, meta)
		}
	}
}

func TestInhibitedAcceptsAndDoesNotPassForward(t *testing.T) {
	m := mustParse(t, chain)
	m.TransitionsInhibited = true
	r := m.Respond([]float64{1, 0}, "", fixedClock)
	if seqResult(r, "s1")["matchedEvents"] == nil || len(seqResult(r, "s1")["matchedEvents"].([]string)) != 0 {
		t.Fatalf("inhibited matched: %v", r)
	}
	if r["arbiterMetadata"].(map[string]any)["totalInputs"] != 0 || m.Sequences["s1"].Events["b"].Active {
		t.Fatal("an inhibited machine changed state or reported inputs")
	}
}

func TestFold(t *testing.T) {
	top := 3
	vs := [][]float64{{1, 0, 2.9999999996, -1}, {1, 1, 1}}
	for name, tc := range map[string]struct {
		transform string
		top       *int
		want      []float64
	}{
		"or":                 {"or", nil, []float64{1, 1, 1, 1}},
		"and":                {"and", nil, []float64{1, 0, 1, 0}},
		"xor":                {"xor", nil, []float64{0, 1, 0, 1}},
		"nor":                {"nor", nil, []float64{0, 0, 0, 0}},
		"nand":               {"nand", nil, []float64{0, 1, 0, 1}},
		"meet":               {"meet", nil, []float64{1, 0, 1, 0}},
		"join rounds":        {"join", nil, []float64{1, 1, 3, 0}},
		"join clamps to top": {"join", &[]int{2}[0], []float64{1, 1, 2, 0}},
		"discrete-median":    {"discrete-median", nil, []float64{1, 0, 1, 0}},
		"strong-disjunction": {"strong-disjunction", &top, []float64{2, 1, 3, 0}},
		"strong-conjunction": {"strong-conjunction", &top, []float64{0, 0, 1, 0}},
	} {
		got, ok := Fold(vs, tc.transform, tc.top)
		if !ok || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: %v %v, want %v", name, got, ok, tc.want)
		}
	}
	if _, ok := Fold(vs, "strong-conjunction", nil); ok {
		t.Error("a strong fold without a chain top folds nothing")
	}
	if _, ok := Fold(nil, "or", nil); ok {
		t.Error("nothing asserted folds nothing")
	}
}

// Numbers leave in ECMAScript form, as the voters write them.
func TestResultNumbersAreECMAScript(t *testing.T) {
	m := mustParse(t, chain)
	step(m, 1, 0)
	body, err := json.Marshal(step(m, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"inputEvent":[0,1]`) || strings.Contains(string(body), "1.0") {
		t.Fatalf("body: %s", body)
	}
}

func TestLoadDirIsRecursiveAndNamesFailures(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, body string) {
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("Top_One.json", `{"id": "declared", "machine": {"name": "One", "sequences": []}}`)
	write("domains/health/Deep.json", `{"name": "Deep", "sequences": []}`)
	write("domains/health/Bad.json", `{"name": "Bad", "sequences": [{"id": "s", "vectors": [], "events": [{"id": "a"}]}]}`)
	write("notes.txt", `not a machine`)
	loaded, failed, err := LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, l := range loaded {
		got = append(got, l.Machine.ID+"@"+l.RelFile+"@"+l.Machine.SourceFile)
	}
	want := []string{"machine-top-one@Top_One.json@Top_One.json", "machine-deep@domains/health/Deep.json@Deep.json"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("loaded %v, want %v", got, want)
	}
	if len(failed) != 1 || failed[0].RelFile != "domains/health/Bad.json" || !strings.Contains(failed[0].Error(), "retired key") {
		t.Fatalf("failed %v", failed)
	}
}

// The real corpus loads, when a RealityEngine_Machines checkout is beside this
// one (local only; a hosted checkout has none and skips).
func TestTheCorpusLoads(t *testing.T) {
	dir := filepath.Join("..", "..", "..", "RealityEngine_Machines", "machines")
	if _, err := os.Stat(dir); err != nil {
		t.Skip("no RealityEngine_Machines checkout beside this one")
	}
	loaded, failed, err := LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("corpus: %d machines loaded, %d files not loaded", len(loaded), len(failed))
	for i, f := range failed {
		if i < 10 {
			t.Logf("  not loaded: %v", f)
		}
	}
	if len(loaded) == 0 {
		t.Fatal("no machine loaded from the corpus")
	}
	for _, l := range loaded {
		if l.Machine.InputLength < 0 {
			t.Fatalf("%s: input length %d", l.RelFile, l.Machine.InputLength)
		}
	}
}
