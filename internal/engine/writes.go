package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jateeter/RealityEngine_GO/internal/instance"
	"github.com/jateeter/RealityEngine_GO/internal/machine"
)

// The machine write, transition and checkpoint routes (RealityEngine_GO#10).
// SURFACE_SPEC.md's Machines section is the contract; RealityEngine_Rust's
// machine_routes.rs, which follows C++'s concrete form, is prior art.
//
//   - POST always ingests: a resident name is versioned and its regions are
//     reallocated; a declared id is kept when free, else one is minted.
//   - PUT replaces the machine at :id with the body, keeping the id; PATCH
//     merges name, description and metadata into the declaration. Both
//     rebuild the machine, so its Reality Events start over, and both keep its
//     transitionsInhibited and its corpus file.
//   - json/:name loads a corpus file by basename under its canonical id,
//     replacing the machine holding it; json/import ingests under a minted id.
//   - process drives the machine; whatif drives an uninhibited copy and
//     persists nothing. The -universal forms slice a universal input space at
//     the machine's input region first.
//   - A checkpoint is a copy of the machine as it is running; restoring it
//     replaces the resident machine with that copy.

type checkpoint struct {
	label     string
	timestamp int64
	machine   *machine.Machine
}

// clock is what a transition stamps: the time, and an id for each output the
// arbiter picks.
var clock = machine.Clock{
	NowMS:  func() int64 { return time.Now().UnixMilli() },
	MintID: func() string { return newUUID() },
}

// newUUID is a version 7 UUID.
func newUUID() string { return instance.NewV7() }

func mintMachineID() string { return "machine-" + newUUID() }

func nameIsResident(machines map[string]*machine.Machine, name string) bool {
	for _, m := range machines {
		if m.Name == name {
			return true
		}
	}
	return false
}

// ingest is POST /api/machines's rule, and boot's: version a resident name
// and reallocate its regions at the end of the space, keep a free declared id
// or mint one, then admit. The caller holds the write lock.
func (e *Engine) ingest(m *machine.Machine) {
	if nameIsResident(e.machines, m.Name) {
		base := machine.VersionBase(m.Name)
		for v := 2; ; v++ {
			candidate := fmt.Sprintf("%s v%d", base, v)
			if nameIsResident(e.machines, candidate) {
				continue
			}
			m.VersionName(candidate, e.dimension)
			if _, output, ok := m.Regions(); ok {
				e.dimension += m.InputLength + output.Length
			}
			break
		}
	}
	if _, held := e.machines[m.ID]; m.ID == "" || held {
		m.ID = mintMachineID()
	}
	e.admit(m)
}

// admit makes m resident under its own id, replacing any machine holding it,
// and widens the space to its mapping.
func (e *Engine) admit(m *machine.Machine) {
	m.Raw["id"] = m.ID
	if input, output, ok := m.Regions(); ok {
		e.dimension = max(e.dimension, m.InputOffset+m.InputLength, output.Offset+output.Length, input.Offset)
	}
	e.machines[m.ID] = m
	e.mappingVersion++
}

// readmit replaces the machine at id with m, keeping the resident one's
// control state and corpus file (a PUT names the same machine).
func (e *Engine) readmit(id string, m *machine.Machine) map[string]any {
	m.ID = id
	if previous, ok := e.machines[id]; ok {
		m.TransitionsInhibited = previous.TransitionsInhibited
		m.SourceFile = previous.SourceFile
		delete(e.machines, id)
	}
	e.admit(m)
	return map[string]any{"success": true, "machine": m.View(true)}
}

func declaredName(body any) any {
	obj, _ := body.(map[string]any)
	if inner, ok := obj["machine"].(map[string]any); ok {
		return inner["name"]
	}
	return obj["name"]
}

// Add is POST /api/machines: {success, versioned, machine}, the machine as
// ingested in full.
func (e *Engine) Add(body []byte) (map[string]any, error) {
	m, err := machine.Parse(body)
	if err != nil {
		return nil, err
	}
	requested := m.Name
	m.LoadedAtMS = clock.NowMS()
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ingest(m)
	return map[string]any{"success": true, "versioned": m.Name != requested, "machine": m.View(true)}, nil
}

