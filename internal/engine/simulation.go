package engine

import (
	"log"
	"math"
	"time"

	"github.com/jateeter/RealityEngine_GO/internal/machine"
)

// Perceptual simulation and the sampler (RealityEngine_GO#2, slice B).
// SURFACE_SPEC "/api/perceptual-simulation has one behaviour on every
// runtime" (RealityEngine_CI#489) governs; C++ PerceptualSpaceRuntime::
// configure / step is the concrete form, and Rust's simulation.rs prior art.
//
//   - configure/chunk buffers the input sequence's Reality Events (events)
//     and, from config or the top level, inputRegion, stepDelayMs and
//     maxSteps. configure/commit makes the buffer the committed sequence and
//     resets; with no region buffered it is a 400.
//   - step writes sequence[currentStep] at the region's offset over the space
//     as the last step left it and runs the phases POST /api/perceive runs.
//     Past the end, or at maxSteps, it answers {done, success} and stops.
//   - start auto-plays: a step now and then every stepDelayMs while the run is
//     live. Each start takes a new generation, so a loop left from an earlier
//     start exits instead of stepping beside it.
//   - The sampler is bookkeeping: start and stop set its state, sample counts
//     and echoes, and nothing samples on its own. A reset leaves it.

type simulation struct {
	sequence   [][]float64
	offset     int
	delayMS    int
	maxSteps   *int
	configured bool
	running    bool
	current    int
	generation int
	buffer     [][]float64
	buffered   *simConfig
}

type simConfig struct {
	offset, delayMS int
	maxSteps        *int
}

func (s *simulation) rewind() {
	s.running = false
	s.current = 0
}

func (s *simulation) done() bool {
	return s.current >= len(s.sequence) || (s.maxSteps != nil && s.current >= *s.maxSteps)
}

type sampler struct {
	running     bool
	strategy    string
	intervalMS  int64
	sampleCount int
}

func newSampler() sampler { return sampler{strategy: "MANUAL"} }

func (s sampler) stats() map[string]any {
	return map[string]any{
		"isRunning": s.running, "sampleCount": s.sampleCount, "bufferSize": 0,
		"strategy": s.strategy, "intervalMs": s.intervalMS,
	}
}

func lenientVector(v any) []float64 {
	l, _ := v.([]any)
	out := make([]float64, len(l))
	for i, x := range l {
		out[i], _ = x.(float64)
	}
	return out
}

// ConfigureChunk is POST /api/perceptual-simulation/configure/chunk.
func (e *Engine) ConfigureChunk(body map[string]any) map[string]any {
	e.mu.Lock()
	defer e.mu.Unlock()
	sim := &e.sim
	if r, _ := body["reset"].(bool); r {
		sim.buffer = nil
	}
	events, _ := body["events"].([]any)
	for _, ev := range events {
		sim.buffer = append(sim.buffer, lenientVector(ev))
	}
	config, ok := body["config"].(map[string]any)
	if !ok {
		config = body
	}
	if region, ok := config["inputRegion"].(map[string]any); ok {
		c := &simConfig{offset: 0, delayMS: 100}
		if f, ok := region["offset"].(float64); ok && f >= 0 && f == math.Trunc(f) {
			c.offset = int(f)
		}
		if f, ok := config["stepDelayMs"].(float64); ok {
			c.delayMS = int(f)
		}
		if f, ok := config["maxSteps"].(float64); ok {
			n := int(max(f, 0))
			c.maxSteps = &n
		}
		sim.buffered = c
	}
	return map[string]any{"success": true, "bufferedEvents": len(sim.buffer)}
}

// ConfigureCommit is POST /api/perceptual-simulation/configure/commit.
func (e *Engine) ConfigureCommit() (map[string]any, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	sim := &e.sim
	if sim.buffered == nil {
		return nil, machine.BadRequest("No config buffered. Send a chunk with config first.")
	}
	c := sim.buffered
	sim.buffered = nil
	sim.sequence, sim.buffer = sim.buffer, nil
	sim.offset, sim.delayMS, sim.maxSteps, sim.configured = c.offset, c.delayMS, c.maxSteps, true
	e.resetSpace()
	return map[string]any{"success": true}, nil
}

// resetSpace returns every machine to its initial Reality Events and the step
// state to its start; a simulation run stops at its start. The caller holds
// the write lock.
func (e *Engine) resetSpace() {
	// Every open K-line closes interrupted ("reset").
	if e.klines.controls.capture {
		if err := e.klines.onReset(); err != nil {
			e.klineFailed(err)
		}
	}
	for _, m := range e.machines {
		m.Reset()
	}
	e.steps.reset()
	e.retention.onReset()
	e.sim.rewind()
	e.completedStep = -1
}

