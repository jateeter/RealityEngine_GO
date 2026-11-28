package engine

// Runtime snapshots (RealityEngine_CI#287, phase 2; this repo's #17).
// SURFACE_SPEC.md, "The snapshot body" and "Routes": a snapshot is a K-line
// over retained steps plus what it takes to re-establish it on an engine
// holding the same corpus, re-snapshot/1, runtime-independent, its digest
// over the JCS form. A port of C++ snapshot.cpp.
//
// Retention keeps, for every retained step (historyLimit), the active list
// after the step, its contributions and its clock, whether or not K-line
// capture is on.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jateeter/RealityEngine_GO/internal/machine"
)

// retainedSnapshotStep is one retained step. Its clock is the instance that
// committed it and its lamport (RealityEngine_CI#904).
type retainedSnapshotStep struct {
	step       int
	instance   string
	lamport    uint64
	isre, osre trajectoryEntry
	ops        []klineOp
	after      []activeEntry
}

// retention is every retained step's active list, contributions and clock.
type retention struct {
	base    []activeEntry
	baseSet bool // a restore sets it; otherwise the base is taken at the first step
	steps   []retainedSnapshotStep
	// restored is what a restore re-established, until the next step (#904):
	// a capture returns it, so restore then capture is an identity. Its clocks
	// keep the instance that committed those steps, which on a sandbox
	// replica is the source, never the replica.
	restored *restorePoint
}

type restorePoint struct {
	kline       map[string]any
	arbitration any
}

func allActives(machines map[string]*machine.Machine) []activeEntry {
	var out []activeEntry
	for _, id := range sortedMachineIDs(machines) {
		out = append(out, activesOf(machines[id])...)
	}
	return out
}