// Delete is DELETE /api/machines/:id.
func (e *Engine) Delete(id string) (map[string]any, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.machines[id]; !ok {
		return nil, machine.NotFound("machine %s not found", id)
	}
	delete(e.machines, id)
	e.mappingVersion++
	return map[string]any{"success": true, "id": id}, nil
}

// Replace is PUT /api/machines/:id.
func (e *Engine) Replace(id string, body []byte) (map[string]any, error) {
	m, err := machine.Parse(emptyIsObject(body))
	if err != nil {
		return nil, err
	}
	m.LoadedAtMS = clock.NowMS()
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.readmit(id, m), nil
}

// Patch is PATCH /api/machines/:id: name and description when they are
// strings, metadata merged key by key.
func (e *Engine) Patch(id string, body map[string]any) (map[string]any, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	resident, ok := e.machines[id]
	if !ok {
		return nil, machine.NotFound("Machine not found")
	}
	declaration := resident.Declaration()
	for _, key := range []string{"name", "description"} {
		if text, ok := body[key].(string); ok {
			declaration[key] = text
		}
	}
	if changes, ok := body["metadata"].(map[string]any); ok {
		meta, ok := declaration["metadata"].(map[string]any)
		if !ok {
			meta = map[string]any{}
			declaration["metadata"] = meta
		}
		for k, v := range changes {
			meta[k] = v
		}
	}
	m, err := machine.FromObject(declaration)
	if err != nil {
		return nil, err
	}
	m.LoadedAtMS = clock.NowMS()
	return e.readmit(id, m), nil
}

// Import is POST /api/machines/json/import: the definition under a minted id.
func (e *Engine) Import(body []byte) (map[string]any, error) {
	m, err := machine.Parse(emptyIsObject(body))
	if err != nil {
		return nil, err
	}
	m.LoadedAtMS = clock.NowMS()
	e.mu.Lock()
	defer e.mu.Unlock()
	m.ID = mintMachineID()
	e.admit(m)
	return map[string]any{"success": true, "machine": m.View(true)}, nil
}

