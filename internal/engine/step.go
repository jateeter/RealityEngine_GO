package engine

import (
	"fmt"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jateeter/RealityEngine_GO/internal/machine"
)

// The step path (RealityEngine_GO#13, slice 2): POST /api/perceive, the
// trajectory histories, step completion and arbitration. SURFACE_SPEC.md,
// "Trajectory histories" and "Step completion", governs; C++
// PerceptualSpaceRuntime::run_phases is the concrete form, and Rust's step.rs
// prior art. One step:
//
//  1. ISRE(n) is captured, before any machine reads its input.
//  2. Every machine composes from ISRE(n) in parallel; the workers are joined
//     before anything resolves OSRE.
//  3. Each machine's completed outputs fold under its declared transformation
//     into one merge operation, carrying the governance its contributors
//     resolve to (the highest severity wins) and any deprecation mark; the
//     batch is ordered by machineName, then region.offset.
//  4. A cell one machine writes is stored as written; a contended cell
//     resolves under its arbitration-registry.json rule. OSRE(n) is observed
//     at that commit.
//  5. The event bus: a firing a meta-machine subscribes to
//     (metadata.compose) writes 1 at the subscribed bit, which latches into
//     every later step's input until a reset.
//  6. The (ISRE, OSRE) pair is appended in one action, the instance clock
//     ticks, and the completion is published, waking every waiter for step n.

const (
	trajectoryCapacity    = 1024
	pairTimeoutDefaultMS  = 5000
	pairTimeoutMaxMS      = 60000
	historyLimitDefault   = 250
	arbitrationWindowInit = 1
)

// stepControls are the engine-scoped step controls (/api/engine/config, #2).
type stepControls struct {
	historyLimit           int
	includeActiveRegions   bool
	includeMachineResults  bool
	includePerceptualSpace bool
	arbitrationRetention   bool
	arbitrationWindow      int
	phaseDetail            bool
}

func defaultControls() stepControls {
	return stepControls{
		historyLimit: historyLimitDefault, includeActiveRegions: true, includeMachineResults: true,
		includePerceptualSpace: true, arbitrationWindow: arbitrationWindowInit,
	}
}

// trajectoryEntry is one dense vector, sparse and lossless.
type trajectoryEntry struct {
	stepNumber int
	length     int
	nonZero    []cellValue
}

type cellValue struct {
	index int
	value float64
}

func sparse(step int, dense []float64) trajectoryEntry {
	e := trajectoryEntry{stepNumber: step, length: len(dense)}
	for i, v := range dense {
		if v != 0 {
			e.nonZero = append(e.nonZero, cellValue{i, v})
		}
	}
	return e
}

func (t trajectoryEntry) json() map[string]any {
	cells := make([]any, len(t.nonZero))
	for i, c := range t.nonZero {
		cells[i] = map[string]any{"index": c.index, "value": c.value}
	}
	return map[string]any{"stepNumber": t.stepNumber, "length": t.length, "nonZero": cells}
}

type retainedStep struct {
	lamport uint64
	records []arbitrationRecord
}

// stepState is the perceptual space, the two histories, and the completion
// point observers wait on. It lives under the engine's lock.
type stepState struct {
	space    []float64
	nextStep int
	isre     []trajectoryEntry
	osre     []trajectoryEntry
	// published is closed when a step commits, and replaced: a waiter takes
	// the current one under the lock and waits on it without.
	published chan struct{}
	// latched event-bus bits, re-applied over every later input until a reset.
	latched         map[int]bool
	history         []*step // newest first (perceptual-simulation history, #2)
	controls        stepControls
	lastArbitration []arbitrationRecord
	retained        map[int]retainedStep
	arbitration     arbitrationRegistry
	// phaseTime sums each step phase over the measured steps, counters since
	// boot that a reset does not clear.
	phaseTime     [5]time.Duration
	measuredSteps uint64
}

func newStepState(machinesDir string) stepState {
	return stepState{
		published: make(chan struct{}), latched: map[int]bool{}, controls: defaultControls(),
		retained: map[int]retainedStep{}, arbitration: loadArbitrationRegistry(machinesDir),
	}
}

// reset: step numbers restart, so the completion point does too; the space
// and every history empty. The clock keeps its value.
func (s *stepState) reset() {
	for i := range s.space {
		s.space[i] = 0
	}
	s.nextStep = 0
	s.isre, s.osre, s.history = nil, nil, nil
	s.lastArbitration = nil
	s.retained = map[int]retainedStep{}
	s.latched = map[int]bool{}
	s.publish()
}

