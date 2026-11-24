package pe

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/jateeter/RealityEngine_GO/internal/engine"
	"github.com/jateeter/RealityEngine_GO/internal/instance"
)

// Sources and sensors (RealityEngine_GO#8): registration, activity, the
// canonical composition order, machine ingestion. SURFACE_SPEC "Sources &
// Sensors" and "Machine ingestion" govern; C++ PerceptionEngine is the
// concrete form, and Rust's pe/sources.rs prior art.
//
//   - Activity is earned. Registration derives active from the source's own
//     state, never from the flag a caller asks for: a sensor is active iff it
//     holds a value inside its TTL, a test source iff it has an interned
//     sequence, a simulated source always. Only a sensor value originates a
//     sensor's activity. An explicit active: false is honoured as a pause.
//   - A read reports stored AND validated, against one clock reading per
//     payload, and never writes the flag back.
//   - Composition is tiered (ARBITER_CONTRACT §4.4b): seed (test) sources
//     first, then live ones; within a tier newest activation first, then
//     descending (name, id), so the incumbent writes last and keeps the cell.
//     Listings use plain (name, id) order.
//   - Machine ingestion interns each machine's metadata.inputSequences, end
//     to end, as one test source over its input region, id test-<machine id>.
//     A machine that already has one is skipped; PE_SOURCE_MERGE=false
//     rebuilds a source whose definition changed.

var patterns = []string{"sine", "sawtooth", "square", "linear-ramp", "random-walk", "constant", "gaussian-noise", "binary"}

// Region is a source's span of the perceptual space.
type Region struct{ Offset, Length int64 }

func (r Region) json() map[string]any { return map[string]any{"offset": r.Offset, "length": r.Length} }
func (r Region) end() int64           { return r.Offset + r.Length }

// Source is a registered source; exactly one of test, sensor, simulated is
// set.
type Source struct {
	ID          string
	Name        string
	Region      Region
	Active      bool
	ActivatedAt uint64
	test        *testDetail
	sensor      *sensorDetail
	simulated   *simDetail
}

type testDetail struct {
	machineID, machineName, sequenceName string
	inputs                               [][]float64
	loop                                 bool
	metadata, sequence                   any
}

type sensorDetail struct {
	sensorID    string
	lastValue   []float64
	lastUpdated *int64
	ttlMS       int64
	origin      string
}

type simDetail struct {
	pattern                        string
	frequency, amplitude, dcOffset float64
}

func (s *Source) kind() string {
	switch {
	case s.test != nil:
		return "test"
	case s.sensor != nil:
		return "sensor"
	}
	return "simulated"
}

func (s *Source) clone() *Source {
	c := *s
	if s.test != nil {
		t := *s.test
		c.test = &t
	}
	if s.sensor != nil {
		t := *s.sensor
		t.lastValue = append([]float64(nil), s.sensor.lastValue...)
		c.sensor = &t
	}
	if s.simulated != nil {
		t := *s.simulated
		c.simulated = &t
	}
	return &c
}

