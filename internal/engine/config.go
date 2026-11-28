package engine

import (
	"math"
	"strconv"
	"time"

	"github.com/jateeter/RealityEngine_GO/internal/machine"
)

// One pathway for every runtime control, and the runtime's own views
// (RealityEngine_GO#13, slice 3; the pathway's routes are listed under #2):
//
//   - /api/engine/config: every control in the five-field shape (name, scope,
//     value, default, mutable), sorted by name, with its declared default
//     (SURFACE_SPEC "/api/engine/config"). PUT sets one, DELETE restores the
//     declared default, and a write that lands nowhere is an error, never 200.
//   - /api/runtime/options: the flat view of the same state; a write through
//     either is visible through the other (SURFACE_SPEC "are one state").
//   - /api/runtime/metrics, /vector-space, /storage-footprint, and
//     POST /api/perception/diagnostic.
//
// Only controls this engine acts on are carried: "a control that reports a
// value it does not act on is worse than an absent one". clone.maxActive
// bounds live clones per template (clones.go, #5).

// arbitrationWindowMax bounds arbitrationWindow.
const arbitrationWindowMax = 1024

// stepPhases are the universal step phases, B0 to B5 (SURFACE_SPEC
// "phaseDetail: step phase timing").
var stepPhases = [5]string{"step.isre_capture", "step.compose", "step.resolve", "step.commit", "step.publish"}

func control(name, scope string, value, def any) map[string]any {
	return map[string]any{"name": name, "scope": scope, "value": value, "default": def, "mutable": true}
}

// controls is every control, sorted by name. The caller holds the lock.
func (e *Engine) controls() []map[string]any {
	c, d := e.steps.controls, defaultControls()
	inhibited := map[string]any{}
	for id, m := range e.machines {
		inhibited[id] = m.TransitionsInhibited
	}
	return []map[string]any{
		control("arbitrationRetention", "engine", c.arbitrationRetention, d.arbitrationRetention),
		control("arbitrationWindow", "engine", c.arbitrationWindow, d.arbitrationWindow),
		control("clone.maxActive", "engine", e.clones.maxActive, cloneMaxActiveFirst),
		control("historyLimit", "engine", c.historyLimit, d.historyLimit),
		control("includeActiveRegions", "engine", c.includeActiveRegions, d.includeActiveRegions),
		control("includeMachineResults", "engine", c.includeMachineResults, d.includeMachineResults),
		control("includePerceptualSpace", "engine", c.includePerceptualSpace, d.includePerceptualSpace),
		control("klineCapture", "engine", e.klines.controls.capture, false),
		control("klineFeedbackHops", "engine", e.klines.controls.feedbackHops, klineFeedbackHopsDefault),
		control("klineStoreMaxBytes", "engine", e.klines.controls.storeMaxBytes, uint64(klineStoreMaxDefault)),
		control("machineJsonSummary", "engine", e.machineJSONSummary, false),
		control("phaseDetail", "engine", c.phaseDetail, d.phaseDetail),
		control("transitionsInhibited", "machine", inhibited, false),
	}
}

func (e *Engine) control(name string) (map[string]any, error) {
	for _, c := range e.controls() {
		if c["name"] == name {
			return c, nil
		}
	}
	return nil, machine.NotFound("Unknown control: %s", name)
}

// Controls is GET /api/engine/config.
func (e *Engine) Controls() map[string]any {
	e.mu.RLock()
	defer e.mu.RUnlock()
	cs := e.controls()
	out := make([]any, len(cs))
	for i, c := range cs {
		out[i] = c
	}
	return map[string]any{"controls": out}
}

// Control is GET /api/engine/config/:control.
func (e *Engine) Control(name string) (map[string]any, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.control(name)
}

// wholeIn reads a whole number in [0, limit] (limit < 0: unbounded).
func wholeIn(v any, limit float64) (int, bool) {
	f, ok := v.(float64)
	if !ok || f < 0 || f != math.Trunc(f) || (limit >= 0 && f > limit) {
		return 0, false
	}
	return int(f), true
}

// SetControl is PUT /api/engine/config/:control. For the machine-scoped
// control a body naming a machine sets that machine (404 if absent); one
// omitting it sets every machine.
func (e *Engine) SetControl(name string, body map[string]any) (map[string]any, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.setControl(name, body)
}

