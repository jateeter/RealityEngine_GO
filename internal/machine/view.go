package machine

import (
	"sort"
	"strings"
)

// How a machine is shown: GET /api/machines (the list form), GET
// /api/machines/:id (the full form) and GET /api/machines/:id/export (the
// corpus document).
//
//   - A machine's sequences are in (name, id) order, and sequenceIds follows
//     them.
//   - The list form summarises each sequence as {id, name, initialEventIds}
//     (SURFACE_SPEC "GET /api/machines — the sequence summary"); the full form
//     carries every Reality Event as it is running.
//   - Declared inputSequences travel in metadata on the runtime views, and move
//     back to the top level, with arbiterRule upper-cased, in the export.

// View is the event as it is running.
func (e *Event) View(matchAlgorithm string, loadedAtMS int64) map[string]any {
	elements := make([]any, len(e.Elements))
	for i, el := range e.Elements {
		v := map[string]any{"value": el.Value}
		if el.Comparator != "" {
			v["comparatorType"] = el.Comparator
		}
		if el.Threshold != nil {
			v["threshold"] = *el.Threshold
		}
		elements[i] = v
	}
	outputs := make([]any, len(e.Outputs))
	for i, o := range e.Outputs {
		outputs[i] = map[string]any{
			"id": o.ID, "vector": nonNil(o.Vector), "metadata": o.Metadata,
			"timestamp": loadedAtMS, "provenance": []string{},
		}
	}
	state := "inactive"
	if e.Active {
		state = "active"
	}
	return map[string]any{
		"id":             e.ID,
		"matchAlgorithm": matchAlgorithm,
		"elements":       elements,
		"state":          state,
		"isActive":       e.Active,
		"nextEventIds":   nonNilStrings(e.NextIDs),
		"outputEvents":   outputs,
		"isInitial":      e.Initial,
		"wasJustMatched": e.WasJustMatched,
		"metadata":       e.Metadata,
	}
}

// InitialEventIDs is the sequence's initial events, in id order.
func (s *Sequence) InitialEventIDs() []string {
	ids := []string{}
	for _, id := range s.EventIDs() {
		if s.Events[id].Initial {
			ids = append(ids, id)
		}
	}
	return ids
}

// View is the sequence in full, events in id order.
func (s *Sequence) View(matchAlgorithm string, loadedAtMS int64) map[string]any {
	events := []any{}
	outputIDs := []string{}
	for _, id := range s.EventIDs() {
		events = append(events, s.Events[id].View(matchAlgorithm, loadedAtMS))
		if len(s.Events[id].Outputs) > 0 {
			outputIDs = append(outputIDs, id)
		}
	}
	out := map[string]any{
		"id": s.ID, "name": s.Name, "events": events,
		"initialEventIds": s.InitialEventIDs(), "outputEventIds": outputIDs,
		"metadata": s.Metadata,
	}
	for key, value := range map[string]string{
		"schemaVersion": s.SchemaVersion, "deprecatedAt": s.DeprecatedAt, "replacedBy": s.ReplacedBy,
	} {
		if value != "" {
			out[key] = value
		}
	}
	return out
}

// Domain is metadata.domain, or "".
func (m *Machine) Domain() string {
	meta, _ := m.Raw["metadata"].(map[string]any)
	d, _ := meta["domain"].(string)
	return d
}

// SequencesInOrder is the machine's sequences by name, then id.
func (m *Machine) SequencesInOrder() []*Sequence {
	seqs := make([]*Sequence, 0, len(m.Sequences))
	for _, s := range m.Sequences {
		seqs = append(seqs, s)
	}
	sort.Slice(seqs, func(i, j int) bool {
		if seqs[i].Name != seqs[j].Name {
			return seqs[i].Name < seqs[j].Name
		}
		return seqs[i].ID < seqs[j].ID
	})
	return seqs
}

// metadata is the machine's metadata with its declared inputSequences stashed
// in it.
func (m *Machine) metadata() map[string]any {
	out := map[string]any{}
	if meta, ok := m.Raw["metadata"].(map[string]any); ok {
		for k, v := range meta {
			out[k] = v
		}
	}
	if seqs, ok := m.Raw["inputSequences"].([]any); ok {
		out["inputSequences"] = seqs
	}
	return out
}

// mapping is {input, output}, with bitsPerElement and outputAlphabetTop only
// when declared and valid; nil with no mapping.
func (m *Machine) mapping() any {
	pm, ok := m.Raw["perceptualMapping"].(map[string]any)
	if !ok {
		return nil
	}
	region := func(v any) map[string]any {
		r, _ := v.(map[string]any)
		return map[string]any{"offset": integer(r["offset"], 0), "length": integer(r["length"], 0)}
	}
	out := map[string]any{"input": region(pm["input"]), "output": region(pm["output"])}
	if bits, ok := pm["bitsPerElement"].(float64); ok {
		switch bits {
		case 1, 2, 4, 8:
			out["bitsPerElement"] = int(bits)
		}
	}
	if m.OutputAlphabetTop != nil && *m.OutputAlphabetTop >= 1 {
		out["outputAlphabetTop"] = *m.OutputAlphabetTop
	}
	return out
}

// View is the machine as GET /api/machines lists it (full false), or as GET
// /api/machines/:id answers (full true).
func (m *Machine) View(full bool) map[string]any {
	seqs := m.SequencesInOrder()
	ids := make([]string, len(seqs))
	views := make([]any, len(seqs))
	total := 0
	for i, s := range seqs {
		ids[i] = s.ID
		total += len(s.Events)
		if full {
			views[i] = s.View(m.MatchAlgorithm, m.LoadedAtMS)
		} else {
			views[i] = map[string]any{"id": s.ID, "name": s.Name, "initialEventIds": s.InitialEventIDs()}
		}
	}
	return map[string]any{
		"id":                        m.ID,
		"name":                      m.Name,
		"description":               m.Description,
		"matchAlgorithm":            m.MatchAlgorithm,
		"outputMergeTransformation": m.OutputMerge,
		// The merge interlock starts locked; no route unlocks it yet.
		"outputMergeLocked": true,
		"arbiterRule":       m.ArbiterRule,
		"sequenceCount":     len(seqs),
		"totalEvents":       total,
		"sequenceIds":       ids,
		"sequences":         views,
		"metadata":          m.metadata(),
		"perceptualMapping": m.mapping(),
	}
}

// CorpusDocument is the full form as the corpus declares a machine:
// arbiterRule upper-cased and inputSequences back at the top level.
func (m *Machine) CorpusDocument() map[string]any {
	doc := m.View(true)
	doc["arbiterRule"] = strings.ToUpper(m.ArbiterRule)
	meta := doc["metadata"].(map[string]any)
	if seqs, ok := meta["inputSequences"]; ok {
		delete(meta, "inputSequences")
		doc["inputSequences"] = seqs
	}
	return doc
}

// CanonicalOrder sorts machines by metadata.domain, then name, then id.
func CanonicalOrder(machines []*Machine) {
	sort.Slice(machines, func(i, j int) bool {
		a, b := machines[i], machines[j]
		if a.Domain() != b.Domain() {
			return a.Domain() < b.Domain()
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.ID < b.ID
	})
}

func nonNilStrings(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}
