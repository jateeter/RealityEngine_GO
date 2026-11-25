package engine

import (
	"runtime"
	"sort"
	"sync"

	"github.com/jateeter/RealityEngine_GO/internal/instance"
	"github.com/jateeter/RealityEngine_GO/internal/machine"
)

// Engine-wide processing and its audit trail (RealityEngine_GO#13, slice 1):
// POST /api/engine/process and /reset, GET /api/engine/history, /stats,
// /active and /clock. SURFACE_SPEC.md is the contract; where C++ and LSP agree
// and RealityEngine_Rust differs (the {result} envelope, the history record,
// the reset answer), this follows the voters.

// historyCapacity bounds the /api/engine/process audit trail.
const historyCapacity = 256

// SetClock gives the engine its instance clock (FromEnvironment boots it
// before the corpus loads).
func (e *Engine) SetClock(c *instance.Clock) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.instance = c
	e.lastCommit = [2]uint64{c.Lamport(), c.Commit()}
}

// ProcessAll is POST /api/engine/process: every resident machine, in parallel,
// over one atomic collection of the machine set, joined in machine-id order
// before the answer is built. A vector as long as the space is a Universal
// Reality Event and each machine gets the slice at its input region (a
// machine whose region falls outside it, or that has none, contributes
// nothing); any other vector is given to every machine whole. outputs is one
// arbitrated output per machine that fired.
func (e *Engine) ProcessAll(vector []float64) map[string]any {
	e.mu.Lock()
	defer e.mu.Unlock()
	ids := make([]string, 0, len(e.machines))
	for id := range e.machines {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	universal := len(vector) == e.dimension

	outputs := make([]any, len(ids))
	work := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < min(runtime.GOMAXPROCS(0), max(len(ids), 1)); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				m := e.machines[ids[i]]
				input := vector
				if universal {
					var ok bool
					if input, ok = slice(m, vector); !ok {
						continue
					}
				}
				if out := m.Respond(input, "", clock)["machineOutput"]; out != nil {
					outputs[i] = out
				}
			}
		}()
	}
	for i := range ids {
		work <- i
	}
	close(work)
	wg.Wait()
	// No coverage is recorded here: as on C++, coverage counts the step path
	// (perceive and simulation steps) only.

	fired := []any{}
	for _, out := range outputs {
		if out != nil {
			fired = append(fired, out)
		}
	}
	result := map[string]any{"inputEvent": nonNilVector(vector), "timestamp": clock.NowMS(), "outputs": fired}
	e.history = append([]any{map[string]any{"type": "engine-process", "result": result}}, e.history...)
	if len(e.history) > historyCapacity {
		e.history = e.history[:historyCapacity]
	}
	return map[string]any{"result": result}
}

// slice is the machine's input region of a universal vector; ok is false
// without a mapping or when the region falls outside the vector.
func slice(m *machine.Machine, universal []float64) ([]float64, bool) {
	input, _, ok := m.Regions()
	if !ok || input.Offset+input.Length > len(universal) {
		return nil, false
	}
	return universal[input.Offset : input.Offset+input.Length], true
}

func nonNilVector(v []float64) []float64 {
	if v == nil {
		return []float64{}
	}
	return v
}

// Reset is POST /api/engine/reset: every machine back to its initial Reality
// Events, and the step state to its start (step numbers restart, the space and
// the trajectory histories empty, waiters wake, a simulation run rewinds). clearAudit is accepted and reported; this engine keeps no semantic
// audit yet, so there is nothing for it to clear. The process audit trail and
// the clock's lamport survive a reset.
func (e *Engine) Reset(clearAudit bool) map[string]any {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.resetSpace()
	return map[string]any{"success": true, "auditCleared": clearAudit}
}

// History is GET /api/engine/history: newest first; limit > 0 returns that
// many.
func (e *Engine) History(limit int) map[string]any {
	e.mu.RLock()
	defer e.mu.RUnlock()
	h := e.history
	if limit > 0 && limit < len(h) {
		h = h[:limit]
	}
	return map[string]any{"history": append([]any{}, h...)}
}

// Stats is GET /api/engine/stats. Its fields are runtime facts the voters each
// report differently, and no comparison reads them.
func (e *Engine) Stats() map[string]any {
	e.mu.RLock()
	defer e.mu.RUnlock()
	sequences, events, required := 0, 0, 0
	for _, m := range e.machines {
		sequences += len(m.Sequences)
		for _, s := range m.Sequences {
			events += len(s.Events)
		}
		if input, output, ok := m.Regions(); ok {
			required = max(required, input.Offset+input.Length, output.Offset+output.Length)
		}
	}
	return map[string]any{"stats": map[string]any{
		"machineCount": len(e.machines), "sequenceCount": sequences, "vectorCount": events,
		"dimension": e.dimension, "requiredDimension": required,
		"historySize": len(e.steps.history), "mappingVersion": e.mappingVersion,
	}}
}

// Active is GET /api/engine/active: every active Reality Event as it is
// running, with its machine and sequence. The list carries no order
// (SURFACE_SPEC "Active events"); it is built in id order.
func (e *Engine) Active() map[string]any {
	e.mu.RLock()
	defer e.mu.RUnlock()
	ids := make([]string, 0, len(e.machines))
	for id := range e.machines {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	active := []any{}
	for _, id := range ids {
		m := e.machines[id]
		for _, seqID := range m.SequenceIDs() {
			seq := m.Sequences[seqID]
			for _, evID := range seq.EventIDs() {
				ev := seq.Events[evID]
				if !ev.Active {
					continue
				}
				view := ev.View(m.MatchAlgorithm, m.LoadedAtMS)
				outs := []any{}
				for _, o := range view["outputEvents"].([]any) {
					o := o.(map[string]any)
					outs = append(outs, map[string]any{"id": o["id"], "metadata": o["metadata"], "vector": o["vector"]})
				}
				view["outputEvents"] = outs
				active = append(active, map[string]any{"machineId": id, "sequenceId": seqID, "vector": view})
			}
		}
	}
	return map[string]any{"activeEvents": active}
}

// Clock is GET /api/engine/clock: {instance, lamport, commit, step} for the
// newest committed step, and headers, the inbound RE-Clock headers since boot.
// Only the step path commits a step, so until it does lamport and commit are
// the values the instance resumed from and step is -1.
func (e *Engine) Clock() map[string]any {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := e.clockJSON(e.lastCommit, e.completedStep)
	out["headers"] = e.instance.Headers()
	return out
}
