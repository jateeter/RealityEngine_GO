package engine

import (
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/jateeter/RealityEngine_GO/internal/instance"
	"github.com/jateeter/RealityEngine_GO/internal/machine"
)

// klineTrackerFor is the tracker and store for this instance:
// <INSTANCE_KLINE_DIR>/<uuid>/, else the clock directory's sibling klines/.
// A minted UUID keeps its K-lines in memory. RE_KLINE_CAPTURE=1 turns capture
// on at boot.
func klineTrackerFor(c *instance.Clock) (*klineTracker, error) {
	uuid := strings.ToLower(os.Getenv("INSTANCE_UUID"))
	dir := ""
	if uuid != "" && uuid == c.Instance() {
		parent := os.Getenv("INSTANCE_KLINE_DIR")
		if parent == "" {
			parent = filepath.Join(filepath.Dir(instance.Dir()), "klines")
		}
		dir = filepath.Join(parent, uuid)
	}
	store, err := openKlineStore(dir)
	if err != nil {
		return nil, err
	}
	t := newKlineTracker(c, store)
	if v := os.Getenv("RE_KLINE_CAPTURE"); v == "1" || v == "true" {
		where := dir
		if where == "" {
			where = "in memory"
		}
		log.Printf("reality-engine: K-line capture on, store %s", where)
		if err := t.startCapture(); err != nil {
			return nil, err
		}
	}
	// A population over budget from an earlier run warns from boot.
	t.refreshBudget()
	return t, nil
}

// klineFailed: a store that cannot be written stops capture rather than the
// engine, and says so; the steps themselves are unaffected.
func (e *Engine) klineFailed(err error) {
	log.Printf("reality-engine: K-line capture stopped: %v", err)
	e.klines.controls.capture = false
}

// machineRemoved closes the machine's open K-lines interrupted
// ("machine-removed"). The caller holds the write lock.
func (e *Engine) machineRemoved(id string) {
	m, ok := e.machines[id]
	if !ok || !e.klines.controls.capture {
		return
	}
	if err := e.klines.onMachineRemoved(m.Name); err != nil {
		e.klineFailed(err)
	}
}

// refreshCorpusFacts recomputes the resident corpus's fingerprint and longest
// initial-to-final path when machines were added or removed.
func (e *Engine) refreshCorpusFacts() {
	if e.fingerprintVersion == e.mappingVersion {
		return
	}
	var declared [][2]any
	longest := 1
	var walk func(events map[string]*machine.Event, id string, seen map[string]bool) int
	walk = func(events map[string]*machine.Event, id string, seen map[string]bool) int {
		ev, ok := events[id]
		if !ok {
			return 0
		}
		seen[id] = true
		best := 0
		for _, n := range ev.NextIDs {
			if !seen[n] {
				best = max(best, walk(events, n, seen))
			}
		}
		delete(seen, id)
		return best + 1
	}
	for _, m := range e.machines {
		var body any = map[string]any{"name": m.Name}
		if m.Raw != nil {
			body = m.Raw
		}
		declared = append(declared, [2]any{m.Name, body})
		for _, seq := range m.Sequences {
			for id, ev := range seq.Events {
				if ev.Initial {
					longest = max(longest, walk(seq.Events, id, map[string]bool{}))
				}
			}
		}
	}
	e.corpusFingerprint = corpusFingerprint(declared)
	e.klines.longestPath = longest
	e.fingerprintVersion = e.mappingVersion
}

// captureKlines hands a committed step to the tracker. The caller holds the
// write lock.
func (e *Engine) captureKlines(n int, commit uint64, results map[string]map[string]any, isre, osre trajectoryEntry, merges []mergeOperation) {
	if !e.klines.controls.capture {
		return
	}
	e.refreshCorpusFacts()
	ops := make([]klineOp, len(merges))
	for i, m := range merges {
		ops[i] = klineOp{m.machineName, kRegion{m.offset, m.length}, m.values, m.sequenceIDs}
	}
	view := stepView{step: n, commit: commit, machines: e.machines, results: results, isre: isre, osre: osre,
		ops: ops, fingerprint: e.corpusFingerprint}
	if err := e.klines.onStep(view); err != nil {
		e.klineFailed(err)
	}
}

// Klines is GET /api/klines.
func (e *Engine) Klines(outcome, machineName, sequence *string, since uint64, limit int) map[string]any {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.klines.list(outcome, machineName, sequence, since, limit)
}

// KlinesOpen is GET /api/klines/open.
func (e *Engine) KlinesOpen() map[string]any {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.klines.openJSON()
}

// KlinesStats is GET /api/klines/stats.
func (e *Engine) KlinesStats() map[string]any {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.klines.store.stats(e.klines.controls)
}

// Kline is GET /api/klines/:id.
func (e *Engine) Kline(id string) (map[string]any, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if rec, ok := e.klines.store.record(id); ok {
		return rec, nil
	}
	return nil, machine.NotFound("no K-line %s", id)
}

// ExportKlines is POST /api/klines/export: a re-kline-bundle/1.
func (e *Engine) ExportKlines(outcomes map[string]bool, since uint64, machines map[string]bool) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.klines.exportBundle(outcomes, since, machines)
}

// KlineWarnings are the store's budget warnings for /api/health, read without
// the engine's lock: health never waits on a step.
func (e *Engine) KlineWarnings() []string {
	return e.klines.store.cachedWarnings()
}