func (s *stepState) publish() {
	close(s.published)
	s.published = make(chan struct{})
}

func (s *stepState) commit(isre, osre trajectoryEntry) {
	if len(s.isre) == trajectoryCapacity {
		s.isre = s.isre[1:]
	}
	if len(s.osre) == trajectoryCapacity {
		s.osre = s.osre[1:]
	}
	s.isre = append(s.isre, isre)
	s.osre = append(s.osre, osre)
}

func (s *stepState) retainArbitration(step int, lamport uint64, records []arbitrationRecord) {
	if s.controls.arbitrationRetention && s.controls.arbitrationWindow > 0 {
		s.retained[step] = retainedStep{lamport, records}
		first := step - s.controls.arbitrationWindow + 1
		for n := range s.retained {
			if n < first {
				delete(s.retained, n)
			}
		}
	}
}

// Selector is POST /api/perceive's only: keep what is attributable to these
// sequences or these machine names. Present, it selects, even when it names
// nothing.
type Selector struct {
	sequenceIDs  map[string]bool
	machineNames map[string]bool
}

// SelectorFrom is the selector a body's only declares; nil when absent, null
// or not an object, which is no selector at all.
func SelectorFrom(v any) *Selector {
	only, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	strings := func(key string) map[string]bool {
		out := map[string]bool{}
		l, _ := only[key].([]any)
		for _, x := range l {
			if s, ok := x.(string); ok {
				out[s] = true
			}
		}
		return out
	}
	return &Selector{sequenceIDs: strings("sequenceIds"), machineNames: strings("machineNames")}
}

func (s *Selector) wantsSequences(ids []string) bool {
	for _, id := range ids {
		if s.sequenceIDs[id] {
			return true
		}
	}
	return false
}

type mergeOperation struct {
	offset, length int
	machineID      string
	machineName    string
	sequenceIDs    []string
	values         []float64
	provenance     []string
	governance     map[string]any
	ragStatusCode  string
	deprecation    map[string]any
}

func (op mergeOperation) json() map[string]any {
	out := map[string]any{
		"region":    map[string]any{"offset": op.offset, "length": op.length},
		"machineId": op.machineID, "machineName": op.machineName,
		"sequenceIds": op.sequenceIDs, "values": op.values, "provenance": op.provenance,
	}
	if op.governance != nil {
		out["governance"] = op.governance
	}
	if op.deprecation != nil {
		out["deprecation"] = op.deprecation
	}
	return out
}

type busWrite struct {
	subscriber string
	bit        int
	producer   string
	sequenceID string
	provenance []string
}

type activeRegion struct {
	offset, length int
	machineID      string
	kind           string
}

// step is a committed step, before the caller's include flags shape it.
type step struct {
	number         int
	timestamp      int64
	machineResults map[string]map[string]any
	activeRegions  []activeRegion
	mergeBatch     []mergeOperation
	eventBus       []busWrite
	space          []float64
}

// JSON is the step as POST /api/perceive answers it (C++
// to_json(SimulationStep)): a field not asked for is omitted, never emptied.
// With a selector each list keeps only what it attributes to the selection,
// and entries are skipped before they are copied.
func (s *step) JSON(machineResults, perceptualSpace, activeRegions bool, only *Selector) map[string]any {
	selected := map[string]bool{}
	if only != nil {
		for id, r := range s.machineResults {
			if name, _ := r["machineName"].(string); only.machineNames[name] {
				selected[id] = true
			}
		}
	}
	merges := []any{}
	for _, op := range s.mergeBatch {
		if only == nil || only.wantsSequences(op.sequenceIDs) || selected[op.machineID] {
			merges = append(merges, op.json())
		}
	}
	bus := []any{}
	for _, w := range s.eventBus {
		if only == nil || only.sequenceIDs[w.sequenceID] || selected[w.producer] || selected[w.subscriber] {
			bus = append(bus, map[string]any{
				"producerMachineId": w.producer, "producerSequenceId": w.sequenceID,
				"subscriberMachineId": w.subscriber, "bitOffset": w.bit, "value": 1.0,
				"provenance": w.provenance,
			})
		}
	}
	out := map[string]any{"stepNumber": s.number, "timestamp": s.timestamp, "mergeBatch": merges, "eventBus": bus}
	if activeRegions {
		regions := []any{}
		for _, r := range s.activeRegions {
			if only == nil || selected[r.machineID] {
				regions = append(regions, map[string]any{"offset": r.offset, "length": r.length, "machineId": r.machineID, "type": r.kind})
			}
		}
		out["activeRegions"] = regions
	}
	if perceptualSpace {
		out["perceptualSpace"] = s.space
		out["perceptualSpaceIsDebugProjection"] = true
	}
	if machineResults {
		results := map[string]any{}
		for id, r := range s.machineResults {
			keep := only == nil || selected[id]
			if !keep {
				for _, op := range s.mergeBatch {
					if op.machineID == id && only.wantsSequences(op.sequenceIDs) {
						keep = true
						break
					}
				}
			}
			if keep {
				results[id] = r
			}
		}
		out["machineResults"] = results
	}
	return out
}

