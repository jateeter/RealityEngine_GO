// Package machine is the Reality Engine's dFSA machine: the canonical machine
// definition, its Critical Event Sequences (CESs) and Reality Events, one
// transition, and the per-machine output fold (RealityEngine_GO#10).
//
// The contract is RealityEngine_CI's SURFACE_SPEC.md and what C++, LSP and
// Scala agree on. RealityEngine_Rust is prior art for how those rules read in
// code, never the reference. Canonical orderings never depend on map
// iteration: sequences and events are visited in id order.
package machine

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"unicode"
)

// Error is a rejected machine definition or request: the message names what
// and where. Status is the HTTP status a route answers with.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return e.Message }

func badRequest(format string, args ...any) *Error {
	return &Error{Status: 400, Message: fmt.Sprintf(format, args...)}
}

// Element is one cell of a Reality Event as a CES declares it.
type Element struct {
	Value      float64
	Comparator string   // lower-cased comparatorType; empty when not declared
	Threshold  *float64 // nil when not declared
}

// Output is an output event an event asserts when it matches.
type Output struct {
	ID       string
	Vector   []float64
	Metadata any
}

// Event is one Reality Event of a sequence, with its run state.
type Event struct {
	ID             string
	Initial        bool
	Active         bool
	Elements       []Element
	Outputs        []Output
	NextIDs        []string
	Metadata       any
	WasJustMatched bool
	// The ids of the events that led here, in order: the provenance of what
	// this event asserts.
	PredecessorChain []string
}

// Sequence is one Critical Event Sequence.
type Sequence struct {
	ID            string
	Name          string
	Metadata      any
	SchemaVersion string
	DeprecatedAt  string
	ReplacedBy    string
	Events        map[string]*Event
}

// EventIDs is the sequence's events in id order.
func (s *Sequence) EventIDs() []string { return sortedKeys(s.Events) }

// Machine is a loaded machine definition and its run state.
type Machine struct {
	ID                   string
	Name                 string
	Description          string
	InputOffset          int
	InputLength          int
	MatchAlgorithm       string // lower-cased; default gte
	ArbiterRule          string // lower-cased; default passthrough
	OutputMerge          string // lower-cased outputMergeTransformation; default or
	OutputAlphabetTop    *int
	Raw                  map[string]any // the machine object as declared
	Sequences            map[string]*Sequence
	TransitionsInhibited bool
	// SourceFile is the corpus file name it was loaded from; empty for a
	// machine registered from a request body. Not serialised.
	SourceFile string
	// CloneOf, AssetID and EnvelopeID are set on a clone only: its
	// template's id, and the assetID and dispatch envelope it serves
	// (SURFACE_SPEC "Cloneable machines").
	CloneOf, AssetID, EnvelopeID string
	// LoadedAtMS is when the machine was loaded: a declared output's
	// timestamp in the full view, as C++ stamps it.
	LoadedAtMS int64
}

