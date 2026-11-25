package pe

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jateeter/RealityEngine_GO/internal/instance"
	"github.com/jateeter/RealityEngine_GO/internal/stream"
)

// The push cycle (RealityEngine_GO#11): POST /api/push, GET /api/push/:id and
// auto push. C++ do_push / execute_push is the concrete form, and SURFACE_SPEC
// "POST /api/push response shape" the contract; Rust's pe/push.rs is prior
// art.
//
//   - A push assembles the universal Reality Event, records its contention,
//     sends it to POST {re}/api/perceive with every observation field asked
//     for, folds the gated machine outputs into the perceptual space the RE
//     returns, and advances the sources. What the caller asked to be shown
//     (compact, includeMachineResults, only) trims the reply afterwards and
//     never changes what was computed (RealityEngine_CI#367).
//   - One push at a time. A push arriving while one is in flight answers 409
//     coalesced and is owed one more push, run when the in-flight one ends.
//   - async: true answers 202 with a job id; GET /api/push/:id reports the
//     job and its result once it has one. The newest 256 jobs are kept.
//   - A push the RE refuses answers 200 with success: false and the error:
//     the push happened, the step did not.
//   - The RE's machine catalog is read on every push for the OSRE fold: each
//     mergeBatch cell's writing machine and its outputMergeTransformation.

const pushRecords = 256

type job struct {
	status             string
	createdAt, updated int64
	result             map[string]any
}

type pushes struct {
	mu       sync.Mutex
	inFlight bool
	owed     int
	jobs     map[string]*job
	order    []string
	autoGen  int
}

func newPushes() *pushes { return &pushes{jobs: map[string]*job{}} }

func (q *pushes) record(id, status string, result map[string]any) {
	q.mu.Lock()
	defer q.mu.Unlock()
	now := nowMS()
	if j, ok := q.jobs[id]; ok {
		j.status, j.updated, j.result = status, now, result
		return
	}
	q.jobs[id] = &job{status: status, createdAt: now, updated: now, result: result}
	q.order = append(q.order, id)
	for len(q.order) > pushRecords {
		delete(q.jobs, q.order[0])
		q.order = q.order[1:]
	}
}

func (q *pushes) claim() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.inFlight {
		q.owed++
		return false
	}
	q.inFlight = true
	return true
}

// releaseUnlessOwed frees the slot, or keeps it when a push is owed.
func (q *pushes) releaseUnlessOwed() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.owed == 0 {
		q.inFlight = false
		return true
	}
	q.owed = 0
	return false
}

func (q *pushes) release() {
	q.mu.Lock()
	q.inFlight, q.owed = false, 0
	q.mu.Unlock()
}