func numbers(v any) []float64 {
	l, _ := v.([]any)
	out := make([]float64, len(l))
	for i, x := range l {
		out[i], _ = x.(float64)
	}
	return out
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

func objectOr(v any) any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

func pattern(name string) (string, error) {
	for _, p := range patterns {
		if p == name {
			return p, nil
		}
	}
	return "", &Error{500, "Unknown sim pattern: " + name}
}

func regionFrom(v any) Region {
	r, ok := v.(map[string]any)
	if !ok {
		return Region{}
	}
	return Region{int64(num(r["offset"], 0)), int64(num(r["length"], 0))}
}

// sourceFrom reads a source definition (POST /api/sources).
func sourceFrom(j map[string]any) (*Source, error) {
	kind := str(j["type"], str(j["kind"], "simulated"))
	s := &Source{ID: str(j["id"], ""), Name: str(j["name"], "source"), Region: regionFrom(j["region"])}
	s.Active = true
	if b, ok := j["active"].(bool); ok {
		s.Active = b
	}
	switch kind {
	case "test":
		var inputs [][]float64
		l, _ := j["inputs"].([]any)
		for _, x := range l {
			inputs = append(inputs, numbers(x))
		}
		loop := true
		if b, ok := j["loop"].(bool); ok {
			loop = b
		}
		s.test = &testDetail{
			machineID: str(j["machineId"], ""), machineName: str(j["machineName"], ""),
			sequenceName: str(j["sequenceName"], ""), inputs: inputs, loop: loop,
			metadata: objectOr(j["metadata"]), sequence: objectOr(j["sequence"]),
		}
	case "sensor":
		d := &sensorDetail{
			sensorID: str(j["sensorId"], ""), lastValue: numbers(j["lastValue"]),
			ttlMS: int64(num(j["ttlMs"], 5000)), origin: str(j["origin"], ""),
		}
		if f, ok := j["lastUpdated"].(float64); ok {
			at := int64(f)
			d.lastUpdated = &at
		}
		s.sensor = d
	default:
		p, err := pattern(str(j["pattern"], "constant"))
		if err != nil {
			return nil, err
		}
		s.simulated = &simDetail{pattern: p, frequency: num(j["frequency"], 1), amplitude: num(j["amplitude"], 1), dcOffset: num(j["dcOffset"], 0)}
	}
	return s, nil
}

// patched applies PATCH /api/sources/:id.
func (s *Source) patched(p map[string]any) (*Source, error) {
	c := s.clone()
	if name, ok := p["name"].(string); ok {
		c.Name = name
	}
	if b, ok := p["active"].(bool); ok {
		c.Active = b
	}
	if r, ok := p["region"].(map[string]any); ok {
		if f, ok := r["offset"].(float64); ok {
			c.Region.Offset = int64(f)
		}
		if f, ok := r["length"].(float64); ok {
			c.Region.Length = int64(f)
		}
	}
	switch {
	case c.test != nil:
		if b, ok := p["loop"].(bool); ok {
			c.test.loop = b
		}
	case c.sensor != nil:
		if v, ok := p["sensorId"].(string); ok {
			c.sensor.sensorID = v
		}
		if f, ok := p["ttlMs"].(float64); ok {
			c.sensor.ttlMS = int64(f)
		}
	default:
		if v, ok := p["pattern"].(string); ok {
			pat, err := pattern(v)
			if err != nil {
				return nil, err
			}
			c.simulated.pattern = pat
		}
		for key, field := range map[string]*float64{"frequency": &c.simulated.frequency, "amplitude": &c.simulated.amplitude, "dcOffset": &c.simulated.dcOffset} {
			if f, ok := p[key].(float64); ok {
				*field = f
			}
		}
	}
	return c, nil
}

func (s *Source) live(now int64) bool {
	return s.sensor != nil && s.sensor.lastUpdated != nil && now-*s.sensor.lastUpdated <= s.sensor.ttlMS
}

func (s *Source) validated(now int64) bool {
	switch {
	case s.sensor != nil:
		return s.live(now)
	case s.test != nil:
		return len(s.test.inputs) > 0
	}
	return true
}

func (s *Source) json(now int64) map[string]any {
	out := map[string]any{
		"id": s.ID, "name": s.Name, "region": s.Region.json(),
		"active": s.Active && s.validated(now), "type": s.kind(),
	}
	switch {
	case s.test != nil:
		inputs := s.test.inputs
		if inputs == nil {
			inputs = [][]float64{}
		}
		out["machineId"], out["machineName"], out["sequenceName"] = s.test.machineID, s.test.machineName, s.test.sequenceName
		out["inputs"], out["loop"], out["metadata"], out["sequence"] = inputs, s.test.loop, s.test.metadata, s.test.sequence
	case s.sensor != nil:
		var lastUpdated any
		if s.sensor.lastUpdated != nil {
			lastUpdated = *s.sensor.lastUpdated
		}
		out["sensorId"], out["lastValue"], out["lastUpdated"], out["ttlMs"] = s.sensor.sensorID, nonNil(s.sensor.lastValue), lastUpdated, s.sensor.ttlMS
		if s.sensor.origin != "" {
			out["origin"] = s.sensor.origin
		}
	default:
		out["pattern"], out["frequency"], out["amplitude"], out["dcOffset"] = s.simulated.pattern, s.simulated.frequency, s.simulated.amplitude, s.simulated.dcOffset
	}
	return out
}

func nonNil(v []float64) []float64 {
	if v == nil {
		return []float64{}
	}
	return v
}

// idPart is an id fragment: lower-case ASCII letters and digits, any run of
// other bytes one "-", none leading or trailing; "unnamed" when empty.
func idPart(v string) string {
	var b strings.Builder
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case c >= 'A' && c <= 'Z':
			b.WriteByte(c + 32)
		case c >= 'a' && c <= 'z' || c >= '0' && c <= '9':
			b.WriteByte(c)
		default:
			if b.Len() > 0 && !strings.HasSuffix(b.String(), "-") {
				b.WriteByte('-')
			}
		}
	}
	out := strings.TrimRight(b.String(), "-")
	if out == "" {
		return "unnamed"
	}
	return out
}