// SequenceIDs is the machine's sequences in id order.
func (m *Machine) SequenceIDs() []string { return sortedKeys(m.Sequences) }

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// CanonicalID is the id a corpus file's machine gets on every engine:
// "machine-" and the file stem lower-cased, each character that is not an
// ASCII letter or digit replaced by "-".
func CanonicalID(stem string) string {
	var b strings.Builder
	b.WriteString("machine-")
	for _, r := range stem {
		if r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)) {
			b.WriteRune(unicode.ToLower(r))
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

// Parse reads a machine definition: either the machine object itself or a
// document with it under "machine". Retired Reality Event keys are refused,
// as SURFACE_SPEC "Reality Event key names" requires: a legacy file must not
// load as valid but empty.
func Parse(data []byte) (*Machine, error) {
	var doc any
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, badRequest("machine definition is not valid JSON: %v", err)
	}
	root, ok := doc.(map[string]any)
	if !ok {
		return nil, badRequest("machine definition must be a JSON object")
	}
	if version, ok := root["version"].(string); ok {
		if major, _, _ := strings.Cut(version, "."); major != "1" {
			return nil, badRequest("unsupported machine version: %s", version)
		}
	}
	machineValue, ok := root["machine"]
	if !ok {
		machineValue = doc
	}
	obj, ok := machineValue.(map[string]any)
	if !ok {
		return nil, badRequest("machine must be a JSON object")
	}
	name := str(obj["name"], "")
	if strings.TrimSpace(name) == "" {
		return nil, badRequest("machine.name is required and must not be empty")
	}
	// A declared id is the machine object's own: an envelope's id is not one
	// (C++ and LSP read machine.id only; the corpus declares ids there).
	id := str(obj["id"], "")
	mapping, _ := obj["perceptualMapping"].(map[string]any)
	inputRegion, _ := mapping["input"].(map[string]any)

	for i, s := range list(obj["inputSequences"]) {
		if err := rejectRetired(s, "vectors", "events", fmt.Sprintf("inputSequences[%d]", i)); err != nil {
			return nil, err
		}
	}

	sequences := map[string]*Sequence{}
	firstEventWidth := 0
	for _, sv := range list(obj["sequences"]) {
		seq, _ := sv.(map[string]any)
		seqID := str(seq["id"], "")
		if seqID == "" {
			return nil, badRequest("every sequence must have a non-empty id")
		}
		at := "sequence " + seqID
		if err := rejectRetired(sv, "vectors", "events", at); err != nil {
			return nil, err
		}
		eventsJSON := list(seq["events"])
		if len(eventsJSON) == 0 {
			return nil, badRequest("%s has no events", at)
		}
		events := map[string]*Event{}
		for _, ev := range eventsJSON {
			e, _ := ev.(map[string]any)
			eventID := str(e["id"], "")
			if eventID == "" {
				return nil, badRequest("every event in %s must have a non-empty id", at)
			}
			eventAt := fmt.Sprintf("event %s in %s", eventID, at)
			if err := rejectRetired(ev, "outputVectors", "outputEvents", eventAt); err != nil {
				return nil, err
			}
			if err := rejectRetired(ev, "nextVectorIds", "nextEventIds", eventAt); err != nil {
				return nil, err
			}
			var elements []Element
			for _, el := range list(e["elements"]) {
				elObj, _ := el.(map[string]any)
				element := Element{
					Value:      num(elObj["value"], 0),
					Comparator: strings.ToLower(str(elObj["comparatorType"], "")),
				}
				if t, ok := elObj["threshold"].(float64); ok {
					element.Threshold = &t
				}
				elements = append(elements, element)
			}
			if firstEventWidth == 0 && len(elements) > 0 {
				firstEventWidth = len(elements)
			}
			var outputs []Output
			for i, out := range list(e["outputEvents"]) {
				o, _ := out.(map[string]any)
				metadata, ok := o["metadata"]
				if !ok {
					metadata = map[string]any{}
				}
				outputs = append(outputs, Output{
					ID:       str(o["id"], fmt.Sprintf("%s-output-%d", eventID, i)),
					Vector:   vector(o["vector"]),
					Metadata: metadata,
				})
			}
			var next []string
			for _, n := range list(e["nextEventIds"]) {
				if s, ok := n.(string); ok {
					next = append(next, s)
				}
			}
			initial, _ := e["isInitial"].(bool)
			if _, dup := events[eventID]; dup {
				return nil, badRequest("duplicate event id %s in %s", eventID, at)
			}
			events[eventID] = &Event{
				ID: eventID, Initial: initial, Active: initial,
				Elements: elements, Outputs: outputs, NextIDs: next,
				Metadata: objectOrEmpty(e["metadata"]),
			}
		}
		if _, dup := sequences[seqID]; dup {
			return nil, badRequest("duplicate sequence id %s", seqID)
		}
		sequences[seqID] = &Sequence{
			ID: seqID, Name: str(seq["name"], "unnamed"), Metadata: objectOrEmpty(seq["metadata"]),
			SchemaVersion: str(seq["schemaVersion"], ""), DeprecatedAt: str(seq["deprecatedAt"], ""),
			ReplacedBy: str(seq["replacedBy"], ""), Events: events,
		}
	}
	m := &Machine{
		ID: id, Name: name, Description: str(obj["description"], ""),
		InputOffset:    integer(inputRegion["offset"], 0),
		InputLength:    integer(inputRegion["length"], firstEventWidth),
		MatchAlgorithm: strings.ToLower(str(obj["matchAlgorithm"], "gte")),
		ArbiterRule:    strings.ToLower(str(obj["arbiterRule"], "passthrough")),
		OutputMerge:    strings.ToLower(str(obj["outputMergeTransformation"], "or")),
		Raw:            obj,
		Sequences:      sequences,
	}
	if top, ok := mapping["outputAlphabetTop"].(float64); ok && top >= 0 && top == math.Trunc(top) {
		t := int(top)
		m.OutputAlphabetTop = &t
	}
	return m, nil
}

