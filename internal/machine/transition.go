package machine

import (
	"math"
	"sort"
)

// Clock supplies what a transition stamps: the time in ms, and a fresh id for
// the arbiter's pick (minted per transition, as every engine mints it).
type Clock struct {
	NowMS  func() int64
	MintID func() string
}

type assertion struct {
	output     Output
	provenance []string
}

// Respond is the machine's answer to one Reality Event. Inhibited
// (transitionsInhibited), the event is accepted and not passed forward: the
// shape of a machine that matched nothing, no state change, the arbiter
// metadata zeroed (SURFACE_SPEC "transitionsInhibited").
func (m *Machine) Respond(input []float64, override string, clock Clock) map[string]any {
	if !m.TransitionsInhibited {
		return m.Transition(input, override, clock)
	}
	empty := map[string]any{}
	for _, id := range m.SequenceIDs() {
		empty[id] = map[string]any{"matchedEvents": []string{}, "activatedEvents": []string{}, "assertedOutputs": []any{}}
	}
	return map[string]any{
		"machineId":       m.ID,
		"inputEvent":      input,
		"timestamp":       clock.NowMS(),
		"sequenceResults": empty,
		"machineOutput":   nil,
		"mergedOutput":    nil,
		"arbiterMetadata": map[string]any{
			"rule": m.ArbiterRule, "totalInputs": 0, "sequencesWithOutput": 0, "shouldOutput": false,
		},
	}
}

// Transition presents one Reality Event to every sequence, in sequence-id
// order, and answers with each sequence's result, the arbiter's pick and the
// per-machine output fold.
//
// Within a sequence:
//   - every event that was active when the event arrived is tried, in id order;
//     wasJustMatched is cleared first;
//   - an active event that does not match is deactivated, unless it is initial;
//   - a matched event asserts its outputs, with its provenance (its
//     predecessor chain and itself);
//   - a matched, non-initial event with no outputs and a successor hands off:
//     it is deactivated;
//   - successors are activated after every active event has been tried
//     (deferred activation), so an event activated this step is not tried
//     until the next; the first matched predecessor's provenance is kept.
func (m *Machine) Transition(input []float64, override string, clock Clock) map[string]any {
	now := clock.NowMS()
	results := map[string]any{}
	var all []assertion
	sequencesWithOutput := 0

	for _, seqID := range m.SequenceIDs() {
		seq := m.Sequences[seqID]
		var active []string
		for _, id := range seq.EventIDs() {
			seq.Events[id].WasJustMatched = false
			if seq.Events[id].Active {
				active = append(active, id)
			}
		}
		matched, activated, emitted := []string{}, []string{}, []any{}
		pending := map[string][]string{}

		for _, id := range active {
			ev := seq.Events[id]
			if !ev.Matches(input, m.MatchAlgorithm, override) {
				if !ev.Initial {
					ev.Active = false
					ev.PredecessorChain = nil
				}
				continue
			}
			matched = append(matched, id)
			provenance := append(append([]string{}, ev.PredecessorChain...), ev.ID)
			if len(ev.Outputs) > 0 {
				ev.WasJustMatched = true
			}
			for _, out := range ev.Outputs {
				emitted = append(emitted, map[string]any{
					"id": out.ID, "vector": nonNil(out.Vector), "metadata": out.Metadata,
					"timestamp": now, "provenance": provenance,
				})
				all = append(all, assertion{out, provenance})
			}
			if !ev.Initial && len(ev.Outputs) == 0 && len(ev.NextIDs) > 0 {
				ev.Active = false
				ev.PredecessorChain = nil
			}
			for _, next := range ev.NextIDs {
				if _, seen := pending[next]; !seen {
					pending[next] = provenance
				}
			}
		}
		for _, next := range sortedKeys(pending) {
			if ev, ok := seq.Events[next]; ok && !ev.Active {
				ev.Active = true
				ev.PredecessorChain = pending[next]
				activated = append(activated, next)
			}
		}
		if len(emitted) > 0 {
			sequencesWithOutput++
		}
		results[seqID] = map[string]any{
			"matchedEvents": matched, "activatedEvents": activated, "assertedOutputs": emitted,
		}
	}

	var shouldOutput bool
	switch m.ArbiterRule {
	case "and":
		shouldOutput = len(m.Sequences) > 0 && sequencesWithOutput == len(m.Sequences)
	case "or":
		shouldOutput = sequencesWithOutput > 0
	default:
		shouldOutput = len(all) > 0
	}
	var picked, merged any
	if shouldOutput && len(all) > 0 {
		sources := make([]string, len(all))
		for i, a := range all {
			sources[i] = a.output.ID
		}
		picked = map[string]any{
			"id":         "machine-output-" + clock.MintID(),
			"vector":     nonNil(all[0].output.Vector),
			"metadata":   map[string]any{"arbiter": true, "combinedFrom": len(all), "sources": sources},
			"timestamp":  now,
			"provenance": all[0].provenance,
		}
	}
	if shouldOutput {
		vectors := make([][]float64, len(all))
		for i, a := range all {
			vectors[i] = a.output.Vector
		}
		if folded, ok := Fold(vectors, m.OutputMerge, m.OutputAlphabetTop); ok {
			merged = folded
		}
	}
	return map[string]any{
		"machineId":       m.ID,
		"inputEvent":      nonNil(input),
		"timestamp":       now,
		"sequenceResults": results,
		"machineOutput":   picked,
		"mergedOutput":    merged,
		"arbiterMetadata": map[string]any{
			"rule": m.ArbiterRule, "totalInputs": len(m.Sequences),
			"sequencesWithOutput": sequencesWithOutput, "shouldOutput": shouldOutput,
		},
	}
}