func (p *PE) ensureCapacity(end int64) {
	if end > int64(p.dimension) {
		p.dimension = int(end)
		p.persistent = append(p.persistent, make([]float64, p.dimension-len(p.persistent))...)
	}
}

// addSource registers or replaces a source. Its activity is derived, not
// asked for; a source that stays active keeps its activation step.
func (p *PE) addSource(s *Source) *Source {
	if s.ID == "" {
		s.ID = "source-" + instance.NewV7()
	}
	s.Active = s.validated(nowMS())
	if prev, ok := p.sources[s.ID]; ok && prev.Active && s.Active {
		s.ActivatedAt = prev.ActivatedAt
	} else {
		s.ActivatedAt = p.globalStep
	}
	p.ensureCapacity(s.Region.end())
	p.testStep[s.ID] = 0
	if s.simulated != nil && s.simulated.pattern == "random-walk" {
		p.walk[s.ID] = filled(s.simulated.dcOffset, s.Region.Length)
	}
	p.sources[s.ID] = s
	return s
}

func filled(v float64, n int64) []float64 {
	out := make([]float64, max(n, 0))
	for i := range out {
		out[i] = v
	}
	return out
}

func (p *PE) removeSource(id string) bool {
	delete(p.testStep, id)
	delete(p.walk, id)
	delete(p.contention.counters, id)
	_, ok := p.sources[id]
	delete(p.sources, id)
	return ok
}