func rejectRetired(v any, retired, canonical, at string) error {
	if obj, ok := v.(map[string]any); ok {
		if _, has := obj[retired]; has {
			return badRequest("%s uses the retired key `%s`; the canonical key is `%s`", at, retired, canonical)
		}
	}
	return nil
}

func str(v any, def string) string {
	if s, ok := v.(string); ok {
		return s
	}
	return def
}

func num(v any, def float64) float64 {
	if f, ok := v.(float64); ok {
		return f
	}
	return def
}

func integer(v any, def int) int {
	if f, ok := v.(float64); ok && f >= 0 && f == math.Trunc(f) && f <= math.MaxInt32 {
		return int(f)
	}
	return def
}

func list(v any) []any {
	l, _ := v.([]any)
	return l
}

func vector(v any) []float64 {
	out := []float64{}
	for _, x := range list(v) {
		if f, ok := x.(float64); ok {
			out = append(out, f)
		}
	}
	return out
}

func objectOrEmpty(v any) any {
	if obj, ok := v.(map[string]any); ok {
		return obj
	}
	return map[string]any{}
}

// Summary is the machine as GET /api/machines lists it.
func (m *Machine) Summary() map[string]any {
	total := 0
	for _, s := range m.Sequences {
		total += len(s.Events)
	}
	mapping, ok := m.Raw["perceptualMapping"]
	if !ok {
		mapping = nil
	}
	return map[string]any{
		"id":                m.ID,
		"name":              m.Name,
		"description":       m.Description,
		"sequenceCount":     len(m.Sequences),
		"totalEvents":       total,
		"sequenceIds":       m.SequenceIDs(),
		"perceptualMapping": mapping,
	}
}

// Reset returns every Reality Event to its loaded state, wasJustMatched
// included (SURFACE_SPEC "RE reset returns every Reality Event to its loaded
// state", owner 2026-10-01).
func (m *Machine) Reset() {
	for _, s := range m.Sequences {
		for _, e := range s.Events {
			e.Active = e.Initial
			e.WasJustMatched = false
			e.PredecessorChain = nil
		}
	}
}

// Matches reports whether a Reality Event matches this event (SURFACE_SPEC
// "Comparators", the owner's ruling on RealityEngine_CI#606): the element's
// comparatorType, else the machine's matchAlgorithm, else gte; the threshold t
// is the element's, else the comparator's default. gte binarises both sides at
// t, so a 0 means "must be off". An explicit override applies above all three.
func (e *Event) Matches(input []float64, machineAlgorithm, override string) bool {
	if len(input) != len(e.Elements) {
		return false
	}
	for i, el := range e.Elements {
		comparator := override
		if comparator == "" {
			comparator = el.Comparator
			if comparator == "" {
				comparator = machineAlgorithm
			}
		}
		actual, value := input[i], el.Value
		threshold := func(def float64) float64 {
			if el.Threshold != nil {
				return *el.Threshold
			}
			return def
		}
		var ok bool
		switch comparator {
		case "equals", "custom":
			ok = actual == value
		case "threshold":
			ok = math.Abs(value-actual) <= threshold(0.1)
		case "pattern":
			ok = 1-math.Abs(value-actual) >= threshold(0.5)
		default:
			t := threshold(0.5)
			ok = (actual >= t) == (value >= t)
		}
		if !ok {
			return false
		}
	}
	return true
}