// subscriptions are every meta-machine's metadata.compose.subscriptions,
// keyed by producer machine id and sequence id.
type subscriber struct {
	machineID string
	bit       int
}

func (e *Engine) subscriptions() map[[2]string][]subscriber {
	out := map[[2]string][]subscriber{}
	for _, id := range e.machineIDs() {
		meta, _ := e.machines[id].Raw["metadata"].(map[string]any)
		compose, _ := meta["compose"].(map[string]any)
		subs, _ := compose["subscriptions"].([]any)
		for _, sv := range subs {
			sub, _ := sv.(map[string]any)
			producer, _ := sub["producerMachineId"].(string)
			sequence, _ := sub["producerSequenceId"].(string)
			bit, ok := sub["bitOffset"].(float64)
			if !ok || bit < 0 || bit != float64(int(bit)) || producer == "" || sequence == "" {
				continue
			}
			key := [2]string{producer, sequence}
			out[key] = append(out[key], subscriber{id, int(bit)})
		}
	}
	return out
}

func (e *Engine) machineIDs() []string {
	ids := make([]string, 0, len(e.machines))
	for id := range e.machines {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// requiredDimension is max(offset + length) over every machine's regions.
func (e *Engine) requiredDimension() int {
	required := 0
	for _, m := range e.machines {
		if input, output, ok := m.Regions(); ok {
			required = max(required, input.Offset+input.Length, output.Offset+output.Length)
		}
	}
	return required
}

// spaceWidth is the perceptual space's width: the dimension, grown to every
// machine's mapping, every subscribed event-bus bit, and what has been
// presented.
func (e *Engine) spaceWidth() int {
	width := max(e.dimension, e.requiredDimension(), len(e.steps.space))
	for _, subs := range e.subscriptions() {
		for _, s := range subs {
			width = max(width, s.bit+1)
		}
	}
	return width
}

// Perceive is POST /api/perceive: input becomes the space, the corpus is
// presented with it, and the committed step is answered as the flags and
// selector shape it.
func (e *Engine) Perceive(body map[string]any) (map[string]any, error) {
	override := ""
	if name, ok := body["matchAlgorithmOverride"].(string); ok {
		c, err := machine.Comparator(name)
		if err != nil {
			return nil, err
		}
		override = c
	}
	only := SelectorFrom(body["only"])
	flag := func(key string, def bool) bool {
		if b, ok := body[key].(bool); ok {
			return b
		}
		return def
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	c := e.steps.controls
	includeResults := flag("includeMachineResults", !flag("compact", false) && c.includeMachineResults)
	includeSpace := flag("includePerceptualSpace", c.includePerceptualSpace)
	includeRegions := flag("includeActiveRegions", c.includeActiveRegions)
	input, err := assembleInput(body, e.spaceWidth())
	if err != nil {
		return nil, err
	}
	width := max(e.spaceWidth(), len(input))
	if len(e.steps.space) < width {
		e.steps.space = append(e.steps.space, make([]float64, width-len(e.steps.space))...)
	}
	for i := range e.steps.space {
		e.steps.space[i] = 0
	}
	copy(e.steps.space, input)
	for bit := range e.steps.latched {
		if bit < len(e.steps.space) {
			e.steps.space[bit] = 1
		}
	}
	n := e.steps.nextStep
	e.steps.nextStep++
	s, err := e.runPhases(n, override)
	if err != nil {
		return nil, err
	}
	return s.JSON(includeResults, includeSpace, includeRegions, only), nil
}

type composed struct {
	id         string
	snapshot   []float64
	transition map[string]any
}

// runPhases runs one step over the space as it now stands. The caller holds
// the write lock.
func (e *Engine) runPhases(n int, override string) (*step, error) {
	// B0: whether this step is timed is decided here, once.
	timer := newPhaseTimer(e.steps.controls.phaseDetail)
	subs := e.subscriptions()
	isre := sparse(n, e.steps.space)
	timer.boundary(0)

	// Compose in parallel; the WaitGroup is the join.
	ids := e.machineIDs()
	results := make([]composed, len(ids))
	work := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < min(runtime.GOMAXPROCS(0), max(len(ids), 1)); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				m := e.machines[ids[i]]
				snapshot := make([]float64, m.InputLength)
				for j := range snapshot {
					if k := m.InputOffset + j; k < len(e.steps.space) {
						snapshot[j] = e.steps.space[k]
					}
				}
				results[i] = composed{ids[i], snapshot, m.Respond(snapshot, override, clock)}
			}
		}()
	}
	for i := range ids {
		work <- i
	}
	close(work)
	wg.Wait()
	timer.boundary(1)

	machineResults := map[string]map[string]any{}
	var merges []mergeOperation
	var regions []activeRegion
	type firing struct {
		machineID   string
		sequenceIDs []string
		provenance  []string
	}
	var firings []firing
	for _, c := range results {
		m := e.machines[c.id]
		e.coverage.record(m, c.transition)
		input, output, _ := m.Regions()
		t := c.transition
		var outputVector any
		if picked, ok := t["machineOutput"].(map[string]any); ok {
			outputVector = picked["vector"]
		}
		merged, _ := t["mergedOutput"].([]float64)

		type pendingOutput struct {
			sequenceID string
			values     []float64
			action     any
		}
		var pending []pendingOutput
		sequenceIDs, provenance := []string{}, []string{}
		seenSeq, seenVertex := map[string]bool{}, map[string]bool{}
		if meta, _ := t["arbiterMetadata"].(map[string]any); meta["shouldOutput"] == true {
			seqResults, _ := t["sequenceResults"].(map[string]any)
			for _, seqID := range sortedMapKeys(seqResults) {
				r, _ := seqResults[seqID].(map[string]any)
				outs, _ := r["assertedOutputs"].([]any)
				for _, ov := range outs {
					o := ov.(map[string]any)
					if !seenSeq[seqID] {
						seenSeq[seqID] = true
						sequenceIDs = append(sequenceIDs, seqID)
					}
					values, _ := o["vector"].([]float64)
					var action any
					if om, ok := o["metadata"].(map[string]any); ok {
						if a, ok := om["action"].(string); ok {
							action = a
						}
					}
					pending = append(pending, pendingOutput{seqID, values, action})
					pv, _ := o["provenance"].([]string)
					for _, v := range pv {
						if !seenVertex[v] {
							seenVertex[v] = true
							provenance = append(provenance, v)
						}
					}
				}
			}
		}
		sort.Strings(sequenceIDs)
		if len(sequenceIDs) > 0 {
			firings = append(firings, firing{c.id, sequenceIDs, provenance})
		}

		hasOutput := outputVector != nil || merged != nil
		regions = append(regions, activeRegion{input.Offset, input.Length, c.id, "input"})
		var outputRegion any
		if hasOutput {
			regions = append(regions, activeRegion{output.Offset, output.Length, c.id, "output"})
			outputRegion = map[string]any{"offset": output.Offset, "length": output.Length}
		}
		delete(t, "machineId")
		var mergedJSON any
		if merged != nil {
			mergedJSON = merged
		}
		machineResults[c.id] = map[string]any{
			"machineId": c.id, "machineName": m.Name, "inputEvent": c.snapshot,
			"outputVector": outputVector, "mergedOutputVector": mergedJSON,
			"outputMergeTransformation": m.OutputMerge,
			"inputRegion":               map[string]any{"offset": input.Offset, "length": input.Length},
			"outputRegion":              outputRegion, "transitionResult": t,
		}
		// A refused fold presents nothing, and nothing is not zeros.
		if merged == nil {
			continue
		}
		// The join over the contributors: each resolves against its own
		// asserted values; the highest severity wins, ties to the smallest
		// sequence id, and the winning decision travels whole with its action.
		var gov map[string]any
		bestRank, bestSeq := 0.0, ""
		for _, p := range pending {
			// Deprecated fires count once per asserted output (FOLD_PLACEMENT
			// amendment A4).
			if seq, ok := m.Sequences[p.sequenceID]; ok && seq.DeprecatedAt != "" {
				e.coverage.deprecated[[4]string{c.id, m.Name, p.sequenceID, seq.ReplacedBy}]++
			}
			decision := resolveGovernance(m, p.sequenceID, p.values)
			if decision == nil {
				continue
			}
			rag, _ := decision["ragStatusCode"].(string)
			rank := severityRank(rag)
			if gov == nil || rank > bestRank || (rank == bestRank && p.sequenceID < bestSeq) {
				decision["actionCode"] = p.action
				gov, bestRank, bestSeq = decision, rank, p.sequenceID
			}
		}
		var deprecation map[string]any
		for _, seqID := range sequenceIDs {
			if seq, ok := m.Sequences[seqID]; ok && seq.DeprecatedAt != "" {
				deprecation = map[string]any{"since": seq.DeprecatedAt, "ageDays": daysSince(seq.DeprecatedAt)}
				if seq.ReplacedBy != "" {
					deprecation["replacedBy"] = seq.ReplacedBy
				}
				break
			}
		}
		rag := ""
		if gov != nil {
			rag, _ = gov["ragStatusCode"].(string)
			text := func(key, fallback string) string {
				if s, ok := gov[key].(string); ok && s != "" {
					return s
				}
				return fallback
			}
			e.coverage.paging[[4]string{text("ownerTeam", "unrouted"), text("processStatus", "unknown"), text("ragStatusCode", "unknown"), c.id}]++
		}
		merges = append(merges, mergeOperation{
			offset: output.Offset, length: output.Length, machineID: c.id, machineName: m.Name,
			sequenceIDs: sequenceIDs, values: merged, provenance: provenance,
			governance: gov, ragStatusCode: rag, deprecation: deprecation,
		})
	}
	sort.SliceStable(merges, func(i, j int) bool {
		if merges[i].machineName != merges[j].machineName {
			return merges[i].machineName < merges[j].machineName
		}
		return merges[i].offset < merges[j].offset
	})

	// Gather, resolve, commit (ARBITER_CONTRACT §2).
	space := e.steps.space
	fanIn := make([]int, len(space))
	for _, op := range merges {
		for i := 0; i < min(op.length, len(op.values)); i++ {
			if c := op.offset + i; c < len(fanIn) {
				fanIn[c]++
			}
		}
	}
	var osreCells []cellValue
	contended := map[int][]contribution{}
	for _, op := range merges {
		for i := 0; i < min(op.length, len(op.values)); i++ {
			c, v := op.offset+i, op.values[i]
			if c >= len(fanIn) {
				continue
			}
			if fanIn[c] == 1 {
				space[c] = v
				if v != 0 {
					osreCells = append(osreCells, cellValue{c, v})
				}
				continue
			}
			contended[c] = append(contended[c], contribution{
				value: v, provider: "machine", originID: op.machineID,
				cesID: strings.Join(op.sequenceIDs, ","), outputVectorID: "0", ragStatusCode: op.ragStatusCode,
			})
		}
	}
	cells := make([]int, 0, len(contended))
	for c := range contended {
		cells = append(cells, c)
	}
	sort.Ints(cells)
	records := []arbitrationRecord{}
	for _, c := range cells {
		var entry *arbitrationEntry
		if en, ok := e.steps.arbitration.entries[c]; ok {
			entry = &en
		}
		v, rec := resolveCell(c, n, contended[c], entry)
		if rec != nil {
			records = append(records, *rec)
		}
		space[c] = v
		if v != 0 {
			osreCells = append(osreCells, cellValue{c, v})
		}
	}
	sort.Slice(osreCells, func(i, j int) bool { return osreCells[i].index < osreCells[j].index })
	osre := trajectoryEntry{stepNumber: n, length: len(space), nonZero: osreCells}

	// The event bus, driven by the firings rather than the batch, so a refused
	// fold withdraws its value and not its subscribers. Written after OSRE is
	// observed: these bits reach the machines next step.
	seen := map[string]bool{}
	var writes []busWrite
	for _, f := range firings {
		for _, seqID := range f.sequenceIDs {
			for _, sub := range subs[[2]string{f.machineID, seqID}] {
				key := fmt.Sprint(sub.machineID, "\x00", sub.bit, "\x00", f.machineID, "\x00", seqID)
				if seen[key] {
					continue
				}
				seen[key] = true
				writes = append(writes, busWrite{sub.machineID, sub.bit, f.machineID, seqID, f.provenance})
				e.steps.latched[sub.bit] = true
			}
		}
	}
	sort.SliceStable(writes, func(i, j int) bool {
		a, b := writes[i], writes[j]
		if a.subscriber != b.subscriber {
			return a.subscriber < b.subscriber
		}
		if a.bit != b.bit {
			return a.bit < b.bit
		}
		if a.producer != b.producer {
			return a.producer < b.producer
		}
		return a.sequenceID < b.sequenceID
	})
	for _, w := range writes {
		if w.bit < len(space) {
			space[w.bit] = 1
		}
	}

	sort.Slice(regions, func(i, j int) bool {
		a, b := regions[i], regions[j]
		if a.offset != b.offset {
			return a.offset < b.offset
		}
		if a.length != b.length {
			return a.length < b.length
		}
		if a.machineID != b.machineID {
			return a.machineID < b.machineID
		}
		return a.kind < b.kind
	})
	timer.boundary(2)
	s := &step{
		number: n, timestamp: clock.NowMS(), machineResults: machineResults, activeRegions: regions,
		mergeBatch: merges, eventBus: writes, space: append([]float64{}, space...),
	}

	// The commit: the clock ticks for the step and the pair is appended; the
	// arbitration records are retained before the completion is published.
	lamport, err := e.instance.Tick()
	if err != nil {
		return nil, &machine.Error{Status: 500, Message: err.Error()}
	}
	e.steps.commit(isre, osre)
	e.steps.retainArbitration(n, lamport, records)
	e.steps.lastArbitration = records
	e.completedStep = n
	timer.boundary(3)
	e.steps.publish()
	timer.boundary(4)
	if timer.on {
		for i, d := range timer.spent {
			e.steps.phaseTime[i] += d
		}
		e.steps.measuredSteps++
	}
	e.steps.history = append([]*step{s}, e.steps.history...)
	e.steps.trimHistory()
	return s, nil
}

func sortedMapKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// resolveGovernance is C++ resolve_governance: the first
// metadata.triggerConfig.rules entry for sequenceID whose outputMatches
// equals values, with the rule's governance overriding the machine's
// metadata.governance.
func resolveGovernance(m *machine.Machine, sequenceID string, values []float64) map[string]any {
	meta, ok := m.Raw["metadata"].(map[string]any)
	if !ok {
		return nil
	}
	trigger, _ := meta["triggerConfig"].(map[string]any)
	rules, ok := trigger["rules"].([]any)
	if !ok {
		return nil
	}
	var rule map[string]any
	for _, rv := range rules {
		r, ok := rv.(map[string]any)
		if !ok || r["sequenceId"] != sequenceID {
			continue
		}
		matches, ok := r["outputMatches"].([]any)
		if !ok || len(matches) != len(values) {
			continue
		}
		equal := true
		for i, mv := range matches {
			if f, ok := mv.(float64); !ok || f != values[i] {
				equal = false
				break
			}
		}
		if equal {
			rule = r
			break
		}
	}
	if rule == nil {
		return nil
	}
	machineGov, hasMachineGov := meta["governance"].(map[string]any)
	ruleGov, hasRuleGov := rule["governance"].(map[string]any)
	pickText := func(key, fallback string) string {
		if s, ok := ruleGov[key].(string); ok {
			return s
		}
		if hasMachineGov {
			if s, ok := machineGov[key].(string); ok {
				return s
			}
		}
		return fallback
	}
	processStatus, _ := rule["processStatus"].(string)
	var sla any
	if f, ok := ruleGov["slaSeconds"].(float64); hasRuleGov && ok {
		sla = int64(f)
	} else if slaMap, ok := machineGov["sla"].(map[string]any); hasMachineGov && ok && processStatus != "" {
		if f, ok := slaMap[processStatus].(float64); ok {
			sla = int64(f)
		}
	}
	contact := map[string]any{}
	for _, field := range []string{"primary", "secondary"} {
		rc, _ := ruleGov["contact"].(map[string]any)
		mc, _ := machineGov["contact"].(map[string]any)
		v, ok := rc[field].(string)
		if !ok && hasMachineGov {
			v, ok = mc[field].(string)
		}
		if ok && v != "" {
			contact[field] = v
		}
	}
	source := "machine-fallback"
	if hasRuleGov {
		source = "rule-with-override"
	} else if hasMachineGov {
		source = "rule-only"
	}
	rag, _ := rule["ragStatusCode"].(string)
	decision := map[string]any{
		"machineId": m.ID, "machineName": m.Name, "sequenceId": sequenceID,
		"ragStatusCode": nullable(rag), "actionCode": nil, "processStatus": nullable(processStatus),
		"ownerTeam": pickText("ownerTeam", "unrouted"), "slaSeconds": sla,
		"runbook": nullable(pickText("runbook", "")), "escalationPolicy": nullable(pickText("escalationPolicy", "")),
		"contact": contact, "source": source, "hasMachineGovernance": hasMachineGov,
	}
	if d, ok := rule["description"].(string); ok && d != "" {
		decision["description"] = d
	}
	return decision
}

