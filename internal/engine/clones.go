package engine

// Cloneable machines and the clone space (RealityEngine_GO#5;
// RealityEngine_CI SURFACE_SPEC.md "Cloneable machines", #713).
//
//   - Recognition is decided at load by the specification's nine-rule table,
//     in order, the first rule failed naming the reason. Nothing in a machine
//     file marks it cloneable. The verdict is computed against the machines
//     registered when it is asked, so a later POST, PUT or DELETE reassesses
//     every machine, and a live clone keeps running.
//   - The clone space is a band of 64-cell blocks in the nest layout
//     (input, output, fanout, bus quarters of 16), allocated lowest free
//     first, each with a generation the recycle route must present.
//   - Clone and recycle check in the specification's order, with C++'s
//     messages, so engines refusing a request refuse it alike.
//
// C++ clone_space.cpp and its server routes are the concrete form, agreed
// 3-of-3 with LSP and Scala (RealityEngine_Machines clone-recycle-quorum
// .spec.ts); Rust's src/clones.rs is prior art.

import (
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/jateeter/RealityEngine_GO/internal/machine"
)

// The clone space's constants, fixed by the specification ("The clone
// space"); RealityEngine_Machines domains/clone-space.json records the same.
const (
	cloneBandOffset     = 20480
	cloneBandLength     = 65536
	cloneBlockCells     = 64
	cloneQuarter        = 16
	cloneBlocks         = cloneBandLength / cloneBlockCells // 1024
	cloneBusCells       = 4
	cloneMaxActiveFirst = 8
)

// Verdict is one machine's assessment against the nine rules.
type Verdict struct {
	Cloneable bool
	Reason    string // the first rule failed; empty when cloneable
	N         *int   // folded fanout cells + bus cells, once rule 9 is evaluated
	Folded    int    // folded fanout cells alone
}

func notCloneable(reason string) Verdict { return Verdict{Reason: reason} }

func interconnections(m *machine.Machine) []map[string]any {
	meta, _ := m.Raw["metadata"].(map[string]any)
	list, _ := meta["interconnections"].([]any)
	out := make([]map[string]any, 0, len(list))
	for _, v := range list {
		if edge, ok := v.(map[string]any); ok {
			out = append(out, edge)
		}
	}
	return out
}

func isProducer(edge map[string]any) bool {
	t, _ := edge["type"].(string)
	return t == "published-domain-bus-producer"
}

// busTargetFiles is the file names other machines' producer edges target.
func busTargetFiles(machines map[string]*machine.Machine) map[string]bool {
	out := map[string]bool{}
	for _, m := range machines {
		for _, edge := range interconnections(m) {
			if file, _ := edge["targetMachineFile"].(string); isProducer(edge) && file != "" {
				out[file] = true
			}
		}
	}
	return out
}

// regionLength is a declared region's length; ok is false when the machine
// has no perceptualMapping.
func regionLength(m *machine.Machine, which string) (int, bool) {
	mapping, ok := m.Raw["perceptualMapping"].(map[string]any)
	if !ok {
		return 0, false
	}
	fallback := 0
	if which == "input" {
		fallback = m.InputLength
	}
	region, _ := mapping[which].(map[string]any)
	if n, ok := region["length"].(float64); ok {
		return int(n), true
	}
	return fallback, true
}

// Assess applies the nine-rule table in order. busTargets is busTargetFiles
// over every registered machine: rule 3 needs the whole set.
func Assess(m *machine.Machine, busTargets map[string]bool) Verdict {
	stem := strings.TrimSuffix(m.SourceFile, ".json")
	producers, handoffs, folded := 0, 0, 0
	for _, edge := range interconnections(m) {
		switch {
		case isProducer(edge):
			producers++
			elements, _ := edge["targetInputElements"].([]any)
			folded += len(elements)
		case stem != "" && edge["type"] == "document-workflow-handoff" && edge["sourceMachine"] == stem:
			handoffs++
		}
	}
	meta, _ := m.Raw["metadata"].(map[string]any)
	// 1. It dispatches an agentic action.
	if binding, present := meta["agentBinding"]; (!present || binding == nil) && handoffs == 0 {
		return notCloneable("no-agentic-action")
	}
	// 2. It has fanout.
	if producers == 0 && handoffs == 0 {
		return notCloneable("no-fanout")
	}
	// 3. It is not a bus target.
	if m.SourceFile != "" && busTargets[m.SourceFile] {
		return notCloneable("bus-target")
	}
	// 4. Not life-safety.
	if meta["severity"] == "life-safety" {
		return notCloneable("life-safety")
	}
	// 5. Not a fixture: string tags only.
	tags, _ := meta["tags"].([]any)
	for _, t := range tags {
		if tag, ok := t.(string); ok && strings.Contains(tag, "fixture") {
			return notCloneable("fixture")
		}
	}
	// 6. Not opted out.
	if meta["cloneable"] == false {
		return notCloneable("opted-out")
	}
	// 7, 8. Its own regions fit their quarters; no perceptualMapping fails 7.
	if length, ok := regionLength(m, "input"); !ok || length > cloneQuarter {
		return notCloneable("input-too-long")
	}
	if length, _ := regionLength(m, "output"); length > cloneQuarter {
		return notCloneable("output-too-long")
	}
	// 9. The cells its fanout folds into fit the fanout quarter.
	n := folded + cloneBusCells
	v := Verdict{Cloneable: folded <= cloneQuarter, N: &n, Folded: folded}
	if !v.Cloneable {
		v.Reason = "fanout-too-wide"
	}
	return v
}

