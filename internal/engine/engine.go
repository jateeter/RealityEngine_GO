// Package engine is the Reality Engine's state: the resident machines and the
// corpus directory they came from. Routes read it under a read lock and change
// it under a write lock.
package engine

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jateeter/RealityEngine_GO/internal/instance"
	"github.com/jateeter/RealityEngine_GO/internal/machine"
)

// Engine holds the resident machines.
type Engine struct {
	mu          sync.RWMutex
	machines    map[string]*machine.Machine
	machinesDir string
	// dimension is the perceptual space's width: VECTOR_DIMENSION, grown to
	// fit every machine admitted. A versioned machine's regions are appended
	// at its end.
	dimension int
	// checkpoints are copies of machines as they were running, by machine id
	// and checkpoint id.
	checkpoints map[string]map[string]checkpoint
	// sequences is the engine's own sequence store (/api/sequences).
	sequences map[string]storedSequence
	// instance is the instance's participant clock (#296, #600), shared with
	// the request middleware, which ticks it on every receive and send.
	instance *instance.Clock
	// lastCommit is (lamport, commit) at the newest committed step: what
	// GET /api/engine/clock and a step's reply report.
	lastCommit [2]uint64
	// history is the /api/engine/process audit trail, newest first.
	history []any
	// mappingVersion is bumped on every machine admitted or removed.
	mappingVersion int
	// completedStep is the newest committed step, -1 before the first since
	// boot or reset.
	completedStep int
	// steps is the perceptual space, the trajectory histories and the
	// arbiter (step.go).
	steps stepState
	// threshold is PUT /api/config/threshold's value; no comparator reads it.
	threshold float64
	// vectors is the engine-scoped document store (/api/vectors).
	vectors map[string]map[string]any
	// sim is the perceptual simulation; sampler the sampler's bookkeeping,
	// which a reset keeps.
	sim     simulation
	sampler sampler
	// coverage is the CES coverage /api/metrics exposes; a reset keeps it.
	coverage coverage
	// clones is the clone space and clone.maxActive (clones.go).
	clones cloneSpace
	// machineJSONSummary makes GET /api/machines/json/:name serve the list
	// form instead of the full form (RealityEngine_CI#886).
	machineJSONSummary bool
}

// DefaultDimension is the perceptual space's width when VECTOR_DIMENSION is
// not set.
const DefaultDimension = 7680

// New loads every machine under machinesDir (MACHINES_DIR) into a space of
// the given dimension. An empty directory name starts an engine with no
// machines. A file that does not load is logged with its reason and skipped,
// never silently. Boot ingests each file as POST /api/machines would, under
// its canonical id, so a name held twice is versioned the same way.
func New(machinesDir string, dimension int) (*Engine, error) {
	e := &Engine{
		machines: map[string]*machine.Machine{}, machinesDir: machinesDir,
		dimension: dimension, checkpoints: map[string]map[string]checkpoint{},
		sequences: map[string]storedSequence{}, instance: instance.Minted(), completedStep: -1,
		steps: newStepState(machinesDir), threshold: defaultThreshold, vectors: map[string]map[string]any{},
		sampler: newSampler(), coverage: newCoverage(), clones: newCloneSpace(),
	}
	if machinesDir == "" {
		return e, nil
	}
	loaded, failed, err := machine.LoadDir(machinesDir, time.Now().UnixMilli())
	if err != nil {
		return nil, err
	}
	for _, l := range loaded {
		e.ingest(l.Machine)
	}
	for _, f := range failed {
		log.Printf("reality-engine: not loaded: %v", f)
	}
	log.Printf("reality-engine: %d machines loaded from %s, %d files not loaded", len(loaded), machinesDir, len(failed))
	return e, nil
}

// FromEnvironment is New(MACHINES_DIR, VECTOR_DIMENSION), the dimension
// DefaultDimension when unset or not a whole number, under the instance clock
// INSTANCE_UUID and INSTANCE_CLOCK_DIR give. The clock boots first, so an
// allocated instance that cannot keep its clock, or whose UUID another live
// process holds, fails at once rather than after the corpus loads.
func FromEnvironment() (*Engine, error) {
	c, err := instance.Boot()
	if err != nil {
		return nil, err
	}
	log.Printf("reality-engine: instance %s, lamport %d, commit %d", c.Instance(), c.Lamport(), c.Commit())
	dimension := DefaultDimension
	if v, err := strconv.Atoi(os.Getenv("VECTOR_DIMENSION")); err == nil && v >= 0 {
		dimension = v
	}
	e, err := New(os.Getenv("MACHINES_DIR"), dimension)
	if err != nil {
		c.Close()
		return nil, err
	}
	e.instance = c
	e.lastCommit = [2]uint64{c.Lamport(), c.Commit()}
	return e, nil
}