// daysSince is whole days since a YYYY-MM-DD (or YYYY-MM-DDT...) date, at
// midnight UTC; 0 when it does not parse.
func daysSince(date string) int64 {
	if len(date) < 10 {
		return 0
	}
	parts := strings.Split(date[:10], "-")
	if len(parts) != 3 {
		return 0
	}
	y, err1 := strconv.Atoi(parts[0])
	mo, err2 := strconv.Atoi(parts[1])
	d, err3 := strconv.Atoi(parts[2])
	if err1 != nil || err2 != nil || err3 != nil {
		return 0
	}
	then := time.Date(y, time.Month(mo), d, 0, 0, 0, 0, time.UTC)
	return time.Now().UnixMilli()/86_400_000 - then.Unix()/86_400
}

// assembleInput is POST /api/perceive's input: exactly one of vector,
// sparseVector, domainVectors, assembled to at least width.
func assembleInput(body map[string]any, width int) ([]float64, error) {
	number := func(v any, what string) (float64, error) {
		f, ok := v.(float64)
		if !ok {
			return 0, machine.BadRequest("%s must be a number", what)
		}
		return f, nil
	}
	index := func(v any, what string) (int, error) {
		f, ok := v.(float64)
		if !ok || f < 0 || f != float64(int(f)) {
			return 0, machine.BadRequest("%s must be a non-negative integer", what)
		}
		return int(f), nil
	}
	if l, ok := body["vector"].([]any); ok {
		out := make([]float64, len(l))
		for i, x := range l {
			f, ok := x.(float64)
			if !ok {
				return nil, machine.BadRequest("vector values must be finite numbers")
			}
			out[i] = f
		}
		return out, nil
	}
	if cells, ok := body["sparseVector"].([]any); ok {
		type pair struct {
			i int
			v float64
		}
		pairs := make([]pair, 0, len(cells))
		length := width
		for _, cv := range cells {
			c, _ := cv.(map[string]any)
			i, err := index(c["index"], "sparseVector index")
			if err != nil {
				return nil, err
			}
			v, err := number(c["value"], "sparseVector value")
			if err != nil {
				return nil, err
			}
			pairs = append(pairs, pair{i, v})
			length = max(length, i+1)
		}
		out := make([]float64, length)
		for _, p := range pairs {
			out[p.i] = p.v
		}
		return out, nil
	}
	if regions, ok := body["domainVectors"].([]any); ok {
		type write struct {
			offset int
			values []float64
		}
		writes := make([]write, 0, len(regions))
		length := width
		for _, rv := range regions {
			r, _ := rv.(map[string]any)
			offset, err := index(r["offset"], "domainVectors offset")
			if err != nil {
				return nil, err
			}
			raw, _ := r["values"].([]any)
			values := make([]float64, len(raw))
			for i, x := range raw {
				if values[i], err = number(x, "domainVectors value"); err != nil {
					return nil, err
				}
			}
			writes = append(writes, write{offset, values})
			length = max(length, offset+len(values))
		}
		out := make([]float64, length)
		for _, w := range writes {
			copy(out[w.offset:], w.values)
		}
		return out, nil
	}
	return nil, machine.BadRequest("Provide exactly one of: vector, sparseVector, domainVectors")
}