// setControl is SetControl under the caller's write lock (a restore holds it).
func (e *Engine) setControl(name string, body map[string]any) (map[string]any, error) {
	if _, err := e.control(name); err != nil {
		return nil, err
	}
	value := body["value"]
	boolean := func() (bool, error) {
		b, ok := value.(bool)
		if !ok {
			return false, machine.BadRequest("%s requires a boolean `value`", name)
		}
		return b, nil
	}
	c := &e.steps.controls
	var err error
	switch name {
	case "arbitrationRetention":
		// Off is the legacy escape: it drops what was kept. On retains from
		// the next committed step.
		if c.arbitrationRetention, err = boolean(); err == nil && !c.arbitrationRetention {
			e.steps.retained = map[int]retainedStep{}
		}
	case "arbitrationWindow":
		w, ok := wholeIn(value, arbitrationWindowMax)
		if !ok {
			return nil, machine.BadRequest("arbitrationWindow requires a whole-number `value` in [0, %d]", arbitrationWindowMax)
		}
		c.arbitrationWindow = w // narrowing evicts at once
		e.pruneArbitration()
	case "clone.maxActive":
		// Lowering it below the live count refuses new clones of that
		// template until the count falls; it never recycles one.
		bound, ok := wholeIn(value, cloneBlocks)
		if !ok {
			return nil, machine.BadRequest("clone.maxActive requires a whole-number `value` in [0, %d]", cloneBlocks)
		}
		e.clones.maxActive = bound
	case "historyLimit":
		limit, ok := wholeIn(value, -1)
		if !ok {
			return nil, machine.BadRequest("historyLimit requires a non-negative whole-number `value`")
		}
		c.historyLimit = limit
		e.steps.trimHistory()
	case "includeActiveRegions":
		c.includeActiveRegions, err = boolean()
	case "includeMachineResults":
		c.includeMachineResults, err = boolean()
	case "includePerceptualSpace":
		c.includePerceptualSpace, err = boolean()
	case "klineCapture":
		// Off stops capture and leaves the store untouched; on starts afresh,
		// closing what the checkpoint holds (SURFACE_SPEC "Controls").
		var on bool
		if on, err = boolean(); err != nil {
			break
		}
		if on && !e.klines.controls.capture {
			if cerr := e.klines.startCapture(); cerr != nil {
				e.klineFailed(cerr)
			}
		} else if !on {
			e.klines.controls.capture = false
		}
	case "klineFeedbackHops":
		h, ok := wholeIn(value, klineFeedbackHopsMax)
		if !ok {
			return nil, machine.BadRequest("klineFeedbackHops requires a whole-number `value` in [0, %d]", klineFeedbackHopsMax)
		}
		e.klines.controls.feedbackHops = h
	case "klineStoreMaxBytes":
		b, ok := wholeIn(value, klineStoreMaxMax)
		if !ok {
			return nil, machine.BadRequest("klineStoreMaxBytes requires a whole-number `value` in [0, %d]", int64(klineStoreMaxMax))
		}
		e.klines.controls.storeMaxBytes = uint64(b)
		e.klines.refreshBudget()
	case "machineJsonSummary":
		e.machineJSONSummary, err = boolean()
	case "phaseDetail":
		c.phaseDetail, err = boolean()
	case "transitionsInhibited":
		var b bool
		if b, err = boolean(); err != nil {
			break
		}
		if id, ok := body["machine"].(string); ok {
			m, held := e.machines[id]
			if !held {
				return nil, machine.NotFound("Machine not found: %s", id)
			}
			m.TransitionsInhibited = b
		} else {
			for _, m := range e.machines {
				m.TransitionsInhibited = b
			}
		}
	}
	if err != nil {
		return nil, err
	}
	return e.control(name)
}

