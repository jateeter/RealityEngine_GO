package engine

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"

	"github.com/jateeter/RealityEngine_GO/internal/machine"
)

// The engine's own sequence store, /api/sequences (RealityEngine_GO#10), and
// the corpus semantic bus registry, /api/buses/semantic.
//
// The sequence store is engine-scoped scratch, separate from the resident
// machines: a sequence posted here exists on this engine and no other, so a
// 404 says only that this engine does not hold it (SURFACE_SPEC "GET
// /api/vectors/:id — internal, and engine-scoped", RealityEngine_CI#397).
// POST /api/sequences/persist has nothing to persist to and answers success,
// as C++ does.

type storedSequence struct {
	seq       *machine.Sequence
	createdMS int64
}

func mint(prefix string) string { return prefix + "-" + newUUID() }

func (s storedSequence) view() map[string]any {
	return s.seq.View(machine.DefaultMatch, s.createdMS)
}

func sequenceNotFound() error { return machine.NotFound("Sequence not found") }

// Sequences is GET /api/sequences: the store in id order.
func (e *Engine) Sequences() map[string]any {
	e.mu.RLock()
	defer e.mu.RUnlock()
	ids := make([]string, 0, len(e.sequences))
	for id := range e.sequences {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	views := make([]any, len(ids))
	for i, id := range ids {
		views[i] = e.sequences[id].view()
	}
	return map[string]any{"sequences": views}
}

// ValidationError is a 400 whose error is a list of every failure.
type ValidationError struct{ Errors []string }

func (v *ValidationError) Error() string { return v.Errors[0] }

// CreateSequence is POST /api/sequences.
func (e *Engine) CreateSequence(body map[string]any) (map[string]any, error) {
	seq, err := machine.SequenceFromRequest(body, mint)
	if err != nil {
		return nil, err
	}
	if errs := seq.ValidationErrors(); len(errs) > 0 {
		return nil, &ValidationError{errs}
	}
	stored := storedSequence{seq: seq, createdMS: clock.NowMS()}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sequences[seq.ID] = stored
	return map[string]any{"success": true, "sequence": stored.view()}, nil
}

// Sequence is GET /api/sequences/:id.
func (e *Engine) Sequence(id string) (map[string]any, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	s, ok := e.sequences[id]
	if !ok {
		return nil, sequenceNotFound()
	}
	return map[string]any{"sequence": s.view()}, nil
}

// DeleteSequence is DELETE /api/sequences/:id.
func (e *Engine) DeleteSequence(id string) (map[string]any, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.sequences[id]; !ok {
		return nil, sequenceNotFound()
	}
	delete(e.sequences, id)
	return map[string]any{"success": true}, nil
}

// ResetSequence is POST /api/sequences/:id/reset: every event back to its
// initial activation, wasJustMatched cleared.
func (e *Engine) ResetSequence(id string) (map[string]any, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	s, ok := e.sequences[id]
	if !ok {
		return nil, sequenceNotFound()
	}
	for _, ev := range s.seq.Events {
		ev.Active = ev.Initial
		ev.WasJustMatched = false
	}
	return map[string]any{"success": true}, nil
}

// AddEvent is POST /api/sequences/:id/vectors: the event is built (and its
// comparators checked) before the sequence is looked up, as C++ orders it.
func (e *Engine) AddEvent(id string, body map[string]any) (map[string]any, error) {
	ev, err := machine.EventFromRequest(body, mint)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	s, ok := e.sequences[id]
	if !ok {
		return nil, sequenceNotFound()
	}
	s.seq.Events[ev.ID] = ev
	return map[string]any{"success": true, "vector": ev.View(machine.DefaultMatch, s.createdMS)}, nil
}

// semanticBusRegistryPath is SEMANTIC_BUS_REGISTRY, else
// domains/semantic-bus-registry.json in the corpus directory or one of the
// five directories above it.
func (e *Engine) semanticBusRegistryPath() string {
	if path := os.Getenv("SEMANTIC_BUS_REGISTRY"); path != "" {
		return path
	}
	base, err := filepath.Abs(e.machinesDir)
	if err != nil || e.machinesDir == "" {
		base = ""
	}
	for dir, i := base, 0; dir != "" && i < 6; i++ {
		candidate := filepath.Join(dir, "domains", "semantic-bus-registry.json")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return filepath.Join(filepath.Dir(base), "domains", "semantic-bus-registry.json")
}

// SemanticBusRegistry is GET /api/buses/semantic: the corpus registry as it
// lies on disk. Unreadable, or without a semanticBuses array, it is a 404.
func (e *Engine) SemanticBusRegistry() (map[string]any, error) {
	path := e.semanticBusRegistryPath()
	unavailable := func(why string) error {
		return machine.NotFound("semantic bus registry unavailable: %s", why)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, unavailable("cannot open " + path)
	}
	// Numbers pass through as written (1.0 stays 1.0).
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var registry map[string]any
	if err := decoder.Decode(&registry); err != nil {
		return nil, unavailable(err.Error())
	}
	if _, ok := registry["semanticBuses"].([]any); !ok {
		return nil, unavailable("invalid registry shape at " + path)
	}
	return registry, nil
}

// SemanticBus is GET /api/buses/semantic/:id.
func (e *Engine) SemanticBus(id string) (map[string]any, error) {
	registry, err := e.SemanticBusRegistry()
	if err != nil {
		return nil, err
	}
	for _, b := range registry["semanticBuses"].([]any) {
		if bus, ok := b.(map[string]any); ok && bus["id"] == id {
			return map[string]any{"bus": bus}, nil
		}
	}
	return nil, machine.NotFound("Semantic bus not found")
}