// Trajectory is GET /api/engine/isre-history (isre true) or osre-history:
// ascending stepNumber from from, at most limit entries (0: all).
func (e *Engine) Trajectory(isre bool, from, limit int) map[string]any {
	e.mu.RLock()
	defer e.mu.RUnlock()
	h := e.steps.osre
	if isre {
		h = e.steps.isre
	}
	out := []any{}
	for _, entry := range h {
		if entry.stepNumber < from {
			continue
		}
		if limit > 0 && len(out) == limit {
			break
		}
		out = append(out, entry.json())
	}
	return map[string]any{"history": out}
}

// StepPair is GET /api/engine/steps/:n/pair: step n's committed pair, waiting
// up to timeout for it. The wait holds no lock, so the step can run.
func (e *Engine) StepPair(n int, timeout time.Duration) (map[string]any, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		e.mu.RLock()
		if e.completedStep >= n {
			defer e.mu.RUnlock()
			for i, entry := range e.steps.isre {
				if entry.stepNumber == n {
					return map[string]any{"stepNumber": n, "isre": entry.json(), "osre": e.steps.osre[i].json()}, nil
				}
			}
			return nil, &machine.Error{Status: 410, Message: fmt.Sprintf("step %d is no longer retained", n)}
		}
		wait := e.steps.published
		e.mu.RUnlock()
		select {
		case <-wait:
		case <-deadline.C:
			return nil, &machine.Error{Status: 408, Message: fmt.Sprintf("step %d not resolved within %d ms", n, timeout.Milliseconds())}
		}
	}
}