// ResetControl is DELETE /api/engine/config/:control: the declared default.
func (e *Engine) ResetControl(name string) (map[string]any, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, err := e.control(name); err != nil {
		return nil, err
	}
	c, d := &e.steps.controls, defaultControls()
	switch name {
	case "arbitrationRetention":
		c.arbitrationRetention = d.arbitrationRetention
		e.steps.retained = map[int]retainedStep{}
	case "arbitrationWindow":
		c.arbitrationWindow = d.arbitrationWindow
		e.pruneArbitration()
	case "clone.maxActive":
		e.clones.maxActive = cloneMaxActiveFirst
	case "historyLimit":
		c.historyLimit = d.historyLimit
		e.steps.trimHistory()
	case "includeActiveRegions":
		c.includeActiveRegions = d.includeActiveRegions
	case "includeMachineResults":
		c.includeMachineResults = d.includeMachineResults
	case "includePerceptualSpace":
		c.includePerceptualSpace = d.includePerceptualSpace
	case "klineCapture":
		e.klines.controls.capture = false
	case "klineFeedbackHops":
		e.klines.controls.feedbackHops = klineFeedbackHopsDefault
	case "klineStoreMaxBytes":
		e.klines.controls.storeMaxBytes = klineStoreMaxDefault
		e.klines.refreshBudget()
	case "machineJsonSummary":
		e.machineJSONSummary = false
	case "phaseDetail":
		c.phaseDetail = d.phaseDetail
	case "transitionsInhibited":
		for _, m := range e.machines {
			m.TransitionsInhibited = false
		}
	}
	return e.control(name)
}

// pruneArbitration drops every retained step outside the window ending at the
// newest committed step.
func (e *Engine) pruneArbitration() {
	first := e.completedStep - e.steps.controls.arbitrationWindow + 1
	for n := range e.steps.retained {
		if n < first {
			delete(e.steps.retained, n)
		}
	}
}

func (s *stepState) trimHistory() {
	if len(s.history) > s.controls.historyLimit {
		s.history = s.history[:s.controls.historyLimit]
	}
}

func (e *Engine) runtimeOptions() map[string]any {
	c := e.steps.controls
	return map[string]any{
		"historyLimit": c.historyLimit, "includeActiveRegions": c.includeActiveRegions,
		"includeMachineResults": c.includeMachineResults, "includePerceptualSpace": c.includePerceptualSpace,
		"phaseDetail": c.phaseDetail,
	}
}

// RuntimeOptions is GET /api/runtime/options.
func (e *Engine) RuntimeOptions() map[string]any {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.runtimeOptions()
}

// PatchRuntimeOptions is PATCH /api/runtime/options: each key that is present
// with the right type is written; the answer is the options as they now are.
func (e *Engine) PatchRuntimeOptions(body map[string]any) (map[string]any, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	c := &e.steps.controls
	if v, ok := body["historyLimit"].(float64); ok {
		if v < 0 {
			return nil, machine.BadRequest("historyLimit must be a non-negative number")
		}
		c.historyLimit = int(v)
		e.steps.trimHistory()
	}
	for key, slot := range map[string]*bool{
		"includeMachineResults": &c.includeMachineResults, "includePerceptualSpace": &c.includePerceptualSpace,
		"includeActiveRegions": &c.includeActiveRegions, "phaseDetail": &c.phaseDetail,
	} {
		if b, ok := body[key].(bool); ok {
			*slot = b
		}
	}
	return e.runtimeOptions(), nil
}

// RuntimeMetrics is GET /api/runtime/metrics: the engine stats, and how this
// runtime composes machines in parallel.
func (e *Engine) RuntimeMetrics() map[string]any {
	stats := e.Stats()["stats"]
	return map[string]any{"stats": stats, "domainWorkerPool": map[string]any{"semantics": "goroutine-pool"}}
}

// VectorSpace is GET /api/runtime/vector-space.
func (e *Engine) VectorSpace() map[string]any {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return map[string]any{
		"dimension": e.dimension, "requiredDimension": e.requiredDimension(),
		"encoding": "dense-float64-clamped-0-1", "mappingVersion": e.mappingVersion,
	}
}

