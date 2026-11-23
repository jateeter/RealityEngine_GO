package machine

import "strings"

// Reality Events and sequences built from a request body, for the engine's
// own sequence store (/api/sequences). C++'s vector_from_json and
// sequence_from_json are the concrete form: unlike a corpus definition, an
// event's metadata is not read, a missing id is minted, and a non-number in
// an output vector reads 0.

// DefaultMatch is the comparator a request-built event reports.
const DefaultMatch = "gte"

// Comparator is a comparatorType's canonical name; an unknown name is a 500
// (SURFACE_SPEC "Comparators", owner ruling 2026-11-01).
func Comparator(name string) (string, error) {
	switch c := strings.ToLower(name); c {
	case "equals", "threshold", "pattern", "custom", "gte":
		return c, nil
	}
	return "", &Error{Status: 500, Message: "Unknown comparator type: " + name}
}

// EventFromRequest builds a Reality Event from a request body; mint gives a
// fresh id with a prefix.
func EventFromRequest(body map[string]any, mint func(prefix string) string) (*Event, error) {
	var elements []Element
	for _, el := range list(body["elements"]) {
		obj, _ := el.(map[string]any)
		element := Element{Value: num(obj["value"], 0)}
		if name, ok := obj["comparatorType"].(string); ok {
			c, err := Comparator(name)
			if err != nil {
				return nil, err
			}
			element.Comparator = c
		}
		if t, ok := obj["threshold"].(float64); ok {
			element.Threshold = &t
		}
		elements = append(elements, element)
	}
	var outputs []Output
	for _, out := range list(body["outputEvents"]) {
		o, _ := out.(map[string]any)
		values := list(o["vector"])
		vec := make([]float64, len(values))
		for i, v := range values {
			vec[i] = num(v, 0)
		}
		outputs = append(outputs, Output{ID: str(o["id"], mint("output")), Vector: vec, Metadata: objectOrEmpty(o["metadata"])})
	}
	var next []string
	for _, n := range list(body["nextEventIds"]) {
		if s, ok := n.(string); ok {
			next = append(next, s)
		}
	}
	initial, _ := body["isInitial"].(bool)
	return &Event{
		ID: str(body["id"], mint("vector")), Initial: initial, Active: initial,
		Elements: elements, Outputs: outputs, NextIDs: next, Metadata: map[string]any{},
	}, nil
}

// SequenceFromRequest builds a sequence from a request body. A later event
// with an id already taken replaces the earlier one.
func SequenceFromRequest(body map[string]any, mint func(prefix string) string) (*Sequence, error) {
	events := map[string]*Event{}
	for _, ev := range list(body["events"]) {
		obj, _ := ev.(map[string]any)
		e, err := EventFromRequest(obj, mint)
		if err != nil {
			return nil, err
		}
		events[e.ID] = e
	}
	return &Sequence{
		ID: str(body["id"], mint("sequence")), Name: str(body["name"], "unnamed"),
		Metadata: objectOrEmpty(body["metadata"]), Events: events,
	}, nil
}

// ValidationErrors are why a request-built sequence cannot be stored: it
// needs an initial event and an event with an output.
func (s *Sequence) ValidationErrors() []string {
	var initial, output bool
	for _, e := range s.Events {
		initial = initial || e.Initial
		output = output || len(e.Outputs) > 0
	}
	var errs []string
	if !initial {
		errs = append(errs, "CriticalEventSequence must have at least one initial vector")
	}
	if !output {
		errs = append(errs, "CriticalEventSequence must have at least one vector with output")
	}
	return errs
}