// Arbitration is GET /api/arbitration (SURFACE_SPEC "Arbitration retention
// and the instance clock"). Retention off (the default) answers the legacy
// object: the latest step's records in resolution order. On, the retained
// steps in the window ending at the newest, oldest first, each under the
// clock it committed at; step addresses one.
func (e *Engine) Arbitration(stepParam *string) (any, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	c := e.steps.controls
	if stepParam != nil {
		n, ok := wholeNumber(*stepParam)
		if !ok {
			return nil, machine.BadRequest("step must be a non-negative integer")
		}
		if !c.arbitrationRetention {
			return nil, &machine.Error{Status: 409, Message: "arbitration retention is off; GET /api/arbitration?step=N needs arbitrationRetention true"}
		}
		if n > e.completedStep {
			return nil, machine.NotFound("step %d has not resolved", n)
		}
		r, ok := e.steps.retained[n]
		if !ok {
			return nil, &machine.Error{Status: 410, Message: fmt.Sprintf("step %d is no longer retained", n)}
		}
		return e.retainedStepJSON(n, r), nil
	}
	if c.arbitrationRetention {
		first := max(e.completedStep-c.arbitrationWindow+1, 0)
		steps := []any{}
		for n := first; n <= e.completedStep; n++ {
			if r, ok := e.steps.retained[n]; ok {
				steps = append(steps, e.retainedStepJSON(n, r))
			}
		}
		return steps, nil
	}
	return e.arbitrationBody(e.steps.lastArbitration, false), nil
}

