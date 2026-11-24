package engine

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/jateeter/RealityEngine_GO/internal/machine"
)

// CES coverage and GET /api/metrics (RealityEngine_GO#2, slice C), as C++'s
// CesCoverageRegistry keeps and exposes it: counters since the engine started,
// recorded at every step (perceive or simulation) for each uninhibited
// machine; the process routes do not record, as on C++. A reset does not
// clear them: coverage records what the corpus has been shown to do, and a
// reset does not un-show it. /api/metrics is never compared byte for byte
// (SURFACE_SPEC "phaseDetail"); the metric names, HELP text and labels are
// the ones the three engines share, so one scrape config covers every runtime.

type coverage struct {
	startedMS  int64
	steps      map[[2]string]int // (machine id, name) -> transitions
	matched    map[[4]string]int // (id, name, sequence, event)
	activated  map[[4]string]int
	outputs    map[[3]string]int // (id, name, sequence) -> asserted outputs
	paging     map[[4]string]int // (owner team, process status, RAG, machine id)
	deprecated map[[4]string]int // (id, name, sequence, replaced by)
}

func newCoverage() coverage {
	return coverage{
		startedMS: clock.NowMS(), steps: map[[2]string]int{}, matched: map[[4]string]int{},
		activated: map[[4]string]int{}, outputs: map[[3]string]int{}, paging: map[[4]string]int{},
		deprecated: map[[4]string]int{},
	}
}

// record counts one transition result of m. The caller holds the write lock.
func (c *coverage) record(m *machine.Machine, result map[string]any) {
	if m.TransitionsInhibited {
		return
	}
	c.steps[[2]string{m.ID, m.Name}]++
	meta, _ := result["arbiterMetadata"].(map[string]any)
	shouldOutput, _ := meta["shouldOutput"].(bool)
	seqs, _ := result["sequenceResults"].(map[string]any)
	for seq, rv := range seqs {
		r, _ := rv.(map[string]any)
		matched, _ := r["matchedEvents"].([]string)
		for _, ev := range matched {
			c.matched[[4]string{m.ID, m.Name, seq, ev}]++
		}
		activated, _ := r["activatedEvents"].([]string)
		for _, ev := range activated {
			c.activated[[4]string{m.ID, m.Name, seq, ev}]++
		}
		asserted, _ := r["assertedOutputs"].([]any)
		if shouldOutput && len(asserted) > 0 {
			c.outputs[[3]string{m.ID, m.Name, seq}] += len(asserted)
		}
	}
}

func sortedKeysOf[K interface{ comparable }](m map[K]int, less func(a, b K) bool) []K {
	keys := make([]K, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return less(keys[i], keys[j]) })
	return keys
}

func lessStrings(a, b []string) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

func escapeLabel(v string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(v)
}

func labels(pairs ...string) string {
	var b strings.Builder
	b.WriteString(`{runtime="go"`)
	for i := 0; i+1 < len(pairs); i += 2 {
		fmt.Fprintf(&b, `,%s="%s"`, pairs[i], escapeLabel(pairs[i+1]))
	}
	b.WriteString("}")
	return b.String()
}