// LoadByName is GET /api/machines/json/:name: a corpus file by basename, the
// flat path first and then a recursive search (corpus stems are globally
// unique). The machine takes its canonical id and replaces any machine
// holding it, as boot would have; a file that does not load leaves the
// resident machine.
func (e *Engine) LoadByName(name string) (map[string]any, error) {
	if strings.Contains(name, "..") || strings.ContainsAny(name, `/\`) {
		return nil, machine.BadRequest("Invalid machine name: %s", name)
	}
	filename := name
	if !strings.HasSuffix(filename, ".json") {
		filename += ".json"
	}
	notFound := machine.NotFound("Machine file not found: %s", name)
	if e.machinesDir == "" {
		return nil, notFound
	}
	path := filepath.Join(e.machinesDir, filename)
	if st, err := os.Stat(path); err != nil || !st.Mode().IsRegular() {
		path = ""
		var paths []string
		_ = filepath.WalkDir(e.machinesDir, func(p string, d os.DirEntry, err error) error {
			if err == nil && d.Type().IsRegular() && d.Name() == filename {
				paths = append(paths, p)
			}
			return nil
		})
		sort.Strings(paths)
		if len(paths) == 0 {
			return nil, notFound
		}
		path = paths[0]
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, notFound
	}
	m, err := machine.Parse(data)
	if err != nil {
		return nil, machine.BadRequest("invalid machine JSON in %s: %v", filename, err)
	}
	base := filepath.Base(path)
	m.ID = machine.CanonicalID(strings.TrimSuffix(base, ".json"))
	m.LoadedAtMS = clock.NowMS()
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.machines, m.ID)
	e.ingest(m)
	m.SourceFile = base
	return map[string]any{"success": true, "machine": m.Summary(), "message": "Machine loaded successfully"}, nil
}

// Process is POST /api/machines/:id/process: the transition result, without
// machineId (the caller named the machine).
func (e *Engine) Process(id string, input []float64) (map[string]any, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.process(id, input)
}

func (e *Engine) process(id string, input []float64) (map[string]any, error) {
	m, ok := e.machines[id]
	if !ok {
		return nil, machine.NotFound("machine %s not found", id)
	}
	if m.InputLength != 0 && len(input) != m.InputLength {
		return nil, machine.BadRequest("input vector length %d does not match machine input length %d", len(input), m.InputLength)
	}
	result := m.Respond(input, "", clock)
	delete(result, "machineId")
	return result, nil
}

// ProcessUniversal is POST /api/machines/:id/process-universal.
func (e *Engine) ProcessUniversal(id string, universal []float64) (map[string]any, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	m, ok := e.machines[id]
	if !ok {
		return nil, machine.NotFound("Machine not found")
	}
	input, err := m.Resolve(universal)
	if err != nil {
		return nil, err
	}
	return e.process(id, input)
}

// ProcessUniversalAll is POST /api/machines/process-universal/all: every
// machine with a perceptual mapping, by id.
func (e *Engine) ProcessUniversalAll(universal []float64) (map[string]any, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	results := map[string]any{}
	ids := make([]string, 0, len(e.machines))
	for id := range e.machines {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		m := e.machines[id]
		if _, _, ok := m.Regions(); !ok {
			continue
		}
		input, err := m.Resolve(universal)
		if err != nil {
			return nil, err
		}
		result, err := e.process(id, input)
		if err != nil {
			return nil, err
		}
		results[id] = result
	}
	return map[string]any{"results": results}, nil
}

// WhatIf is POST /api/machines/:id/whatif; with universal true, the
// -universal form.
func (e *Engine) WhatIf(id string, values []float64, universal bool) (map[string]any, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	m, ok := e.machines[id]
	if !ok {
		return nil, machine.NotFound("Machine not found")
	}
	input := values
	if universal {
		var err error
		if input, err = m.Resolve(values); err != nil {
			return nil, err
		}
	}
	return m.WhatIf(input, clock), nil
}

// Checkpoints is GET /api/machines/:id/checkpoints: a bare array of
// {id, label, timestamp} in id order; empty for a machine with none.
func (e *Engine) Checkpoints(id string) []any {
	e.mu.RLock()
	defer e.mu.RUnlock()
	byID := e.checkpoints[id]
	ids := make([]string, 0, len(byID))
	for cp := range byID {
		ids = append(ids, cp)
	}
	sort.Strings(ids)
	out := make([]any, len(ids))
	for i, cp := range ids {
		out[i] = map[string]any{"id": cp, "label": byID[cp].label, "timestamp": byID[cp].timestamp}
	}
	return out
}

// Checkpoint is POST /api/machines/:id/checkpoints.
func (e *Engine) Checkpoint(id, label string) (map[string]any, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	m, ok := e.machines[id]
	if !ok {
		return nil, machine.NotFound("Machine not found")
	}
	cpID := "checkpoint-" + newUUID()
	if e.checkpoints[id] == nil {
		e.checkpoints[id] = map[string]checkpoint{}
	}
	e.checkpoints[id][cpID] = checkpoint{label: label, timestamp: clock.NowMS(), machine: m.Clone()}
	return map[string]any{"success": true, "checkpointId": cpID}, nil
}

// Restore is POST /api/machines/:machineId/checkpoints/:cpId/restore. The
// checkpoint stays, so it can be restored again.
func (e *Engine) Restore(id, cpID string) (map[string]any, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	cp, ok := e.checkpoints[id][cpID]
	if !ok {
		return nil, machine.NotFound("Checkpoint not found")
	}
	delete(e.machines, id)
	e.admit(cp.machine.Clone())
	return map[string]any{"success": true}, nil
}

// DeleteCheckpoint is DELETE /api/machines/:machineId/checkpoints/:cpId:
// {success} says whether there was one to delete.
func (e *Engine) DeleteCheckpoint(id, cpID string) map[string]any {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, ok := e.checkpoints[id][cpID]
	delete(e.checkpoints[id], cpID)
	return map[string]any{"success": ok}
}

func emptyIsObject(body []byte) []byte {
	if len(strings.TrimSpace(string(body))) == 0 {
		return []byte("{}")
	}
	return body
}