// InstanceClock is the engine's participant clock, for the request middleware.
func (e *Engine) InstanceClock() *instance.Clock {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.instance
}

// clockJSON is a committed step's clock: {instance, lamport, commit, step}.
// The caller holds mu.
func (e *Engine) clockJSON(at [2]uint64, step int) map[string]any {
	return map[string]any{"instance": e.instance.Instance(), "lamport": at[0], "commit": at[1], "step": step}
}

// List is GET /api/machines: every resident machine, mapped or not, in
// canonical order (metadata.domain, then name, then id), in the list form.
func (e *Engine) List() map[string]any {
	e.mu.RLock()
	defer e.mu.RUnlock()
	ms := make([]*machine.Machine, 0, len(e.machines))
	for _, m := range e.machines {
		ms = append(ms, m)
	}
	machine.CanonicalOrder(ms)
	views := make([]any, len(ms))
	for i, m := range ms {
		views[i] = m.View(false)
	}
	return map[string]any{"machines": views}
}

// Machine is GET /api/machines/:id: the machine in full, as it is running.
func (e *Engine) Machine(id string) (map[string]any, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	m, ok := e.machines[id]
	if !ok {
		return nil, false
	}
	return map[string]any{"machine": m.View(true)}, true
}

// Export is GET /api/machines/:id/export: the corpus document.
func (e *Engine) Export(id string) (map[string]any, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	m, ok := e.machines[id]
	if !ok {
		return nil, false
	}
	return map[string]any{"version": "1.0.0", "machine": m.CorpusDocument()}, true
}

// Catalog is GET /api/machines/json/list: every .json file under the corpus
// directory, recursively, in path order, with its relFile. Each row carries
// the corpus OWL semantics (semanticsIri, semanticsHash) when the ABox
// manifest names it. Without a corpus directory the catalog is empty.
func (e *Engine) Catalog() (map[string]any, error) {
	rows := []any{}
	if e.machinesDir == "" {
		return map[string]any{"machines": rows}, nil
	}
	if _, err := os.Stat(e.machinesDir); err != nil {
		return map[string]any{"machines": rows}, nil
	}
	var paths []string
	err := filepath.WalkDir(e.machinesDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() && strings.HasSuffix(d.Name(), ".json") {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("could not read %s: %w", e.machinesDir, err)
	}
	sort.Strings(paths)
	semantics := semanticsManifest(e.machinesDir)
	for _, path := range paths {
		rel, err := filepath.Rel(e.machinesDir, path)
		if err != nil {
			continue
		}
		rel = filepath.ToSlash(rel)
		base := filepath.Base(path)
		row := map[string]any{
			"filename": base, "relFile": rel, "name": strings.TrimSuffix(base, ".json"),
			"description": "", "version": "1.0.0", "metadata": map[string]any{}, "sequenceCount": 0,
		}
		// Manifest keys are "<domain>/<stem>": domains/<d>/X.json is d/X, else core/X.
		key := "core/" + rel
		if strings.HasPrefix(rel, "domains/") {
			key = strings.TrimPrefix(rel, "domains/")
		}
		key = strings.TrimSuffix(key, ".json")
		if entry, ok := semantics[key].(map[string]any); ok {
			row["semanticsIri"] = entry["iri"]
			row["semanticsHash"] = entry["sha256"]
		}
		rows = append(rows, row)
	}
	return map[string]any{"machines": rows}, nil
}

// semanticsManifest is the corpus ABox manifest's "machines" object:
// SEMANTICS_MANIFEST, else the first semantics/abox-manifest.json found walking
// up to six levels from the machines directory. Nil when there is none.
func semanticsManifest(machinesDir string) map[string]any {
	path := os.Getenv("SEMANTICS_MANIFEST")
	if path == "" {
		dir, err := filepath.Abs(machinesDir)
		if err != nil {
			return nil
		}
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			dir = resolved
		}
		for i := 0; i < 6; i++ {
			candidate := filepath.Join(dir, "semantics", "abox-manifest.json")
			if st, err := os.Stat(candidate); err == nil && st.Mode().IsRegular() {
				path = candidate
				break
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var doc map[string]any
	if json.Unmarshal(data, &doc) != nil {
		return nil
	}
	machines, _ := doc["machines"].(map[string]any)
	return machines
}