// Metrics is GET /api/metrics: the CES coverage exposition, then the runtime
// gauges, in Prometheus text format.
func (e *Engine) Metrics() string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	var out strings.Builder
	header := func(name, help, kind string) {
		fmt.Fprintf(&out, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, kind)
	}
	gauge := func(name, help, value string) {
		header(name, help, "gauge")
		fmt.Fprintf(&out, "%s%s %s\n", name, labels(), value)
	}
	ids := e.machineIDs()
	sequences, events := 0, 0
	for _, m := range e.machines {
		sequences += len(m.Sequences)
		for _, s := range m.Sequences {
			events += len(s.Events)
		}
	}
	gauge("ces_machines_total", "Number of machines loaded into the reality engine.", strconv.Itoa(len(e.machines)))
	gauge("ces_sequences_total", "Number of sequences across all registered machines.", strconv.Itoa(sequences))
	gauge("ces_vectors_total", "Number of event vectors across all registered machines.", strconv.Itoa(events))

	c := e.coverage
	four := func(a, b [4]string) bool { return lessStrings(a[:], b[:]) }
	header("ces_vector_matched_total", "Number of times a vector matched its input during a transition phase.", "counter")
	for _, k := range sortedKeysOf(c.matched, four) {
		fmt.Fprintf(&out, "ces_vector_matched_total%s %d\n", labels("machine", k[1], "machine_id", k[0], "sequence", k[2], "vector", k[3]), c.matched[k])
	}
	header("ces_vector_activated_total", "Number of times a vector was activated as a successor in a transition.", "counter")
	for _, k := range sortedKeysOf(c.activated, four) {
		fmt.Fprintf(&out, "ces_vector_activated_total%s %d\n", labels("machine", k[1], "machine_id", k[0], "sequence", k[2], "vector", k[3]), c.activated[k])
	}
	header("ces_sequence_outputs_total", "Number of asserted outputs emitted by a sequence.", "counter")
	for _, k := range sortedKeysOf(c.outputs, func(a, b [3]string) bool { return lessStrings(a[:], b[:]) }) {
		fmt.Fprintf(&out, "ces_sequence_outputs_total%s %d\n", labels("machine", k[1], "machine_id", k[0], "sequence", k[2]), c.outputs[k])
	}
	header("ces_machine_steps_total", "Number of process_input calls observed for this machine.", "counter")
	stepped := map[string]bool{}
	for _, k := range sortedKeysOf(c.steps, func(a, b [2]string) bool { return lessStrings(a[:], b[:]) }) {
		stepped[k[0]] = true
		fmt.Fprintf(&out, "ces_machine_steps_total%s %d\n", labels("machine", k[1], "machine_id", k[0]), c.steps[k])
	}
	header("ces_paging_decisions_total", "Number of governance-resolved paging decisions issued by the engine.", "counter")
	for _, k := range sortedKeysOf(c.paging, four) {
		name := ""
		if m, ok := e.machines[k[3]]; ok {
			name = m.Name
		}
		fmt.Fprintf(&out, "ces_paging_decisions_total%s %d\n", labels("owner_team", k[0], "process_status", k[1], "rag_status_code", k[2], "machine_id", k[3], "machine", name), c.paging[k])
	}
	header("ces_deprecated_fires_total", "Number of times a deprecated sequence emitted output.", "counter")
	for _, k := range sortedKeysOf(c.deprecated, four) {
		fmt.Fprintf(&out, "ces_deprecated_fires_total%s %d\n", labels("machine", k[1], "machine_id", k[0], "sequence", k[2], "replaced_by", k[3]), c.deprecated[k])
	}
	// Zero baselines, so a dashboard can plot every machine before it fires.
	for _, id := range ids {
		m := e.machines[id]
		l := labels("machine", m.Name, "machine_id", id)
		fmt.Fprintf(&out, "ces_vector_matched_total%s 0\nces_vector_activated_total%s 0\nces_sequence_outputs_total%s 0\nces_deprecated_fires_total%s 0\n", l, l, l, l)
		if !stepped[id] {
			fmt.Fprintf(&out, "ces_machine_steps_total%s 0\n", l)
		}
	}
	perMachine := func(name, help string, value func(*machine.Machine) int) {
		header(name, help, "gauge")
		for _, id := range ids {
			m := e.machines[id]
			fmt.Fprintf(&out, "%s%s %d\n", name, labels("machine", m.Name, "machine_id", id), value(m))
		}
	}
	perMachine("ces_unfired_sequences", "Number of sequences in this machine that have never emitted output.", func(m *machine.Machine) int {
		n := 0
		for seq := range m.Sequences {
			if c.outputs[[3]string{m.ID, m.Name, seq}] == 0 {
				n++
			}
		}
		return n
	})
	perMachine("ces_unfired_vectors", "Number of vectors in this machine that have never matched or activated.", func(m *machine.Machine) int {
		n := 0
		for seq, s := range m.Sequences {
			for ev := range s.Events {
				k := [4]string{m.ID, m.Name, seq, ev}
				if c.matched[k] == 0 && c.activated[k] == 0 {
					n++
				}
			}
		}
		return n
	})
	perMachine("ces_machine_sequence_count", "Total sequences declared by this machine.", func(m *machine.Machine) int { return len(m.Sequences) })
	perMachine("ces_machine_vector_count", "Total vectors declared by this machine.", func(m *machine.Machine) int {
		n := 0
		for _, s := range m.Sequences {
			n += len(s.Events)
		}
		return n
	})
	gauge("ces_registry_uptime_seconds", "Seconds since the coverage registry was instantiated.",
		strconv.FormatFloat(float64(clock.NowMS()-c.startedMS)/1000, 'f', -1, 64))
	gauge("re_runtime_dimension", "Current dimension of the shared perceptual space.", strconv.Itoa(e.dimension))
	gauge("re_runtime_required_dimension", "Max(offset+length) across all registered machine mappings.", strconv.Itoa(e.requiredDimension()))
	gauge("re_runtime_mapping_version", "Monotonic version bumped on every add/remove_machine.", strconv.Itoa(e.mappingVersion))
	if e.steps.controls.phaseDetail {
		header("re_step_phase_seconds_total", "Cumulative wall clock per phase of a step.", "counter")
		for i, p := range stepPhases {
			fmt.Fprintf(&out, "re_step_phase_seconds_total%s %.9f\n", labels("phase", p), e.steps.phaseTime[i].Seconds())
		}
		header("re_step_phase_detail_steps_total", "Steps the universal step phases were measured over.", "counter")
		fmt.Fprintf(&out, "re_step_phase_detail_steps_total%s %d\n", labels(), e.steps.measuredSteps)
	}
	detail := "0"
	if e.steps.controls.phaseDetail {
		detail = "1"
	}
	gauge("re_step_phase_detail", "Whether step phase timing (phaseDetail) is on.", detail)
	return out.String()
}