// simulationStep runs one simulation step; nil when the walk is done. The
// caller holds the write lock.
func (e *Engine) simulationStep() (*step, error) {
	sim := &e.sim
	if !sim.configured {
		return nil, machine.BadRequest("Simulation not configured")
	}
	if sim.done() {
		sim.running = false
		return nil, nil
	}
	width := e.spaceWidth()
	ev := sim.sequence[sim.current]
	if sim.offset+len(ev) > width {
		return nil, &machine.Error{Status: 500, Message: "Region outside perceptual space"}
	}
	if len(e.steps.space) < width {
		e.steps.space = append(e.steps.space, make([]float64, width-len(e.steps.space))...)
	}
	copy(e.steps.space[sim.offset:], ev)
	s, err := e.runPhases(sim.current, "")
	if err != nil {
		return nil, err
	}
	sim.current++
	// A run ends on the step that finishes the walk, not on the call after it.
	if sim.running && sim.done() {
		sim.running = false
	}
	return s, nil
}

// SimulationStep is POST /api/perceptual-simulation/step.
func (e *Engine) SimulationStep() (map[string]any, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	s, err := e.simulationStep()
	if err != nil {
		return nil, err
	}
	if s == nil {
		return map[string]any{"done": true, "success": true}, nil
	}
	out := s.JSON(true, true, true, nil)
	out["clock"] = e.clockJSON(e.lastCommit, s.number) // as on POST /api/perceive (#600)
	return map[string]any{"success": true, "step": out}, nil
}

// SimulationReset is POST /api/perceptual-simulation/reset.
func (e *Engine) SimulationReset() map[string]any {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.resetSpace()
	return map[string]any{"success": true}
}

// SimulationStart is POST /api/perceptual-simulation/start: auto-play under
// a new generation.
func (e *Engine) SimulationStart() (map[string]any, error) {
	e.mu.Lock()
	if !e.sim.configured {
		e.mu.Unlock()
		return nil, machine.BadRequest("Simulation not configured")
	}
	e.sim.running = true
	e.sim.generation++
	generation := e.sim.generation
	e.mu.Unlock()
	go e.autoplay(generation)
	return map[string]any{"success": true}, nil
}

func (e *Engine) autoplay(generation int) {
	for {
		e.mu.Lock()
		if e.sim.generation != generation || !e.sim.running {
			e.mu.Unlock()
			return
		}
		s, err := e.simulationStep()
		if err != nil {
			log.Printf("perceptual-simulation auto-play stopped: %v", err)
			e.sim.running = false
		}
		if err != nil || s == nil || !e.sim.running {
			e.mu.Unlock()
			return
		}
		delay := max(e.sim.delayMS, 1)
		e.mu.Unlock()
		time.Sleep(time.Duration(delay) * time.Millisecond)
	}
}

// SimulationStop is POST /api/perceptual-simulation/stop.
func (e *Engine) SimulationStop() map[string]any {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sim.running = false
	return map[string]any{"success": true}
}

// SimulationState is GET /api/perceptual-simulation/state.
func (e *Engine) SimulationState() map[string]any {
	e.mu.RLock()
	space := make([]float64, max(e.spaceWidth(), len(e.steps.space)))
	copy(space, e.steps.space)
	current, running := e.sim.current, e.sim.running
	e.mu.RUnlock()
	return map[string]any{"state": map[string]any{
		"perceptualSpace": space, "currentStep": current, "isRunning": running,
		"machines": e.List()["machines"],
	}}
}

// SimulationHistory is GET /api/perceptual-simulation/history: every step,
// pushed or simulated, newest first, at most historyLimit.
func (e *Engine) SimulationHistory() map[string]any {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]any, len(e.steps.history))
	for i, s := range e.steps.history {
		out[i] = s.JSON(true, true, true, nil)
	}
	return map[string]any{"history": out}
}

// SamplerStart is POST /api/sampler/start.
func (e *Engine) SamplerStart(body map[string]any) map[string]any {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sampler.running = true
	e.sampler.strategy = "manual"
	if s, ok := body["strategy"].(string); ok {
		e.sampler.strategy = s
	}
	e.sampler.intervalMS = 0
	if f, ok := body["intervalMs"].(float64); ok {
		e.sampler.intervalMS = int64(f)
	}
	return map[string]any{"success": true, "stats": e.sampler.stats()}
}

// SamplerStop is POST /api/sampler/stop.
func (e *Engine) SamplerStop() map[string]any {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sampler.running = false
	return map[string]any{"success": true}
}

// SamplerSample is POST /api/sampler/sample: counted, and its data echoed.
func (e *Engine) SamplerSample(body map[string]any) map[string]any {
	e.mu.Lock()
	e.sampler.sampleCount++
	e.mu.Unlock()
	return map[string]any{"success": true, "result": map[string]any{
		"inputEvent": lenientVector(body["data"]), "processingTimestamp": clock.NowMS(),
	}}
}

// SamplerStats is GET /api/sampler/stats.
func (e *Engine) SamplerStats() map[string]any {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return map[string]any{"stats": e.sampler.stats()}
}