func nonNil(v []float64) []float64 {
	if v == nil {
		return []float64{}
	}
	return v
}

// chainMember reads a cell as a member of the output chain: the chain bottom
// (0) for anything not positive, NaN included; else the nearest integer,
// halves away from zero, saturated at 2^40. Rounded, not truncated, so an
// arbiter-scaled 2.9999999996 folds as 3 on every runtime.
func chainMember(cell float64) float64 {
	const ceiling = float64(uint64(1) << 40)
	if cell > 0 {
		return math.Min(math.Round(cell), ceiling)
	}
	return 0
}

// Fold is the per-machine output fold (outputMergeTransformation) over the
// outputs a transition asserted, cell by cell; a shorter vector reads 0 past
// its end. It is false when there is nothing to fold, or a strong fold has no
// declared chain top (outputAlphabetTop).
func Fold(vectors [][]float64, transformation string, chainTop *int) ([]float64, bool) {
	if len(vectors) == 0 {
		return nil, false
	}
	if (transformation == "strong-conjunction" || transformation == "strong-disjunction") && chainTop == nil {
		return nil, false
	}
	width := 0
	for _, v := range vectors {
		width = max(width, len(v))
	}
	top := 0.0
	if chainTop != nil {
		top = float64(*chainTop)
	}
	result := make([]float64, 0, width)
	for i := 0; i < width; i++ {
		asserted := 0
		cells := make([]float64, len(vectors))
		for j, v := range vectors {
			raw := 0.0
			if i < len(v) {
				raw = v[i]
			}
			if raw != 0 {
				asserted++
			}
			cells[j] = chainMember(raw)
			if transformation == "join" && chainTop != nil {
				cells[j] = math.Min(cells[j], top)
			}
		}
		n := len(cells)
		var value float64
		switch transformation {
		case "and":
			value = boolf(asserted == n)
		case "xor":
			value = boolf(asserted%2 == 1)
		case "nor":
			value = boolf(asserted == 0)
		case "nand":
			value = boolf(asserted < n)
		case "meet":
			value = math.Inf(1)
			for _, c := range cells {
				value = math.Min(value, c)
			}
		case "join":
			value = math.Inf(-1)
			for _, c := range cells {
				value = math.Max(value, c)
			}
		case "discrete-median":
			sorted := append([]float64(nil), cells...)
			sort.Float64s(sorted)
			value = sorted[(len(sorted)-1)/2]
		case "strong-disjunction":
			sum := 0.0
			for _, c := range cells {
				sum += c
			}
			value = math.Min(sum, top)
		case "strong-conjunction":
			sum := 0.0
			for _, c := range cells {
				sum += c
			}
			value = math.Max(sum-top*float64(n-1), 0)
		default: // "or"
			value = boolf(asserted > 0)
		}
		result = append(result, value)
	}
	return result, true
}

func boolf(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