func (e *Engine) arbitrationBody(records []arbitrationRecord, canonical bool) map[string]any {
	rs := recordsJSON(records, canonical)
	return map[string]any{
		"registryEntries": len(e.steps.arbitration.entries), "registrySource": nullable(e.steps.arbitration.source),
		"shards": arbiterShards(), "count": len(rs), "records": rs,
	}
}

func (e *Engine) retainedStepJSON(n int, r retainedStep) map[string]any {
	body := e.arbitrationBody(r.records, true)
	body["clock"] = map[string]any{"instance": e.instance.Instance(), "lamport": r.lamport, "step": n}
	return body
}

// wholeNumber parses a string of ASCII digits.
func wholeNumber(raw string) (int, bool) {
	if raw == "" || strings.TrimLeft(raw, "0123456789") != "" {
		return 0, false
	}
	n, err := strconv.Atoi(raw)
	return n, err == nil
}

// PairTimeout reads timeoutMs: default 5000, at most 60000.
func PairTimeout(raw *string) (time.Duration, error) {
	if raw == nil {
		return pairTimeoutDefaultMS * time.Millisecond, nil
	}
	ms, ok := wholeNumber(*raw)
	if !ok || ms > pairTimeoutMaxMS {
		return 0, machine.BadRequest("timeoutMs must be an integer in [0, %d]", pairTimeoutMaxMS)
	}
	return time.Duration(ms) * time.Millisecond, nil
}

// StepNumber reads a step path parameter.
func StepNumber(raw string) (int, error) {
	n, ok := wholeNumber(raw)
	if !ok {
		return 0, machine.BadRequest("step must be a non-negative integer")
	}
	return n, nil
}