func sortedMachineIDs(machines map[string]*machine.Machine) []string {
	ids := make([]string, 0, len(machines))
	for id := range machines {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// onReset: the base is taken at the first step after it, every machine at its
// initial events.
func (r *retention) onReset() {
	r.base, r.baseSet, r.steps, r.restored = nil, false, nil, nil
}

func (r *retention) onStep(step int, instance string, lamport uint64, isre, osre trajectoryEntry, ops []klineOp,
	machines map[string]*machine.Machine, limit int) {
	r.restored = nil
	if len(r.steps) == 0 && !r.baseSet {
		r.base = nil
		for _, id := range sortedMachineIDs(machines) {
			r.base = append(r.base, initialActives(machines[id])...)
		}
		r.baseSet = true
	}
	r.steps = append(r.steps, retainedSnapshotStep{step, instance, lamport, isre, osre, ops, allActives(machines)})
	// Keep historyLimit steps; the newest one leaving becomes the base for the next.
	for len(r.steps) > max(1, limit) {
		r.base = r.steps[0].after
		r.steps = r.steps[1:]
	}
}

// restore: after any restore nothing is retained, the live list is the next
// step's base, and the restored K-line is the restore point until that step.
func (r *retention) restore(live []activeEntry, kline map[string]any, arbitration any) {
	r.base, r.baseSet, r.steps = live, true, nil
	r.restored = &restorePoint{kline, arbitration}
}

func (r *retention) at(step int) *retainedSnapshotStep {
	for i := range r.steps {
		if r.steps[i].step == step {
			return &r.steps[i]
		}
	}
	return nil
}

// before is the list before step: after step-1, or the base when it is the first.
func (r *retention) before(step int) ([]activeEntry, bool) {
	if prev := r.at(step - 1); prev != nil {
		return prev.after, true
	}
	if len(r.steps) == 0 || r.steps[0].step == step {
		return r.base, true
	}
	return nil, false
}

func snapshotEntryJSON(e trajectoryEntry, keep []kRegion, scoped bool) map[string]any {
	nz := []any{}
	for _, c := range e.nonZero {
		in := !scoped
		for _, r := range keep {
			if c.index >= r.offset && c.index < r.offset+r.length {
				in = true
				break
			}
		}
		if in {
			nz = append(nz, map[string]any{"index": c.index, "value": c.value})
		}
	}
	return map[string]any{"stepNumber": e.stepNumber, "length": e.length, "nonZero": nz}
}

func entryFrom(v any) trajectoryEntry {
	m, _ := v.(map[string]any)
	e := trajectoryEntry{stepNumber: int(num(m["stepNumber"])), length: int(num(m["length"]))}
	cells, _ := m["nonZero"].([]any)
	for _, c := range cells {
		cm, _ := c.(map[string]any)
		e.nonZero = append(e.nonZero, cellValue{int(num(cm["index"])), num(cm["value"])})
	}
	return e
}

func onlyScope(list []activeEntry, scope map[string]bool) []activeEntry {
	if scope == nil {
		return list
	}
	var out []activeEntry
	for _, e := range list {
		if scope[e.machine] {
			out = append(out, e)
		}
	}
	return out
}

// digestOf is "sha256:<hex>" over the body without digest and the clocks'
// instance and lamport.
func digestOf(body map[string]any) string {
	copied, _ := normalize(body).(map[string]any)
	delete(copied, "digest")
	if kl, ok := copied["kline"].(map[string]any); ok {
		if clocks, ok := kl["clocks"].(map[string]any); ok {
			for _, end := range []string{"from", "to"} {
				if c, ok := clocks[end].(map[string]any); ok {
					delete(c, "instance")
					delete(c, "lamport")
				}
			}
		}
	}
	return "sha256:" + sha256Hex([]byte(jcs(copied)))
}

// normalize is a deep copy as generic JSON (map[string]any, []any, float64).
func normalize(v any) any {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var out any
	_ = json.Unmarshal(raw, &out)
	return out
}

func snapshotID(body map[string]any) string {
	d, _ := body["digest"].(string)
	return strings.TrimPrefix(d, "sha256:")
}

func isSnapshotID(id string) bool {
	if id == "" {
		return false
	}
	for _, c := range id {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

// snapshotStore is RE_SNAPSHOT_STORE: memory (default), file:<dir>, off.
// qdrant:<url> is phase 3 and is off until then, never a boot failure.
type snapshotStore struct {
	mode   string // memory, file, off
	dir    string
	memory map[string]map[string]any
}

func snapshotStoreFromEnvironment() *snapshotStore {
	v := os.Getenv("RE_SNAPSHOT_STORE")
	s := &snapshotStore{mode: "memory", memory: map[string]map[string]any{}}
	switch {
	case v == "" || v == "memory":
	case v == "off":
		s.mode = "off"
	case strings.HasPrefix(v, "file:"):
		s.mode, s.dir = "file", strings.TrimPrefix(v, "file:")
		_ = os.MkdirAll(s.dir, 0o755)
	default:
		s.mode = "off"
	}
	return s
}

func (s *snapshotStore) enabled() bool { return s.mode != "off" }

func (s *snapshotStore) put(id string, body map[string]any) {
	if s.mode == "file" {
		tmp := filepath.Join(s.dir, id+".json.tmp")
		if os.WriteFile(tmp, []byte(jcs(body)+"\n"), 0o644) == nil {
			_ = os.Rename(tmp, filepath.Join(s.dir, id+".json"))
		}
		return
	}
	s.memory[id] = body
}

func (s *snapshotStore) get(id string) (map[string]any, bool) {
	if !isSnapshotID(id) {
		return nil, false
	}
	if s.mode == "file" {
		data, err := os.ReadFile(filepath.Join(s.dir, id+".json"))
		if err != nil {
			return nil, false
		}
		var m map[string]any
		if json.Unmarshal(data, &m) != nil {
			return nil, false
		}
		return m, true
	}
	m, ok := s.memory[id]
	return m, ok
}

func (s *snapshotStore) remove(id string) bool {
	if _, ok := s.get(id); !ok {
		return false
	}
	if s.mode == "file" {
		return os.Remove(filepath.Join(s.dir, id+".json")) == nil
	}
	delete(s.memory, id)
	return true
}

// list is {id, digest, scope, span, machines}, oldest first by clocks.to.lamport.
func (s *snapshotStore) list() map[string]any {
	var bodies []map[string]any
	if s.mode == "file" {
		entries, _ := os.ReadDir(s.dir)
		for _, e := range entries {
			if id, ok := strings.CutSuffix(e.Name(), ".json"); ok {
				if b, ok := s.get(id); ok {
					bodies = append(bodies, b)
				}
			}
		}
	} else {
		for _, b := range s.memory {
			bodies = append(bodies, b)
		}
	}
	lamport := func(b map[string]any) float64 {
		kl, _ := b["kline"].(map[string]any)
		clocks, _ := kl["clocks"].(map[string]any)
		to, _ := clocks["to"].(map[string]any)
		return num(to["lamport"])
	}
	sort.SliceStable(bodies, func(i, j int) bool { return lamport(bodies[i]) < lamport(bodies[j]) })
	out := []any{}
	for _, b := range bodies {
		kl, _ := b["kline"].(map[string]any)
		corpus, _ := b["corpus"].(map[string]any)
		out = append(out, map[string]any{"id": snapshotID(b), "digest": b["digest"], "scope": kl["scope"],
			"span": kl["span"], "machines": corpus["machines"]})
	}
	return map[string]any{"snapshots": out}
}

// SnapshotError is a snapshot route's refusal: its status and body.
type SnapshotError struct {
	Status int
	Body   map[string]any
}

func (e *SnapshotError) Error() string { return fmt.Sprint(e.Body["error"]) }

func refuse(status int, format string, args ...any) *SnapshotError {
	return &SnapshotError{status, map[string]any{"error": fmt.Sprintf(format, args...)}}
}

var errSnapshotsDisabled = refuse(http.StatusServiceUnavailable, "snapshots are disabled")

// snapshotState is the engine's controls by scope, and the retunable
// overrides, by corpus machine name. The caller holds the lock.
func (e *Engine) snapshotState() (map[string]any, map[string]any, map[string]any) {
	controls, machineControls, overrides := map[string]any{}, map[string]any{}, map[string]any{}
	for _, c := range e.controls() {
		name := c["name"].(string)
		if c["scope"] == "engine" {
			controls[name] = c["value"]
			continue
		}
		perID, ok := c["value"].(map[string]any)
		if !ok {
			continue
		}
		for id, v := range perID {
			m, ok := e.machines[id]
			if !ok {
				continue
			}
			per, _ := machineControls[m.Name].(map[string]any)
			if per == nil {
				per = map[string]any{}
				machineControls[m.Name] = per
			}
			per[name] = v
		}
	}
	for _, m := range e.machines {
		overrides[m.Name] = map[string]any{"arbiterRule": m.ArbiterRule, "outputMergeTransformation": m.OutputMerge,
			"outputMergeLocked": true}
	}
	return controls, machineControls, overrides
}

// CaptureSnapshot is POST /api/runtime/snapshot {scope?, from?, to?}.
func (e *Engine) CaptureSnapshot(body map[string]any) (map[string]any, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.snapshots.enabled() {
		return nil, errSnapshotsDisabled
	}
	known := map[string]bool{}
	for _, m := range e.machines {
		known[m.Name] = true
	}
	var scope map[string]bool
	switch v := body["scope"].(type) {
	case []any:
		scope = map[string]bool{}
		for _, n := range v {
			s, _ := n.(string)
			if !known[s] {
				return nil, refuse(http.StatusUnprocessableEntity, "no machine %s", s)
			}
			scope[s] = true
		}
	case nil:
	case string:
		if v != "corpus" {
			return nil, refuse(http.StatusUnprocessableEntity, `scope is "corpus" or a list of machine names`)
		}
	default:
		return nil, refuse(http.StatusUnprocessableEntity, `scope is "corpus" or a list of machine names`)
	}
	var ends [2]*int
	for i, end := range []string{"from", "to"} {
		v, present := body[end]
		if !present || v == nil {
			continue
		}
		n, ok := wholeIn(v, -1)
		if !ok {
			return nil, refuse(http.StatusUnprocessableEntity, "%s is a step number", end)
		}
		ends[i] = &n
	}
	e.refreshCorpusFacts()
	r := &e.retention
	if len(r.steps) == 0 && r.restored != nil {
		return e.captureRestored(r.restored, body, scope, ends)
	}
	if len(r.steps) == 0 {
		return nil, refuse(http.StatusGone, "no step is retained")
	}
	newest := r.steps[len(r.steps)-1].step
	to := newest
	if ends[1] != nil {
		to = *ends[1]
	} else if ends[0] != nil {
		to = *ends[0]
	}
	from := to
	if ends[0] != nil {
		from = *ends[0]
	}
	if from > to {
		return nil, refuse(http.StatusUnprocessableEntity, "span.from is after span.to")
	}
	for _, s := range []int{from, to} {
		if r.at(s) == nil {
			return nil, refuse(http.StatusGone, "step %d is no longer retained", s)
		}
	}
	before, ok := r.before(from)
	if !ok {
		return nil, refuse(http.StatusGone, "step %d is no longer retained", from-1)
	}

	// The scope's regions, and the output cells it shares with a machine outside it.
	var inputs, outputs, others []kRegion
	var scopeJSON any = "corpus"
	if scope != nil {
		names := make([]string, 0, len(scope))
		for n := range scope {
			names = append(names, n)
		}
		sort.Strings(names)
		list := make([]any, len(names))
		for i, n := range names {
			list[i] = n
		}
		scopeJSON = list
		for _, id := range sortedMachineIDs(e.machines) {
			m := e.machines[id]
			in, out, ok := regionsOf(m)
			if !ok {
				continue
			}
			if scope[m.Name] {
				inputs = append(inputs, in)
				outputs = append(outputs, out)
			} else {
				others = append(others, out)
			}
		}
	}
	shared := map[int]bool{}
	for _, o := range outputs {
		for _, x := range others {
			for c := max(o.offset, x.offset); c < min(o.offset+o.length, x.offset+x.length); c++ {
				shared[c] = true
			}
		}
	}
	sharedList := make([]int, 0, len(shared))
	for c := range shared {
		sharedList = append(sharedList, c)
	}
	sort.Ints(sharedList)
	sharedJSON := make([]any, len(sharedList))
	for i, c := range sharedList {
		sharedJSON[i] = c
	}
	pairs := []any{}
	for k := from; k <= to; k++ {
		s := r.at(k)
		if s == nil {
			return nil, refuse(http.StatusGone, "step %d is no longer retained", k)
		}
		var ops []klineOp
		for _, op := range s.ops {
			if scope == nil || scope[op.machine] {
				ops = append(ops, op)
			}
		}
		sort.SliceStable(ops, func(i, j int) bool {
			if ops[i].region.offset != ops[j].region.offset {
				return ops[i].region.offset < ops[j].region.offset
			}
			return ops[i].machine < ops[j].machine
		})
		contribs := []any{}
		for _, op := range ops {
			contribs = append(contribs, opJSON(op))
		}
		pairs = append(pairs, map[string]any{"stepNumber": k,
			"isre": snapshotEntryJSON(s.isre, inputs, scope != nil), "osre": snapshotEntryJSON(s.osre, outputs, scope != nil),
			"contributions": contribs})
	}
	clock := func(s *retainedSnapshotStep) map[string]any {
		return map[string]any{"instance": s.instance, "lamport": s.lamport, "step": s.step}
	}
	var arbitration any
	if e.steps.controls.arbitrationRetention {
		steps := []int{}
		for n := range e.steps.retained {
			if n >= from && n <= to {
				steps = append(steps, n)
			}
		}
		sort.Ints(steps)
		list := []any{}
		for _, n := range steps {
			list = append(list, e.retainedStepJSON(n, e.steps.retained[n]))
		}
		arbitration = list
	}
	return e.storeSnapshot(map[string]any{
		"scope":        scopeJSON,
		"span":         map[string]any{"from": from, "to": to},
		"clocks":       map[string]any{"from": clock(r.at(from)), "to": clock(r.at(to))},
		"activeBefore": activesJSON(onlyScope(before, scope)),
		"pairs":        pairs,
		"activeAfter":  activesJSON(onlyScope(r.at(to).after, scope)),
		"sharedCells":  sharedJSON,
	}, arbitration)
}

// storeSnapshot makes the body around a K-line, with the engine's corpus,
// controls and overrides, digests and stores it.
func (e *Engine) storeSnapshot(kline map[string]any, arbitration any) (map[string]any, error) {
	controls, machineControls, overrides := e.snapshotState()
	snap := map[string]any{
		"format": "re-snapshot/1",
		"corpus": map[string]any{"algorithm": "sha256/resident-declared-v1", "fingerprint": e.corpusFingerprint,
			"machines": len(e.machines)},
		"controls": controls, "machineControls": machineControls, "machineOverrides": overrides,
		"kline":       kline,
		"arbitration": arbitration,
	}
	snap, _ = normalize(snap).(map[string]any)
	snap["digest"] = digestOf(snap)
	id := snapshotID(snap)
	e.snapshots.put(id, snap)
	kl := snap["kline"].(map[string]any)
	return map[string]any{"id": id, "digest": snap["digest"], "span": kl["span"]}, nil
}

// captureRestored is a capture right after a restore (#904): the restored
// K-line, when the request asks for its scope and span or leaves them out.
func (e *Engine) captureRestored(rp *restorePoint, body map[string]any, scope map[string]bool,
	ends [2]*int) (map[string]any, error) {
	var restoredScope map[string]bool
	if list, ok := rp.kline["scope"].([]any); ok {
		restoredScope = map[string]bool{}
		for _, n := range list {
			if s, ok := n.(string); ok {
				restoredScope[s] = true
			}
		}
	}
	_, isList := body["scope"].([]any)
	scopeGiven := isList || body["scope"] == "corpus"
	same := true
	switch {
	case scope != nil:
		same = restoredScope != nil && len(restoredScope) == len(scope)
		for n := range scope {
			same = same && restoredScope[n]
		}
	case scopeGiven:
		same = restoredScope == nil
	}
	span, _ := rp.kline["span"].(map[string]any)
	for i, end := range []string{"from", "to"} {
		if ends[i] != nil && float64(*ends[i]) != num(span[end]) {
			same = false
		}
	}
	if !same {
		return nil, refuse(http.StatusGone, "no step is retained since the restore; only the restored snapshot is capturable")
	}
	return e.storeSnapshot(rp.kline, rp.arbitration)
}

// Snapshots is GET /api/runtime/snapshots.
func (e *Engine) Snapshots() (map[string]any, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if !e.snapshots.enabled() {
		return nil, errSnapshotsDisabled
	}
	return e.snapshots.list(), nil
}

// Snapshot is GET /api/runtime/snapshot/:id.
func (e *Engine) Snapshot(id string) (map[string]any, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if !e.snapshots.enabled() {
		return nil, errSnapshotsDisabled
	}
	if s, ok := e.snapshots.get(id); ok {
		return s, nil
	}
	return nil, refuse(http.StatusNotFound, "no snapshot %s", id)
}

// DeleteSnapshot is DELETE /api/runtime/snapshot/:id.
func (e *Engine) DeleteSnapshot(id string) (map[string]any, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.snapshots.enabled() {
		return nil, errSnapshotsDisabled
	}
	if !e.snapshots.remove(id) {
		return nil, refuse(http.StatusNotFound, "no snapshot %s", id)
	}
	return map[string]any{"deleted": id}, nil
}

// RestoreSnapshot is POST /api/runtime/restore {id} | {snapshot}.
func (e *Engine) RestoreSnapshot(body map[string]any) (map[string]any, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.snapshots.enabled() {
		return nil, errSnapshotsDisabled
	}
	var snap map[string]any
	if s, ok := body["snapshot"].(map[string]any); ok {
		snap = s
	} else if id, ok := body["id"].(string); ok {
		stored, ok := e.snapshots.get(id)
		if !ok {
			return nil, refuse(http.StatusNotFound, "no snapshot %s", id)
		}
		snap = stored
	} else {
		return nil, refuse(http.StatusUnprocessableEntity, "body is {id} or {snapshot}")
	}
	if snap["format"] != "re-snapshot/1" {
		return nil, refuse(http.StatusUnprocessableEntity, "unknown format")
	}
	if d, _ := snap["digest"].(string); d != digestOf(snap) {
		return nil, refuse(http.StatusUnprocessableEntity, "the digest does not match the body")
	}
	e.refreshCorpusFacts()
	corpus, _ := snap["corpus"].(map[string]any)
	expected, _ := corpus["fingerprint"].(string)
	if expected != e.corpusFingerprint {
		return nil, &SnapshotError{http.StatusConflict, map[string]any{"error": "corpus mismatch", "expected": expected,
			"actual": e.corpusFingerprint}}
	}
	if err := e.restoreSnapshot(snap); err != nil {
		return nil, refuse(http.StatusUnprocessableEntity, "%s", err.Error())
	}
	kl, _ := snap["kline"].(map[string]any)
	var step any = e.completedStep
	if _, corpusScope := kl["scope"].(string); corpusScope {
		span, _ := kl["span"].(map[string]any)
		step = span["to"]
	}
	return map[string]any{"digest": snap["digest"], "step": step}, nil
}

// restoreSnapshot re-establishes controls, machine controls, overrides, then
// activeAfter; for a corpus scope also the history and the step count. A
// restore is a reset for K-line capture. It commits no step. The caller holds
// the write lock.
func (e *Engine) restoreSnapshot(snap map[string]any) error {
	controls, _ := snap["controls"].(map[string]any)
	for _, c := range e.controls() {
		name := c["name"].(string)
		if c["scope"] != "engine" {
			continue
		}
		v, ok := controls[name]
		if !ok || v == nil {
			continue
		}
		if _, err := e.setControl(name, map[string]any{"value": v}); err != nil {
			return err
		}
	}
	idOf := map[string]string{}
	for id, m := range e.machines {
		idOf[m.Name] = id
	}
	mc, _ := snap["machineControls"].(map[string]any)
	for name, raw := range mc {
		per, _ := raw.(map[string]any)
		if b, ok := per["transitionsInhibited"].(bool); ok {
			if id, ok := idOf[name]; ok {
				e.machines[id].TransitionsInhibited = b
			}
		}
	}
	mo, _ := snap["machineOverrides"].(map[string]any)
	for name, raw := range mo {
		o, _ := raw.(map[string]any)
		id, ok := idOf[name]
		if !ok {
			continue
		}
		if t, ok := o["outputMergeTransformation"].(string); ok {
			t = strings.ToLower(t)
			if !knownOutputMerges[t] {
				return fmt.Errorf("unknown outputMergeTransformation: %s", t)
			}
			e.machines[id].OutputMerge = t
		}
	}
	kl, _ := snap["kline"].(map[string]any)
	_, corpusScope := kl["scope"].(string)
	scope := map[string]bool{}
	if corpusScope {
		for name := range idOf {
			scope[name] = true
		}
	} else {
		list, _ := kl["scope"].([]any)
		for _, n := range list {
			if s, ok := n.(string); ok {
				scope[s] = true
			}
		}
	}
	byMachine := map[string][]activeEntry{}
	list, _ := kl["activeAfter"].([]any)
	for _, raw := range list {
		o, _ := raw.(map[string]any)
		str := func(k string) string { s, _ := o[k].(string); return s }
		jm, _ := o["justMatched"].(bool)
		a := activeEntry{str("machine"), str("sequence"), str("event"), jm}
		byMachine[a.machine] = append(byMachine[a.machine], a)
	}
	for _, m := range e.machines {
		if !scope[m.Name] {
			continue
		}
		for _, seq := range m.Sequences {
			for _, ev := range seq.Events {
				ev.Active, ev.WasJustMatched, ev.PredecessorChain = false, false, nil
			}
		}
		for _, a := range byMachine[m.Name] {
			if seq, ok := m.Sequences[a.sequence]; ok {
				if ev, ok := seq.Events[a.event]; ok {
					ev.Active, ev.WasJustMatched = true, a.justMatched
				}
			}
		}
	}
	// Any restore is a retention boundary (#904): the next step starts from the
	// live list, and until it the restored snapshot is what a capture returns.
	e.retention.restore(allActives(e.machines), kl, snap["arbitration"])
	if corpusScope {
		span, _ := kl["span"].(map[string]any)
		to := int(num(span["to"]))
		pairs, _ := kl["pairs"].([]any)
		var isre, osre []trajectoryEntry
		for _, p := range pairs {
			pm, _ := p.(map[string]any)
			isre = append(isre, entryFrom(pm["isre"]))
			osre = append(osre, entryFrom(pm["osre"]))
		}
		s := &e.steps
		s.isre, s.osre, s.history = isre, osre, nil
		s.lastArbitration = nil
		s.retained = map[int]retainedStep{}
		s.nextStep = to + 1
		e.completedStep = to
	}
	if e.klines.controls.capture {
		if err := e.klines.onReset(); err != nil {
			e.klineFailed(err)
		}
	}
	return nil
}

// knownOutputMerges is every outputMergeTransformation the fold knows.
var knownOutputMerges = map[string]bool{"or": true, "and": true, "xor": true, "nor": true, "nand": true, "meet": true,
	"join": true, "discrete-median": true, "strong-conjunction": true, "strong-disjunction": true}