// Cloneable is GET /api/machines/cloneable: every registered machine except
// clones, sorted by id, with its verdict.
func (e *Engine) Cloneable() map[string]any {
	e.mu.RLock()
	defer e.mu.RUnlock()
	targets := busTargetFiles(e.machines)
	ids := make([]string, 0, len(e.machines))
	for id, m := range e.machines {
		if m.CloneOf == "" {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	rows := make([]any, len(ids))
	for i, id := range ids {
		v := Assess(e.machines[id], targets)
		var reason, n any
		if !v.Cloneable {
			reason = v.Reason
		}
		if v.N != nil {
			n = *v.N
		}
		rows[i] = map[string]any{"id": id, "cloneable": v.Cloneable, "reason": reason, "n": n}
	}
	return map[string]any{"machines": rows}
}

// cloneRecord is one live clone's tenancy of one block.
type cloneRecord struct {
	cloneID, machineID, engineInstance, envelopeID, assetID string
	block                                                   int
	generation                                              int
	inputLength, outputLength, fanoutLength                 int
}

func (c *cloneRecord) offset() int { return cloneBandOffset + cloneBlockCells*c.block }

func region(offset, length int) map[string]any {
	return map[string]any{"offset": offset, "length": length}
}

func (c *cloneRecord) blockJSON() map[string]any {
	return map[string]any{"index": c.block, "offset": c.offset(), "length": cloneBlockCells}
}

// json is the 201 body, and each entry of GET /api/engine/clones.
func (c *cloneRecord) json() map[string]any {
	o := c.offset()
	return map[string]any{
		"cloneId": c.cloneID, "machineId": c.machineID, "engineInstance": c.engineInstance,
		"envelopeId": c.envelopeID, "assetId": c.assetID, "block": c.blockJSON(),
		"input":      region(o, c.inputLength),
		"output":     region(o+cloneQuarter, c.outputLength),
		"fanout":     region(o+2*cloneQuarter, c.fanoutLength),
		"bus":        region(o+3*cloneQuarter, cloneBusCells),
		"generation": c.generation,
	}
}

// cloneSpace is the live clones and every block's generation. A restart loses
// them all, and every generation starts again at 0.
type cloneSpace struct {
	generations [cloneBlocks]int
	byBlock     map[int]*cloneRecord
	maxActive   int
	instance    string // INSTANCE_ID: the middle segment of every clone id
}

func newCloneSpace() cloneSpace {
	return cloneSpace{byBlock: map[int]*cloneRecord{}, maxActive: cloneMaxActiveFirst, instance: os.Getenv("INSTANCE_ID")}
}

func (s *cloneSpace) lowestFree() (int, bool) {
	for k := 0; k < cloneBlocks; k++ {
		if _, held := s.byBlock[k]; !held {
			return k, true
		}
	}
	return 0, false
}

func (s *cloneSpace) liveOf(machineID string) int {
	n := 0
	for _, c := range s.byBlock {
		if c.machineID == machineID {
			n++
		}
	}
	return n
}

func (s *cloneSpace) find(match func(*cloneRecord) bool) *cloneRecord {
	for _, c := range s.byBlock {
		if match(c) {
			return c
		}
	}
	return nil
}

// live is every live clone in block order.
func (s *cloneSpace) live() []any {
	blocks := make([]int, 0, len(s.byBlock))
	for k := range s.byBlock {
		blocks = append(blocks, k)
	}
	sort.Ints(blocks)
	out := make([]any, len(blocks))
	for i, k := range blocks {
		out[i] = s.byBlock[k].json()
	}
	return out
}

// refusal is a refused request with fields beside its message.
func refusal(status int, message string, extra map[string]any) (int, map[string]any) {
	out := map[string]any{"error": message}
	for k, v := range extra {
		out[k] = v
	}
	return status, out
}

// CloneMachine is POST /api/engine/clone/machine/:machineId, asset and
// envelope already percent-decoded. The checks run in the specification's
// order and the first that fails decides the answer.
func (e *Engine) CloneMachine(machineID, asset, envelope string) (int, map[string]any) {
	if asset == "" || envelope == "" {
		return refusal(http.StatusBadRequest, "asset and envelope are required", nil)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	template, ok := e.machines[machineID]
	if !ok {
		return refusal(http.StatusNotFound, "Machine not found", nil)
	}
	if template.CloneOf != "" {
		return refusal(http.StatusBadRequest, "a clone is not cloneable", map[string]any{"reason": "clone"})
	}
	verdict := Assess(template, busTargetFiles(e.machines))
	if !verdict.Cloneable {
		return refusal(http.StatusBadRequest, "machine is not cloneable", map[string]any{"reason": verdict.Reason})
	}
	s := &e.clones
	if s.instance == "" {
		return refusal(http.StatusConflict, "engine launched without an INSTANCE_ID", nil)
	}
	if held := s.find(func(c *cloneRecord) bool { return c.envelopeID == envelope }); held != nil {
		if held.machineID == machineID {
			return http.StatusOK, held.json() // a retried dispatch
		}
		return refusal(http.StatusConflict, fmt.Sprintf("envelope %s holds a clone of another machine", envelope), nil)
	}
	if s.liveOf(machineID) >= s.maxActive {
		return refusal(http.StatusConflict, "template is at clone.maxActive", nil)
	}
	block, free := s.lowestFree()
	if !free {
		return refusal(http.StatusConflict, "the clone band has no free block", nil)
	}
	input, _ := regionLength(template, "input")
	output, _ := regionLength(template, "output")
	record := &cloneRecord{
		cloneID: machineID + "." + s.instance + "." + envelope, machineID: machineID, engineInstance: s.instance,
		envelopeID: envelope, assetID: asset, block: block, generation: s.generations[block],
		inputLength: input, outputLength: output, fanoutLength: verdict.Folded,
	}
	// The template as declared, at its initial state: only the id and the two
	// regions differ. It starts uninhibited, whatever the template's control
	// says, and has no corpus file.
	c := template.Clone()
	c.Reset()
	c.ID = record.cloneID
	c.SourceFile = ""
	c.TransitionsInhibited = false
	c.CloneOf, c.AssetID, c.EnvelopeID = machineID, asset, envelope
	o := record.offset()
	c.InputOffset = o
	mapping, _ := c.Raw["perceptualMapping"].(map[string]any)
	mapping["input"] = map[string]any{"offset": float64(o), "length": float64(input)}
	mapping["output"] = map[string]any{"offset": float64(o + cloneQuarter), "length": float64(output)}
	// Not ingest: that versions a resident name, and a clone keeps its
	// template's name unchanged.
	e.admit(c)
	s.byBlock[block] = record
	return http.StatusCreated, record.json()
}

// parseWhole is a whole number, optionally negative, and nothing else (C++
// parse_whole).
func parseWhole(text string) (int, bool) {
	digits := strings.TrimPrefix(text, "-")
	if text == "" || len(text) > 18 || digits == "" {
		return 0, false
	}
	for _, r := range digits {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(text)
	return n, err == nil
}

// RecycleClone is POST /api/engine/recycle/machine/:cloneId?generation=:
// 400, then 404, then 409. present is false when generation was not given.
func (e *Engine) RecycleClone(cloneID, generation string, present bool) (int, map[string]any) {
	g, ok := parseWhole(generation)
	if !present || !ok {
		return refusal(http.StatusBadRequest, "generation is required, as an integer", nil)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	s := &e.clones
	live := s.find(func(c *cloneRecord) bool { return c.cloneID == cloneID })
	if live == nil {
		return refusal(http.StatusNotFound, "No live clone "+cloneID, nil)
	}
	if live.generation != g {
		return refusal(http.StatusConflict, "generation is not the clone's", map[string]any{"generation": live.generation})
	}
	e.machineRemoved(cloneID)
	delete(e.machines, cloneID)
	delete(e.checkpoints, cloneID)
	e.mappingVersion++
	delete(s.byBlock, live.block)
	s.generations[live.block]++
	return http.StatusOK, map[string]any{"cloneId": cloneID, "block": live.blockJSON(), "generation": s.generations[live.block]}
}

// Clones is GET /api/engine/clones.
func (e *Engine) Clones() map[string]any {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return map[string]any{"clones": e.clones.live()}
}

// refuseClone is the 409 a PUT, PATCH or DELETE naming a clone answers: a
// clone changes only through recycle. The caller holds the lock.
func (e *Engine) refuseClone(id string) error {
	if m, ok := e.machines[id]; ok && m.CloneOf != "" {
		return &machine.Error{Status: http.StatusConflict, Message: "a clone changes only through POST /api/engine/recycle/machine/:cloneId"}
	}
	return nil
}