func (p *PE) sourcesInOrder() []*Source {
	out := make([]*Source, 0, len(p.sources))
	for _, s := range p.sources {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func (p *PE) sourcesJSON() []any {
	now := nowMS()
	out := []any{}
	for _, s := range p.sourcesInOrder() {
		out = append(out, s.json(now))
	}
	return out
}

// updateSensor writes a sensor's value; false when no source has that
// sensorId.
func (p *PE) updateSensor(sensorID string, values []float64) bool {
	ids := make([]string, 0, len(p.sources))
	for id := range p.sources {
		ids = append(ids, id)
	}
	sort.Strings(ids) // the first sensor by id takes the value
	for _, id := range ids {
		s := p.sources[id]
		if s.sensor == nil || s.sensor.sensorID != sensorID {
			continue
		}
		at := nowMS()
		s.sensor.lastValue, s.sensor.lastUpdated = values, &at
		if !s.Active {
			s.ActivatedAt = p.globalStep
		}
		s.Active = true
		return true
	}
	return false
}

func (p *PE) sensorByID(sensorID string) *Source {
	for _, s := range p.sourcesInOrder() {
		if s.sensor != nil && s.sensor.sensorID == sensorID {
			return s
		}
	}
	return nil
}

// compositionOrder is the active sources, seed tier first, newest activation
// first, then descending (name, id): the last writer keeps a cell.
func (p *PE) compositionOrder() []*Source {
	var active []*Source
	for _, s := range p.sources {
		if s.Active {
			active = append(active, s)
		}
	}
	sort.Slice(active, func(i, j int) bool {
		a, b := active[i], active[j]
		aLive, bLive := a.kind() != "test", b.kind() != "test"
		if aLive != bLive {
			return !aLive
		}
		if a.ActivatedAt != b.ActivatedAt {
			return a.ActivatedAt > b.ActivatedAt
		}
		if a.Name != b.Name {
			return a.Name > b.Name
		}
		return a.ID > b.ID
	})
	return active
}

func (p *PE) uniform() float64 {
	x := p.rng
	x ^= x << 13
	x ^= x >> 7
	x ^= x << 17
	p.rng = x
	return float64(x>>11) / float64(uint64(1)<<53)
}

func (p *PE) sourceValues(s *Source, now int64) []float64 {
	length := int(max(s.Region.Length, 0))
	switch {
	case s.test != nil:
		step := p.testStep[s.ID]
		if step < len(s.test.inputs) {
			return s.test.inputs[step]
		}
		return make([]float64, length)
	case s.sensor != nil:
		out := make([]float64, length)
		if s.live(now) {
			copy(out, s.sensor.lastValue)
		}
		return out
	}
	d := s.simulated
	out := make([]float64, length)
	for i := range out {
		t := float64(p.globalStep) + float64(i)*0.1
		period := 1.0
		if d.frequency > 0 {
			period = 1 / d.frequency
		}
		phase := math.Mod(t/period, 1)
		switch d.pattern {
		case "sine":
			out[i] = d.dcOffset + d.amplitude*math.Sin(2*math.Pi*phase)
		case "sawtooth":
			out[i] = d.dcOffset + d.amplitude*(2*phase-1)
		case "square":
			sq := -1.0
			if phase < 0.5 {
				sq = 1
			}
			out[i] = d.dcOffset + d.amplitude*sq
		case "linear-ramp":
			out[i] = d.dcOffset + d.amplitude*phase
		case "random-walk":
			out[i] = d.dcOffset
			if w := p.walk[s.ID]; len(w) > 0 {
				out[i] = w[0]
			}
		case "gaussian-noise":
			out[i] = d.dcOffset + d.amplitude*(p.uniform()*2-1)
		case "binary":
			if phase < 0.5 {
				out[i] = 1
			}
		default:
			out[i] = d.dcOffset
		}
	}
	return out
}

func clamp01(v float64) float64 { return math.Min(math.Max(v, 0), 1) }

// assemble is the vector the active sources compose now, over the persistent
// vector, each cell clamped to [0, 1], with the OSRE fold applied.
func (p *PE) assemble() []float64 { return p.assembleRecording(nil) }

// provider is the source's provider for arbitration: its origin's head,
// sensor or synthetic without one, openclaw read as acp and ollama or
// localaistack as localai.
func (s *Source) provider() string {
	origin := ""
	if s.sensor != nil {
		origin = strings.ToLower(s.sensor.origin)
	}
	head, _, _ := strings.Cut(origin, ".")
	switch head {
	case "", "signal":
		if s.sensor != nil {
			return "sensor"
		}
		return "synthetic"
	case "openclaw":
		return "acp"
	case "ollama", "localaistack":
		return "localai"
	}
	return head
}

// foldUnitInterval is a machine's outputMergeTransformation over [0, 1].
func foldUnitInterval(transformation string, s, o float64) float64 {
	switch transformation {
	case "and", "meet", "discrete-median":
		return math.Min(s, o)
	case "strong-disjunction":
		return math.Min(s+o, 1)
	case "strong-conjunction":
		return math.Max(s+o-1, 0)
	case "xor":
		return math.Max(math.Min(s, 1-o), math.Min(1-s, o))
	case "nor":
		return 1 - math.Max(s, o)
	case "nand":
		return 1 - math.Min(s, o)
	}
	return math.Max(s, o)
}

// assembleRecording composes the sources, then folds every source-written
// cell the last push's mergeBatch also wrote with the OSRE value
// (ARBITER_CONTRACT §4.4b, RealityEngine_CI#525): by the cell's declared rule
// when the registry ranks the source's provider, else by the writing
// machine's outputMergeTransformation. Each fold is recorded when folds is
// given.
func (p *PE) assembleRecording(folds *[]any) []float64 {
	now := nowMS()
	out := append([]float64(nil), p.persistent...)
	writer := make([]*Source, len(out))
	for _, s := range p.compositionOrder() {
		values := p.sourceValues(s, now)
		for i := 0; i < int(max(s.Region.Length, 0)) && i < len(values); i++ {
			cell := s.Region.Offset + int64(i)
			if cell < 0 || cell >= int64(len(out)) {
				continue
			}
			out[cell] = clamp01(values[i])
			writer[cell] = s
		}
	}
	cells := make([]int64, 0, len(p.osreFold))
	for c := range p.osreFold {
		cells = append(cells, c)
	}
	sort.Slice(cells, func(i, j int) bool { return cells[i] < cells[j] })
	for _, cell := range cells {
		if cell < 0 || cell >= int64(len(writer)) || writer[cell] == nil {
			continue
		}
		machineName, transformation := p.osreFold[cell][0], p.osreFold[cell][1]
		src, c := writer[cell], int(cell)
		sv, ov := out[c], p.persistent[c]
		provider := src.provider()
		rule, ranks, declared := p.registry.Entry(c)
		_, named := ranks[provider]
		named = declared && named
		byRule, osreWins := false, false
		if named && rule == "PRECEDENCE" {
			rank := func(pr string) float64 {
				if r, ok := ranks[pr]; ok {
					return r
				}
				return engine.DeterminismRank(pr)
			}
			osreRank, sourceRank := rank("machine"), rank(provider)
			byRule, osreWins = osreRank != sourceRank, osreRank > sourceRank
		}
		resolved := foldUnitInterval(transformation, sv, ov)
		if byRule {
			resolved = sv
			if osreWins {
				resolved = ov
			}
		}
		out[c] = clamp01(resolved)
		if folds == nil {
			continue
		}
		record := map[string]any{
			"cell":     cell,
			"osre":     map[string]any{"machine": machineName, "provider": "machine", "value": ov},
			"source":   map[string]any{"id": src.ID, "name": src.Name, "kind": src.kind(), "provider": provider, "value": sv},
			"resolved": resolved,
		}
		if byRule {
			record["resolution"], record["rule"] = "declared-rule", rule
			record["kept"] = "source"
			if osreWins {
				record["kept"] = "osre"
			}
		} else {
			record["resolution"], record["operator"] = "osre-fold", transformation
			if declared {
				record["declaredRule"] = rule
				if !named {
					record["review"] = "provider-unranked"
				}
			}
			switch {
			case resolved == ov && resolved == sv:
				record["kept"] = "both"
			case resolved == ov:
				record["kept"] = "osre"
			case resolved == sv:
				record["kept"] = "source"
			default:
				record["kept"] = "combined"
			}
		}
		*folds = append(*folds, record)
	}
	return out
}

// recordContention records, at a push only, the contended cells, the folds
// and each source's counters (contended, and lost).
func (p *PE) recordContention() {
	cells := p.sourceContention()
	folds := []any{}
	p.assembleRecording(&folds)
	contended, lost := map[string]bool{}, map[string]bool{}
	for _, f := range folds {
		fm := f.(map[string]any)
		id := fm["source"].(map[string]any)["id"].(string)
		if fm["kept"] == "osre" {
			lost[id] = true
		}
		contended[id] = true
	}
	for _, cv := range cells {
		cm := cv.(map[string]any)
		contended[cm["winner"].(map[string]any)["id"].(string)] = true
		for _, l := range cm["suppressed"].([]any) {
			id := l.(map[string]any)["id"].(string)
			contended[id], lost[id] = true, true
		}
	}
	for id := range contended {
		c := p.contention.counters[id]
		c[0]++
		if lost[id] {
			c[1]++
		}
		p.contention.counters[id] = c
	}
	p.contention.transition, p.contention.cells, p.contention.folds = p.globalStep, cells, folds
}

// advance moves every active source on a step: a test source to its next
// event (a non-looping one deactivates at its end), a random walk by a step.
func (p *PE) advance() {
	p.globalStep++
	ids := make([]string, 0, len(p.sources))
	for id := range p.sources {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		s := p.sources[id]
		if !s.Active {
			continue
		}
		switch {
		case s.test != nil:
			next := p.testStep[id] + 1
			if next >= len(s.test.inputs) {
				p.testStep[id] = 0
				if !s.test.loop {
					s.Active = false
				}
			} else {
				p.testStep[id] = next
			}
		case s.simulated != nil && s.simulated.pattern == "random-walk":
			walk := p.walk[id]
			for i := range walk {
				walk[i] = clamp01(walk[i] + (p.uniform()*2-1)*0.05)
			}
		}
	}
}

// adoptSpace makes the RE's returned space the persistent vector.
func (p *PE) adoptSpace(values []float64) {
	p.ensureCapacity(int64(len(values)))
	p.persistent = make([]float64, p.dimension)
	copy(p.persistent, values)
}

// sourceContention is every cell more than one active source writes, the
// last writer in composition order keeping it.
func (p *PE) sourceContention() []any {
	now := nowMS()
	size := int64(len(p.persistent))
	writers := map[int64][]*Source{}
	for _, s := range p.compositionOrder() {
		values := p.sourceValues(s, now)
		for i := 0; i < min(int(max(s.Region.Length, 0)), len(values)); i++ {
			if cell := s.Region.Offset + int64(i); cell >= 0 && cell < size {
				writers[cell] = append(writers[cell], s)
			}
		}
	}
	cells := make([]int64, 0, len(writers))
	for c, ws := range writers {
		if len(ws) > 1 {
			cells = append(cells, c)
		}
	}
	sort.Slice(cells, func(i, j int) bool { return cells[i] < cells[j] })
	ref := func(s *Source) map[string]any {
		return map[string]any{"id": s.ID, "name": s.Name, "kind": s.kind(), "activatedAt": s.ActivatedAt}
	}
	out := []any{}
	for _, c := range cells {
		ws := append([]*Source(nil), writers[c]...)
		winner := ws[len(ws)-1]
		ws = ws[:len(ws)-1]
		sort.Slice(ws, func(i, j int) bool {
			if ws[i].Name != ws[j].Name {
				return ws[i].Name < ws[j].Name
			}
			return ws[i].ID < ws[j].ID
		})
		winnerLive := winner.kind() != "test"
		sameTier := false
		suppressed := []any{}
		for _, l := range ws {
			if (l.kind() != "test") == winnerLive {
				sameTier = true
			}
			suppressed = append(suppressed, ref(l))
		}
		resolution := "live-over-seed"
		if sameTier {
			resolution = "incumbent"
		}
		out = append(out, map[string]any{"cell": c, "resolution": resolution, "winner": ref(winner), "suppressed": suppressed})
	}
	return out
}

// resetSources: each test source back to its first event, each random walk to
// its offset, activity revalidated and activation cleared; membership, last
// values and timestamps stay.
func (p *PE) resetSources() {
	now := nowMS()
	for id, s := range p.sources {
		p.testStep[id] = 0
		if s.simulated != nil && s.simulated.pattern == "random-walk" {
			p.walk[id] = filled(s.simulated.dcOffset, s.Region.Length)
		}
		s.Active = s.validated(now)
		s.ActivatedAt = 0
	}
	p.contention = newContention()
	p.osreFold = map[int64][2]string{}
}

func (p *PE) contentionJSON() map[string]any {
	counters := []any{}
	for _, s := range p.sourcesInOrder() {
		if c, ok := p.contention.counters[s.ID]; ok {
			counters = append(counters, map[string]any{"id": s.ID, "name": s.Name, "contended": c[0], "suppressed": c[1]})
		}
	}
	return map[string]any{"transition": p.contention.transition, "cells": p.contention.cells, "folds": p.contention.folds, "counters": counters}
}

// intern makes one test source from a machine's declared inputSequences;
// 1 when it created or rebuilt one.
func (p *PE) intern(m map[string]any) int {
	machineID, _ := m["id"].(string)
	if machineID == "" {
		return 0
	}
	mapping, ok := m["perceptualMapping"].(map[string]any)
	input, ok2 := mapping["input"].(map[string]any)
	if !ok || !ok2 {
		return 0
	}
	meta, _ := m["metadata"].(map[string]any)
	seqs, ok := meta["inputSequences"].([]any)
	if !ok {
		return 0
	}
	id := "test-" + idPart(machineID)
	present, has := p.sources[id]
	if has && p.mergeOnly {
		return 0
	}
	machineName := str(m["name"], machineID)
	var inputs [][]float64
	type segment struct {
		name   string
		length int
	}
	var segments []segment
	for _, sv := range seqs {
		seq, _ := sv.(map[string]any)
		events, _ := seq["events"].([]any)
		if len(events) == 0 {
			continue
		}
		for _, ev := range events {
			inputs = append(inputs, numbers(ev))
		}
		segments = append(segments, segment{str(seq["name"], "Test sequence"), len(events)})
	}
	if len(inputs) == 0 {
		return 0
	}
	sequenceName := fmt.Sprintf("%d sequences", len(segments))
	if len(segments) == 1 {
		sequenceName = segments[0].name
	}
	segJSON := make([]any, len(segments))
	for i, sg := range segments {
		segJSON[i] = map[string]any{"name": sg.name, "length": sg.length}
	}
	s := &Source{
		ID: id, Name: machineName + " / " + sequenceName,
		Region: Region{int64(num(input["offset"], 0)), int64(num(input["length"], 0))},
		test: &testDetail{
			machineID: machineID, machineName: machineName, sequenceName: sequenceName,
			inputs: inputs, loop: true, metadata: map[string]any{"segments": segJSON}, sequence: map[string]any{},
		},
	}
	if has && present.test != nil && present.Region == s.Region && present.test.loop == s.test.loop && equalInputs(present.test.inputs, inputs) {
		return 0
	}
	p.addSource(s)
	return 1
}

func equalInputs(a, b [][]float64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if len(a[i]) != len(b[i]) {
			return false
		}
		for j := range a[i] {
			if a[i][j] != b[i][j] {
				return false
			}
		}
	}
	return true
}

// bootstrap interns every machine of an RE listing. A machine holding more
// than one test source loses them all first, so the rebuild is clean.
func (p *PE) bootstrap(listing map[string]any) map[string]any {
	perMachine := map[string][]string{}
	for _, s := range p.sources {
		if s.test != nil && s.test.machineID != "" {
			perMachine[s.test.machineID] = append(perMachine[s.test.machineID], s.ID)
		}
	}
	for _, ids := range perMachine {
		if len(ids) > 1 {
			for _, id := range ids {
				p.removeSource(id)
			}
		}
	}
	created, skipped, seen := 0, 0, 0
	machines, _ := listing["machines"].([]any)
	for _, mv := range machines {
		seen++
		m, _ := mv.(map[string]any)
		if added := p.intern(m); added > 0 {
			created += added
		} else {
			skipped++
		}
	}
	return map[string]any{"created": created, "errors": []any{}, "machinesSeen": seen, "skipped": skipped, "success": true}
}

func bootstrapFailed(err error) map[string]any {
	return map[string]any{"created": 0, "errors": []any{err.Error()}, "machinesSeen": 0, "skipped": 0, "success": true}
}

// ingestSignal is POST /api/signals: a value for a sensor, registering one
// when the signal names a region.
func (p *PE) ingestSignal(body map[string]any) (*Source, error) {
	values := numbers(body["values"])
	if len(values) == 0 {
		return nil, &Error{400, "values must be a non-empty array"}
	}
	origin := str(body["origin"], "signal")
	region, hasRegion := body["region"].(map[string]any)
	sensor := func(id, name, sensorID string) *Source {
		at := nowMS()
		return &Source{ID: id, Name: name, Region: regionFrom(region), Active: true, sensor: &sensorDetail{
			sensorID: sensorID, lastValue: values, lastUpdated: &at, ttlMS: int64(num(body["ttlMs"], 30000)), origin: origin,
		}}
	}
	if sensorID, ok := body["sensorId"].(string); ok {
		if p.updateSensor(sensorID, values) {
			return p.sensorByID(sensorID), nil
		}
		if !hasRegion {
			return nil, &Error{404, fmt.Sprintf("No sensor source with sensorId %q", sensorID)}
		}
		return p.addSource(sensor(sensorID, str(body["name"], sensorID), sensorID)), nil
	}
	if !hasRegion {
		return nil, &Error{400, "signal requires sensorId or region"}
	}
	name := str(body["name"], "external/signal")
	sensorID := str(body["name"], "external-sensor-"+instance.NewV7())
	return p.addSource(sensor("", name, sensorID)), nil
}
