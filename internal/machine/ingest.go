package machine

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// VersionBase is a name with a trailing " v<n>" removed, if it has one
// (SURFACE_SPEC "POST /api/machines always ingests"): the base a conflicting
// name is numbered from, so versions continue rather than nest.
func VersionBase(name string) string {
	i := strings.LastIndex(name, " v")
	if i < 0 {
		return name
	}
	suffix := strings.TrimPrefix(name[i+2:], "+")
	if suffix == "" || strings.HasPrefix(suffix, "+") {
		return name
	}
	if _, err := strconv.ParseUint(suffix, 10, 64); err != nil {
		return name
	}
	return name[:i]
}

// VersionName gives the machine a versioned name and, when it has a
// perceptual mapping, newly allocated regions appended at the end of the
// space: the input region at inputOffset, the output region right after it
// (SURFACE_SPEC "The allocation must be deterministic"). The declared regions
// are discarded.
func (m *Machine) VersionName(name string, inputOffset int) {
	m.Name = name
	m.Raw["name"] = name
	mapping, ok := m.Raw["perceptualMapping"].(map[string]any)
	if !ok {
		return
	}
	output, _ := mapping["output"].(map[string]any)
	outputLength := integer(output["length"], 0)
	mapping["input"] = map[string]any{"offset": float64(inputOffset), "length": float64(m.InputLength)}
	mapping["output"] = map[string]any{"offset": float64(inputOffset + m.InputLength), "length": float64(outputLength)}
	m.InputOffset = inputOffset
}

// Region is a perceptual-mapping region.
type Region struct{ Offset, Length int }

// Regions are the machine's declared input and output regions, read from its
// mapping (a missing value is 0); ok is false with no perceptual mapping.
func (m *Machine) Regions() (input, output Region, ok bool) {
	mapping, ok := m.Raw["perceptualMapping"].(map[string]any)
	if !ok {
		return Region{}, Region{}, false
	}
	read := func(v any) Region {
		r, _ := v.(map[string]any)
		return Region{integer(r["offset"], 0), integer(r["length"], 0)}
	}
	return read(mapping["input"]), read(mapping["output"]), true
}

// Resolve is the machine's slice of a universal input space; cells past the
// space read 0.
func (m *Machine) Resolve(universal []float64) ([]float64, error) {
	input, _, ok := m.Regions()
	if !ok {
		return nil, badRequest("Machine has no perceptual mapping")
	}
	out := make([]float64, input.Length)
	for i := range out {
		if j := input.Offset + i; j < len(universal) {
			out[i] = universal[j]
		}
	}
	return out, nil
}

// Clone is an independent copy of the machine and its run state.
func (m *Machine) Clone() *Machine {
	c := *m
	c.Raw, _ = deepCopy(m.Raw).(map[string]any)
	c.Sequences = make(map[string]*Sequence, len(m.Sequences))
	for id, s := range m.Sequences {
		sc := *s
		sc.Events = make(map[string]*Event, len(s.Events))
		for eid, e := range s.Events {
			ec := *e
			ec.PredecessorChain = append([]string(nil), e.PredecessorChain...)
			sc.Events[eid] = &ec
		}
		c.Sequences[id] = &sc
	}
	if m.OutputAlphabetTop != nil {
		top := *m.OutputAlphabetTop
		c.OutputAlphabetTop = &top
	}
	return &c
}

// Declaration is a copy of the machine object as declared, for a route that
// rebuilds the machine from a changed declaration.
func (m *Machine) Declaration() map[string]any {
	d, _ := deepCopy(m.Raw).(map[string]any)
	return d
}

// WhatIf is what input would do, asked of an uninhibited copy: nothing
// persists, and the answer carries no machineId.
func (m *Machine) WhatIf(input []float64, clock Clock) map[string]any {
	c := m.Clone()
	c.TransitionsInhibited = false
	result := c.Respond(input, "", clock)
	delete(result, "machineId")
	return result
}

// FromObject parses a decoded machine definition (Parse on its JSON).
func FromObject(v any) (*Machine, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, badRequest("machine definition could not be encoded: %v", err)
	}
	return Parse(data)
}

// NotFound is a 404 with the message the route answers.
func NotFound(format string, args ...any) *Error {
	return &Error{Status: 404, Message: fmt.Sprintf(format, args...)}
}

// BadRequest is a 400 with the message the route answers.
func BadRequest(format string, args ...any) *Error { return badRequest(format, args...) }

func deepCopy(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			out[k] = deepCopy(x)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = deepCopy(x)
		}
		return out
	default:
		return v
	}
}