// postRE posts JSON to a route of this PE's RE.
func (p *PE) postRE(path string, body any) ([]byte, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	url := p.reURL + path
	resp, err := p.client.Post(url, "application/json", bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("POST %s: %w", url, err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("POST %s: %w", url, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("POST %s: HTTP %s: %s", url, resp.Status, out)
	}
	return out, nil
}

// refreshCatalog caches the RE's machines by id; a failed read keeps the old
// cache.
func (p *PE) refreshCatalog() {
	data, err := p.getRE("/api/machines")
	if err != nil {
		return
	}
	var listing map[string]any
	if json.Unmarshal(data, &listing) != nil {
		return
	}
	p.mu.Lock()
	p.cacheCatalog(listing)
	p.mu.Unlock()
}

func (p *PE) cacheCatalog(listing map[string]any) {
	catalog := map[string]map[string]any{}
	machines, _ := listing["machines"].([]any)
	for _, mv := range machines {
		if m, ok := mv.(map[string]any); ok {
			if id, _ := m["id"].(string); id != "" {
				catalog[id] = m
			}
		}
	}
	p.catalog = catalog
	p.catalogLoaded, p.catalogRefreshedAt = true, nowMS()
}

// osreFoldCells maps each mergeBatch cell to its writing machine's name and
// outputMergeTransformation; where machines overlap, the smallest name.
func osreFoldCells(step map[string]any, catalog map[string]map[string]any) map[int64][2]string {
	cells := map[int64][2]string{}
	batch, _ := step["mergeBatch"].([]any)
	for _, ov := range batch {
		op, _ := ov.(map[string]any)
		id, _ := op["machineId"].(string)
		region, ok := op["region"].(map[string]any)
		if id == "" || !ok {
			continue
		}
		name, transformation := str(op["machineName"], id), "or"
		if m, ok := catalog[id]; ok {
			name, transformation = str(m["name"], name), str(m["outputMergeTransformation"], "or")
		}
		offset, length := int64(num(region["offset"], -1)), int64(num(region["length"], 0))
		if offset < 0 {
			continue
		}
		for c := offset; c < offset+length; c++ {
			if held, ok := cells[c]; ok && held[0] <= name {
				continue
			}
			cells[c] = [2]string{name, transformation}
		}
	}
	return cells
}

// aggregate writes each gated machine output into the space the RE returned,
// where exactly one machine writes the cell.
func aggregate(base []float64, results map[string]any) []float64 {
	type record struct {
		name   string
		offset int
		values []float64
	}
	var records []record
	for id, rv := range results {
		r, _ := rv.(map[string]any)
		tr, _ := r["transitionResult"].(map[string]any)
		meta, _ := tr["arbiterMetadata"].(map[string]any)
		if meta["shouldOutput"] != true {
			continue
		}
		region, ok := r["outputRegion"].(map[string]any)
		if !ok {
			continue
		}
		offset, length := int(num(region["offset"], -1)), int(num(region["length"], 0))
		if offset < 0 || length <= 0 {
			continue
		}
		vector, ok := r["mergedOutputVector"].([]any)
		if !ok {
			if vector, ok = r["outputVector"].([]any); !ok {
				continue
			}
		}
		values := numbers(vector)
		if len(values) == 0 {
			continue
		}
		if len(values) > length {
			values = values[:length]
		}
		records = append(records, record{str(r["machineName"], id), offset, values})
	}
	sort.SliceStable(records, func(i, j int) bool { return records[i].name < records[j].name })
	var writers []int
	for _, r := range records {
		if end := r.offset + len(r.values); end > len(writers) {
			writers = append(writers, make([]int, end-len(writers))...)
		}
		for i := range r.values {
			writers[r.offset+i]++
		}
	}
	for _, r := range records {
		if end := r.offset + len(r.values); end > len(base) {
			base = append(base, make([]float64, end-len(base))...)
		}
		for i, v := range r.values {
			if writers[r.offset+i] == 1 {
				base[r.offset+i] = v
			}
		}
	}
	return base
}

// selectStep trims a push reply to what the only selector attributes.
func selectStep(step map[string]any, only any) {
	o, ok := only.(map[string]any)
	if !ok {
		return
	}
	set := func(v any) map[string]bool {
		out := map[string]bool{}
		l, _ := v.([]any)
		for _, x := range l {
			if s, ok := x.(string); ok {
				out[s] = true
			}
		}
		return out
	}
	wantSeq, wantName := set(o["sequenceIds"]), set(o["machineNames"])
	results, _ := step["machineResults"].(map[string]any)
	named := map[string]bool{}
	for id, rv := range results {
		if r, ok := rv.(map[string]any); ok && wantName[str(r["machineName"], "")] {
			named[id] = true
		}
	}
	carries := func(op map[string]any) bool {
		for s := range set(op["sequenceIds"]) {
			if wantSeq[s] {
				return true
			}
		}
		return false
	}
	bySequence := map[string]bool{}
	batch, _ := step["mergeBatch"].([]any)
	for _, ov := range batch {
		if op, ok := ov.(map[string]any); ok && carries(op) {
			bySequence[str(op["machineId"], "")] = true
		}
	}
	if results != nil {
		kept := map[string]any{}
		for id, r := range results {
			if named[id] || bySequence[id] {
				kept[id] = r
			}
		}
		step["machineResults"] = kept
	}
	keep := func(key string, f func(map[string]any) bool) {
		items, ok := step[key].([]any)
		if !ok {
			return
		}
		out := []any{}
		for _, v := range items {
			if m, ok := v.(map[string]any); ok && f(m) {
				out = append(out, v)
			}
		}
		step[key] = out
	}
	keep("mergeBatch", func(op map[string]any) bool { return named[str(op["machineId"], "")] || carries(op) })
	keep("eventBus", func(w map[string]any) bool {
		return wantSeq[str(w["producerSequenceId"], "")] || named[str(w["producerMachineId"], "")] || named[str(w["subscriberMachineId"], "")]
	})
	keep("activeRegions", func(r map[string]any) bool { return named[str(r["machineId"], "")] })
}

func (p *PE) failure(err error) map[string]any {
	p.mu.RLock()
	step := p.globalStep
	p.mu.RUnlock()
	return map[string]any{"success": false, "step": nil, "timestamp": nowMS(), "globalStep": step, "error": err.Error()}
}

func (p *PE) transition(includeMachineResults bool, only any) map[string]any {
	p.mu.Lock()
	vector := p.assemble()
	p.recordContention()
	match := p.matchAlgorithm
	p.mu.Unlock()
	p.refreshCatalog()
	data, err := p.postRE("/api/perceive", map[string]any{
		"vector": vector, "matchAlgorithm": match, "includeMachineResults": true, "includePerceptualSpace": true,
	})
	if err != nil {
		return p.failure(err)
	}
	var step map[string]any
	if err := json.Unmarshal(data, &step); err != nil {
		return p.failure(err)
	}
	timestamp := nowMS()
	p.mu.Lock()
	p.osreFold = osreFoldCells(step, p.catalog)
	if space, ok := step["perceptualSpace"].([]any); ok {
		results, _ := step["machineResults"].(map[string]any)
		p.adoptSpace(aggregate(numbers(space), results))
	}
	p.advance()
	// A completed push is the PE's commit (#600).
	if lamport, commit, err := p.clock.CommitStep(); err == nil {
		p.lastPushClock = [2]uint64{lamport, commit}
	}
	p.lastPush = step
	p.recordPerceptionEvents()
	globalStep := p.globalStep
	// On the whole step, before the caller's selector narrows the reply.
	dispatch := p.dispatchTriggers(step)
	p.mu.Unlock()
	reply := map[string]any{}
	for k, v := range step {
		reply[k] = v
	}
	selectStep(reply, only)
	if !includeMachineResults {
		delete(reply, "machineResults")
	}
	return map[string]any{"success": true, "step": reply, "timestamp": timestamp, "globalStep": globalStep, "error": nil, "dispatch": dispatch}
}

func (p *PE) execute(includeMachineResults bool, only any) map[string]any {
	result := p.transition(includeMachineResults, only)
	if result["success"] == true {
		p.BroadcastState()
	}
	p.broadcastPushResult(result)
	return result
}

// Push is POST /api/push: the status and the answer.
func (p *PE) Push(includeMachineResults, async bool, only any) (int, map[string]any) {
	if !p.pushes.claim() {
		answer := p.failure(fmt.Errorf("push already in progress"))
		answer["coalesced"] = true
		return http.StatusConflict, answer
	}
	id := "push-" + instance.NewV7()
	p.pushes.record(id, "queued", nil)
	run := func() map[string]any {
		p.pushes.record(id, "running", nil)
		result := p.execute(includeMachineResults, only)
		p.pushes.record(id, "completed", result)
		if !p.pushes.releaseUnlessOwed() {
			go func() {
				p.execute(false, nil)
				p.pushes.release()
			}()
		}
		return result
	}
	if async {
		go run()
		return http.StatusAccepted, map[string]any{"success": true, "accepted": true, "jobId": id, "statusEndpoint": "/api/push/" + id}
	}
	return http.StatusOK, run()
}

// Job is GET /api/push/:id.
func (p *PE) Job(id string) (map[string]any, error) {
	p.pushes.mu.Lock()
	defer p.pushes.mu.Unlock()
	j, ok := p.pushes.jobs[id]
	if !ok {
		return nil, &Error{404, "Push job not found"}
	}
	out := map[string]any{"id": id, "status": j.status, "createdAt": j.createdAt, "updatedAt": j.updated}
	if j.result != nil {
		out["result"] = j.result
	}
	return out, nil
}

// AutoStart is POST /api/auto/start: a push every intervalMs (default and
// floor 1000 when not positive) under a new generation.
func (p *PE) AutoStart(body map[string]any) map[string]any {
	interval := int(num(body["intervalMs"], 1000))
	if interval <= 0 {
		interval = 1000
	}
	p.mu.Lock()
	p.autoRunning, p.autoIntervalMS = true, interval
	p.mu.Unlock()
	p.pushes.mu.Lock()
	p.pushes.autoGen++
	generation := p.pushes.autoGen
	p.pushes.mu.Unlock()
	go func() {
		for {
			time.Sleep(time.Duration(interval) * time.Millisecond)
			p.pushes.mu.Lock()
			current := p.pushes.autoGen
			p.pushes.mu.Unlock()
			if current != generation {
				return
			}
			p.Push(true, false, nil)
		}
	}()
	return map[string]any{"success": true, "intervalMs": interval}
}

// AutoStop is POST /api/auto/stop.
func (p *PE) AutoStop() map[string]any {
	p.mu.Lock()
	p.autoRunning = false
	p.mu.Unlock()
	p.pushes.mu.Lock()
	p.pushes.autoGen++
	p.pushes.mu.Unlock()
	return map[string]any{"success": true}
}

// encode is a message as the engines write JSON: no HTML escaping, no
// trailing newline.
func encode(v any) string {
	var buf bytes.Buffer
	e := json.NewEncoder(&buf)
	e.SetEscapeHTML(false)
	if e.Encode(v) != nil {
		return ""
	}
	return strings.TrimSuffix(buf.String(), "\n")
}

// BroadcastState publishes {type: state-update, state} when anyone listens.
func (p *PE) BroadcastState() {
	if p.hub.Listened() {
		p.hub.Publish(encode(map[string]any{"type": "state-update", "state": p.State()}))
	}
}

// StateUpdate is the state-update message a WebSocket client is sent as it
// connects.
func (p *PE) StateUpdate() string {
	return encode(map[string]any{"type": "state-update", "state": p.State()})
}

func (p *PE) broadcastPushResult(result map[string]any) {
	if !p.hub.Listened() {
		return
	}
	msg := map[string]any{"type": "push-result"}
	for k, v := range result {
		msg[k] = v
	}
	p.hub.Publish(encode(msg))
}

// Hub is the PE's event stream hub (/api/events, /ws).
func (p *PE) Hub() *stream.Hub { return p.hub }
