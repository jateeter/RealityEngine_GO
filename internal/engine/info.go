package engine

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/jateeter/RealityEngine_GO/internal/machine"
)

// The RE's configuration, vector store, machine graph, governance route and
// demos (RealityEngine_GO#2, slice A). SURFACE_SPEC.md governs; C++ is the
// concrete form, and Rust's lib.rs, machine_view.rs, sequences.rs and
// auxiliary.rs prior art.

const (
	defaultThreshold  = 0.5
	defaultQdrantURL  = "http://localhost:4333"
	defaultCollection = "reality-events"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// Config is GET /api/config: exactly four keys (SURFACE_SPEC "GET
// /api/config"). eventDimension is the width the engine holds now, never the
// launch seed (#422).
func (e *Engine) Config() map[string]any {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return map[string]any{
		"eventDimension": e.dimension, "matchThreshold": e.threshold,
		"qdrantUrl": envOr("QDRANT_URL", defaultQdrantURL), "collectionName": envOr("COLLECTION_NAME", defaultCollection),
	}
}

// SetDimension is PUT /api/config/dimension?dimension=N: read-only downward.
// Below the corpus requirement or the width already held is refused, naming
// which; nothing changes on a refusal, and the answer is the width now held.
func (e *Engine) SetDimension(raw *string) (map[string]any, error) {
	if raw == nil || *raw == "" {
		return nil, machine.BadRequest("dimension query parameter is required")
	}
	requested, ok := wholeNumber(*raw)
	if !ok {
		return nil, machine.BadRequest("dimension must be an integer")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	required, current := e.requiredDimension(), e.dimension
	if requested < max(required, current) {
		why := "the " + strconv.Itoa(current) + " this engine already holds"
		if requested < required {
			why = "the " + strconv.Itoa(required) + " the resident corpus requires"
		}
		return nil, machine.BadRequest("dimension %d is below %s; the perceptual space is read-only downward", requested, why)
	}
	e.dimension = requested
	return map[string]any{"success": true, "dimension": e.dimension, "requiredDimension": required}, nil
}

// SetThreshold is PUT /api/config/threshold?threshold=x (owner ruling
// RealityEngine_CI#674): a finite number is stored and reported by GET
// /api/config; absent or empty changes nothing; anything else is a 400. No
// comparator reads it (#675).
func (e *Engine) SetThreshold(raw *string) (map[string]any, error) {
	var requested *float64
	if raw != nil && *raw != "" {
		v, err := strconv.ParseFloat(*raw, 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || strings.ContainsAny(*raw, "xX_pP") {
			return nil, machine.BadRequest("threshold must be a number")
		}
		requested = &v
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if requested != nil {
		e.threshold = *requested
	}
	return map[string]any{"success": true, "threshold": e.threshold}, nil
}

// decodeDocument decodes a JSON object. Every field is kept; numbers come
// back in the ECMAScript form the voters write (1.0 is 1).
func decodeDocument(data []byte) (map[string]any, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return map[string]any{}, nil
	}
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, machine.BadRequest("invalid JSON body: %v", err)
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, machine.BadRequest("a vector must be a JSON object")
	}
	return obj, nil
}

// StoreVector is POST /api/vectors: the body stored verbatim under its id (one
// is minted when absent) and handed back (SURFACE_SPEC "POST /api/vectors").
func (e *Engine) StoreVector(body []byte) (map[string]any, error) {
	doc, err := decodeDocument(body)
	if err != nil {
		return nil, err
	}
	id, ok := doc["id"].(string)
	if !ok {
		id = mint("vector")
	}
	doc["id"] = id
	e.mu.Lock()
	defer e.mu.Unlock()
	e.vectors[id] = doc
	return map[string]any{"success": true, "vector": doc}, nil
}

func number(v any) float64 {
	switch n := v.(type) {
	case json.Number:
		f, _ := n.Float64()
		return f
	case float64:
		return n
	}
	return 0
}

// searchable is what a stored document is scored by: its vector, else its
// elements' values (an element may be a bare number).
func searchable(doc map[string]any) []float64 {
	var out []float64
	if l, ok := doc["vector"].([]any); ok {
		for _, x := range l {
			out = append(out, number(x))
		}
		return out
	}
	l, _ := doc["elements"].([]any)
	for _, x := range l {
		if obj, ok := x.(map[string]any); ok {
			out = append(out, number(obj["value"]))
		} else {
			out = append(out, number(x))
		}
	}
	return out
}

func cosine(a, b []float64) float64 {
	var dot, na, nb float64
	for i := 0; i < min(len(a), len(b)); i++ {
		dot += a[i] * b[i]
	}
	for _, v := range a {
		na += v * v
	}
	for _, v := range b {
		nb += v * v
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// SearchVectors is POST /api/vectors/search: cosine against the stored
// documents, first-k above threshold in id order, not top-k by score.
func (e *Engine) SearchVectors(body []byte) (map[string]any, error) {
	req, err := decodeDocument(body)
	if err != nil {
		return nil, err
	}
	query := searchable(map[string]any{"vector": req["vector"]})
	limit := 10
	if _, ok := req["limit"]; ok {
		limit = int(number(req["limit"]))
	}
	_, hasThreshold := req["threshold"]
	threshold := number(req["threshold"])
	e.mu.RLock()
	defer e.mu.RUnlock()
	ids := make([]string, 0, len(e.vectors))
	for id := range e.vectors {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	results := []any{}
	for _, id := range ids {
		if len(results) >= limit {
			break
		}
		score := cosine(query, searchable(e.vectors[id]))
		if hasThreshold && score < threshold {
			continue
		}
		results = append(results, map[string]any{"vector": e.vectors[id], "score": score})
	}
	return map[string]any{"results": results}, nil
}

// Vector is GET /api/vectors/:id: engine-scoped and internal.
func (e *Engine) Vector(id string) (map[string]any, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	doc, ok := e.vectors[id]
	if !ok {
		return nil, machine.NotFound("Vector not found")
	}
	return map[string]any{"vector": doc}, nil
}

// DeleteVector is DELETE /api/vectors/:id.
func (e *Engine) DeleteVector(id string) map[string]any {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.vectors, id)
	return map[string]any{"success": true, "id": id}
}

// MachineGraph is GET /api/machine-graph: every mapped machine in canonical
// order, and an edge wherever one's output region overlaps another's input.
func (e *Engine) MachineGraph() map[string]any {
	e.mu.RLock()
	defer e.mu.RUnlock()
	var mapped []*machine.Machine
	for _, m := range e.machines {
		if _, _, ok := m.Regions(); ok {
			mapped = append(mapped, m)
		}
	}
	machine.CanonicalOrder(mapped)
	nodes, edges := []any{}, []any{}
	for _, m := range mapped {
		view := m.View(false)
		mapping, _ := view["perceptualMapping"].(map[string]any)
		nodes = append(nodes, map[string]any{
			"id": m.ID, "name": m.Name, "description": m.Description,
			"inputMapping": mapping["input"], "outputMapping": mapping["output"], "metadata": view["metadata"],
		})
	}
	for _, src := range mapped {
		_, out, _ := src.Regions()
		srcMapping, _ := src.View(false)["perceptualMapping"].(map[string]any)
		for _, dst := range mapped {
			if src.ID == dst.ID {
				continue
			}
			in, _, _ := dst.Regions()
			if out.Offset+out.Length > in.Offset && out.Offset < in.Offset+in.Length {
				dstMapping, _ := dst.View(false)["perceptualMapping"].(map[string]any)
				edges = append(edges, map[string]any{
					"source": src.ID, "target": dst.ID,
					"sourceRegion": srcMapping["output"], "targetRegion": dstMapping["input"], "overlap": true,
				})
			}
		}
	}
	return map[string]any{"nodes": nodes, "edges": edges, "perceptualSpaceDimension": e.spaceWidth()}
}

// leadingNumber is strtod's reading of a token: leading space skipped, then
// the longest prefix that is a number.
func leadingNumber(token string) (float64, bool) {
	token = strings.TrimLeft(token, " \t\n\r\v\f")
	for end := len(token); end > 0; end-- {
		prefix := token[:end]
		if strings.ContainsAny(prefix, "_xXpP") {
			continue
		}
		if v, err := strconv.ParseFloat(prefix, 64); err == nil {
			return v, true
		}
	}
	return 0, false
}

// GovernanceRoute is GET /api/governance/route?machineId&sequenceId&values:
// the paging decision a firing with these values resolves to.
func (e *Engine) GovernanceRoute(machineID, sequenceID, raw *string) (map[string]any, error) {
	if machineID == nil || sequenceID == nil || raw == nil {
		return nil, machine.BadRequest("machineId, sequenceId, and values query parameters are required")
	}
	// getline on ',': a trailing separator ends the list, it opens no token.
	var values []float64
	if *raw != "" {
		for _, tok := range strings.Split(strings.TrimSuffix(*raw, ","), ",") {
			if v, ok := leadingNumber(tok); ok {
				values = append(values, v)
			}
		}
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	m, ok := e.machines[*machineID]
	if !ok {
		return nil, machine.NotFound("Machine not found: %s", *machineID)
	}
	decision := resolveGovernance(m, *sequenceID, values)
	if decision == nil {
		return nil, machine.NotFound("No triggerConfig rule matches (sequenceId=%s, values=%s)", *sequenceID, *raw)
	}
	return map[string]any{"success": true, "decision": decision}, nil
}

// Demo is GET /api/demo/<name>: the first resident machine (in id order) with
// the name, and its metadata summarised.
func (e *Engine) Demo(name, file string) (map[string]any, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	for _, id := range e.machineIDs() {
		m := e.machines[id]
		if m.Name != name {
			continue
		}
		view := m.View(true)
		meta := map[string]any{}
		for k, v := range view["metadata"].(map[string]any) {
			meta[k] = v
		}
		inputVectors := 0
		if seqs, ok := meta["inputSequences"].([]any); ok && len(seqs) > 0 {
			if first, ok := seqs[0].(map[string]any); ok {
				if events, ok := first["events"].([]any); ok {
					inputVectors = len(events)
				}
			}
		}
		seqs := m.SequencesInOrder()
		names := make([]string, len(seqs))
		for i, s := range seqs {
			names[i] = s.Name
		}
		meta["name"], meta["description"], meta["machineId"] = m.Name, m.Description, m.ID
		meta["totalSequences"], meta["sequenceNames"], meta["totalInputVectors"] = len(seqs), names, inputVectors
		return map[string]any{
			"success": true, "machine": view, "metadata": meta,
			"sequencesLoaded": len(seqs), "inputVectorsLoaded": inputVectors,
		}, nil
	}
	return nil, machine.NotFound("%s machine not found. Please ensure %s is loaded.", name, file)
}