// StorageFootprint is GET /api/runtime/storage-footprint: what each mapped
// machine's regions cost as float64 and as packed bitsPerElement cells.
func (e *Engine) StorageFootprint() map[string]any {
	e.mu.RLock()
	defer e.mu.RUnlock()
	histogram := map[string]any{"1": 0, "2": 0, "4": 0, "8": 0}
	var cellsTotal, floatTotal, packedTotal int
	perMachine := []any{}
	for _, id := range e.machineIDs() {
		m := e.machines[id]
		mapping, ok := m.Raw["perceptualMapping"].(map[string]any)
		if !ok {
			continue
		}
		bits := 8
		if b, ok := mapping["bitsPerElement"].(float64); ok && (b == 1 || b == 2 || b == 4 || b == 8) {
			bits = int(b)
		}
		input, output, _ := m.Regions()
		cells := input.Length + output.Length
		float64Bytes, packed := cells*8, (cells*bits+7)/8
		cellsTotal, floatTotal, packedTotal = cellsTotal+cells, floatTotal+float64Bytes, packedTotal+packed
		key := strconv.Itoa(bits)
		histogram[key] = histogram[key].(int) + 1
		perMachine = append(perMachine, map[string]any{
			"machineId": id, "machineName": m.Name, "bitsPerElement": bits,
			"inputCells": input.Length, "outputCells": output.Length,
			"float64Bytes": float64Bytes, "packedBytes": packed, "shrinkFactor": ratio(float64Bytes, packed),
		})
	}
	return map[string]any{
		"machinesRegistered": len(e.machines), "totalCells": cellsTotal,
		"totalFloat64Bytes": floatTotal, "totalPackedBytes": packedTotal,
		"cumulativeShrinkFactor": ratio(floatTotal, packedTotal), "widthHistogram": histogram,
		"perMachine": perMachine,
	}
}

func ratio(float64Bytes, packed int) float64 {
	if packed == 0 {
		return 0
	}
	return float64(float64Bytes) / float64(packed)
}

// Diagnostic is POST /api/perception/diagnostic (C++
// PerceptionMapper::diagnostic_mapping): the universal space's non-zero
// cells, and each mapped machine's input region resolved from it. The space is
// the engine's dimension, widened to the posted vector; a region past it is a
// 500.
func (e *Engine) Diagnostic(universal []float64) (map[string]any, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	dimension := max(e.dimension, len(universal))
	nonZero := []any{}
	for i, v := range universal {
		if v != 0 {
			nonZero = append(nonZero, map[string]any{"index": i, "value": v})
		}
	}
	mappings := []any{}
	for _, id := range e.machineIDs() {
		m := e.machines[id]
		input, _, ok := m.Regions()
		if !ok {
			continue
		}
		if input.Offset+input.Length > dimension {
			return nil, &machine.Error{Status: 500, Message: "Input mapping outside perceptual space"}
		}
		resolved := make([]float64, input.Length)
		for i := range resolved {
			if j := input.Offset + i; j < len(universal) {
				resolved[i] = universal[j]
			}
		}
		mappings = append(mappings, map[string]any{
			"machineId": id, "machineName": m.Name,
			"inputMapping":  map[string]any{"offset": input.Offset, "length": input.Length},
			"resolvedInput": resolved,
		})
	}
	return map[string]any{
		"universalSpace":  map[string]any{"dimension": dimension, "nonZeroValues": nonZero},
		"machineMappings": mappings,
	}, nil
}

// phaseTimer measures one step's phases when phaseDetail was on at B0.
type phaseTimer struct {
	on    bool
	mark  time.Time
	spent [5]time.Duration
}

func newPhaseTimer(on bool) *phaseTimer { return &phaseTimer{on: on, mark: time.Now()} }

// boundary closes phase i at this instant.
func (t *phaseTimer) boundary(i int) {
	if !t.on {
		return
	}
	now := time.Now()
	t.spent[i] = now.Sub(t.mark)
	t.mark = now
}

// PhaseTotals are the step phases' cumulative seconds and the steps measured,
// for /api/metrics; ok is false while phaseDetail is off (the lines are
// absent, not zero).
func (e *Engine) PhaseTotals() (seconds map[string]float64, steps uint64, ok bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if !e.steps.controls.phaseDetail {
		return nil, 0, false
	}
	seconds = map[string]float64{}
	for i, p := range stepPhases {
		seconds[p] = e.steps.phaseTime[i].Seconds()
	}
	return seconds, e.steps.measuredSteps, true
}
