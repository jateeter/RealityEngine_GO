package pe

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/jateeter/RealityEngine_GO/internal/instance"
)

// Dispatch & Triggers (RealityEngine_GO#3, slice A): trigger dispatch on
// every push, the dispatch ledger and records, replay, and GET
// /api/triggers/status. SURFACE_SPEC "Dispatch surface shapes" and "Dispatch
// replay" are the contract; C++ dispatch_triggers and build_trigger_envelope
// the concrete form, Rust's pe/dispatch.rs prior art.
//
//   - Trigger dispatch runs on each push while TRIGGERS_ENABLED is on. A
//     mergeBatch entry with governance, whose machine the catalog holds and
//     binds to an agent and a trigger, becomes a ces.terminal.event envelope
//     and a dispatch record (status "recorded", fire-and-record: no provider
//     is called here). Entries without governance, without a binding, or met
//     before the catalog ever loaded are counted apart.
//   - A record's semantics links it to the corpus ABox (abox-manifest.json).
//   - The ledger is a ring of TRIGGER_DISPATCH_LEDGER_LIMIT records (default
//     256), oldest first. PATCH changes delivery state only; replay re-emits a
//     record's envelope as a new record and calls nothing.

type dispatchState struct {
	enabled          bool
	mode             string
	graphqlEndpoint  string
	capacity         int
	records          map[string]map[string]any
	order            []string
	envelopesCreated int
	replaysCreated   int
	droppedNoGov     int
	droppedNoDisp    int
	droppedCold      int
	errors           int
	bases            map[string]string // machine name -> ABox base IRI; nil until read
}

func truthy(v string) bool {
	switch v {
	case "1", "true", "TRUE", "yes", "YES":
		return true
	}
	return false
}

func newDispatch(localAIBase string) dispatchState {
	d := dispatchState{
		enabled: truthy(os.Getenv("TRIGGERS_ENABLED")), mode: "dry-run",
		graphqlEndpoint: localAIBase + "/graphql", capacity: 256, records: map[string]map[string]any{},
	}
	if m := os.Getenv("TRIGGER_DISPATCH_MODE"); m != "" {
		d.mode = m
	}
	if u, ok := os.LookupEnv("TRIGGER_GRAPHQL_URL"); ok {
		d.graphqlEndpoint = u
	}
	if n, err := strconv.Atoi(os.Getenv("TRIGGER_DISPATCH_LEDGER_LIMIT")); err == nil && n > 0 {
		d.capacity = n
	}
	return d
}

func mint(prefix string) string { return prefix + "-" + instance.NewV7() }

func (d *dispatchState) store(record map[string]any) {
	id, _ := record["id"].(string)
	d.order = append(d.order, id)
	d.records[id] = record
	for len(d.order) > d.capacity {
		delete(d.records, d.order[0])
		d.order = d.order[1:]
	}
}

// manifestBases reads the corpus ABox manifest: machine name -> base IRI.
func manifestBases() map[string]string {
	path := os.Getenv("SEMANTICS_MANIFEST")
	if path == "" {
		dir := os.Getenv("MACHINES_DIR")
		if dir == "" {
			dir = "../RealityEngine_Machines/machines"
		}
		if abs, err := filepath.Abs(dir); err == nil {
			for i := 0; i < 6; i++ {
				candidate := filepath.Join(abs, "semantics", "abox-manifest.json")
				if _, err := os.Stat(candidate); err == nil {
					path = candidate
					break
				}
				abs = filepath.Dir(abs)
			}
		}
	}
	out := map[string]string{}
	data, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	var doc map[string]any
	if json.Unmarshal(data, &doc) != nil {
		return out
	}
	machines, _ := doc["machines"].(map[string]any)
	for _, ev := range machines {
		e, _ := ev.(map[string]any)
		name, _ := e["name"].(string)
		iri, _ := e["iri"].(string)
		if base, _, ok := strings.Cut(iri, "#"); ok && name != "" {
			out[name] = base
		}
	}
	return out
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func stringsOf(v any) []any {
	out := []any{}
	l, _ := v.([]any)
	for _, x := range l {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func (d *dispatchState) semantics(machineName string, gov map[string]any, sequenceIDs []any) map[string]any {
	if d.bases == nil {
		d.bases = manifestBases()
	}
	base := d.bases[machineName]
	sequence := str(gov["sequenceId"], "")
	if sequence == "" && len(sequenceIDs) == 1 {
		sequence, _ = sequenceIDs[0].(string)
	}
	local := strings.Map(func(r rune) rune {
		if r < 128 && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return r
		}
		return '_'
	}, sequence)
	out := map[string]any{"machineIri": nil, "sequenceIri": nil, "actionCode": nullable(str(gov["actionCode"], ""))}
	if base != "" {
		out["machineIri"] = base + "#machine"
		if sequence != "" {
			if local == "" {
				local = "unnamed"
			}
			out["sequenceIri"] = base + "#seq-" + local
		}
	}
	return out
}

type binding struct {
	agent, trigger, action, autonomyMode string
	actions                              []any
	writeBack                            any
}

func bindingOf(metadata map[string]any, values any) binding {
	var b binding
	if ab, ok := metadata["agentBinding"].(map[string]any); ok {
		b.agent = str(ab["agent"], str(metadata["dispatchableAgent"], ""))
		b.trigger = str(ab["trigger"], str(metadata["aiTrigger"], ""))
		b.autonomyMode = str(ab["mode"], "")
		b.actions = stringsOf(ab["allowedActions"])
		b.writeBack = ab["writeBack"]
	} else {
		b.agent, b.trigger, b.actions = str(metadata["dispatchableAgent"], ""), str(metadata["aiTrigger"], ""), []any{}
	}
	if len(b.actions) == 0 {
		b.actions = stringsOf(metadata["agentActions"])
	}
	asserted := -1
	vs, _ := values.([]any)
	for i, v := range vs {
		if num(v, 0) != 0 {
			asserted = i
			break
		}
	}
	if asserted >= 0 && asserted < len(b.actions) {
		b.action, _ = b.actions[asserted].(string)
	} else if len(b.actions) > 0 {
		b.action, _ = b.actions[0].(string)
	}
	return b
}

func assertedLabel(values any) string {
	vs, ok := values.([]any)
	if !ok {
		return ""
	}
	var labels []string
	for i, v := range vs {
		if num(v, 0) != 0 {
			labels = append(labels, fmt.Sprintf("cell_%d", i))
		}
	}
	if len(labels) == 0 {
		return "none"
	}
	return strings.Join(labels, "+")
}

func arrayOr(v any) any {
	if l, ok := v.([]any); ok {
		return l
	}
	return []any{}
}

// envelope is C++ build_trigger_envelope.
func (p *PE) envelope(op, machine map[string]any, envelopeID, correlationID string) map[string]any {
	metadata, _ := machine["metadata"].(map[string]any)
	trigger, _ := metadata["triggerConfig"].(map[string]any)
	triggerDispatch, _ := trigger["dispatch"].(map[string]any)
	b := bindingOf(metadata, op["values"])
	graphql := p.dispatch.mode == "graphql"
	orEmpty := func(v string) string {
		if graphql {
			return v
		}
		return ""
	}
	values := arrayOr(op["values"])
	semantics := []any{}
	for i := range values.([]any) {
		semantics = append(semantics, map[string]any{"index": i, "label": fmt.Sprintf("cell_%d", i)})
	}
	machineID := str(op["machineId"], "")
	var governance any
	if g, ok := op["governance"].(map[string]any); ok {
		governance = g
	}
	return map[string]any{
		"schemaVersion": "1.0.0", "envelopeType": "ces.terminal.event", "envelopeId": envelopeID,
		"correlationId": correlationID, "emittedAtMs": nowMS(),
		"source": map[string]any{"engine": "PE", "observedEngine": "RE", "endpoint": p.reURL},
		"ces": map[string]any{
			"machineId": machineID, "machineName": str(machine["name"], machineID),
			"machineCode": str(metadata["machineCode"], ""), "sequenceIds": arrayOr(op["sequenceIds"]),
			"stepNumber": 0, "perceptualMapping": map[string]any{"output": op["region"]},
			"provenance": arrayOr(op["provenance"]), "deprecation": op["deprecation"],
		},
		"outputVector": map[string]any{
			"values": values, "encoding": "vector", "semantics": semantics, "assertedLabel": assertedLabel(op["values"]),
		},
		"projection": nil, "governance": governance,
		"dispatch": map[string]any{
			"processId": str(trigger["processId"], ""), "processName": str(trigger["processName"], ""),
			"agent": b.agent, "action": b.action, "agentActionsCatalog": b.actions, "trigger": b.trigger,
			"autonomyMode": b.autonomyMode, "writeBack": b.writeBack,
			"endpoint": map[string]any{
				"kind":      p.dispatch.mode,
				"url":       orEmpty(str(trigger["endpoint"], p.dispatch.graphqlEndpoint)),
				"mutation":  orEmpty(str(triggerDispatch["mutation"], "updateProcessState")),
				"schemaRef": orEmpty(str(triggerDispatch["schemaRef"], "localAIStack/services/api/routers/graphql_endpoint.py")),
			},
		},
	}
}

// dispatchTriggers runs trigger dispatch over a push's step and answers the
// push reply's dispatch summary. The caller holds the write lock.
func (p *PE) dispatchTriggers(step map[string]any) map[string]any {
	mergeOps, created, noGov, noDisp, cold := 0, 0, 0, 0, 0
	if p.dispatch.enabled {
		catalogCold := !p.catalogLoaded
		batch, _ := step["mergeBatch"].([]any)
		for _, ov := range batch {
			mergeOps++
			op, _ := ov.(map[string]any)
			gov, ok := op["governance"].(map[string]any)
			if !ok {
				noGov++
				continue
			}
			machineID := str(op["machineId"], "")
			machine, ok := p.catalog[machineID]
			if !ok {
				if catalogCold {
					cold++
				} else {
					noDisp++
				}
				continue
			}
			metadata, _ := machine["metadata"].(map[string]any)
			b := bindingOf(metadata, op["values"])
			if b.agent == "" || b.trigger == "" {
				noDisp++
				continue
			}
			envelopeID, correlationID := mint("trigger-envelope"), mint("trigger-correlation")
			sequenceIDs := stringsOf(op["sequenceIds"])
			now := nowMS()
			record := map[string]any{
				"id": mint("dispatch"), "envelopeId": envelopeID, "correlationId": correlationID,
				"status": "recorded", "mode": p.dispatch.mode, "target": b.agent, "machineId": machineID,
				"sequenceIds": sequenceIDs, "ragStatusCode": nullable(str(gov["ragStatusCode"], "")),
				"processStatus": nullable(str(gov["processStatus"], "")), "attempts": 0,
				"createdAt": now, "updatedAt": now, "providerReceipt": nil,
				"envelope": p.envelope(op, machine, envelopeID, correlationID), "error": nil,
				"semantics": p.dispatch.semantics(str(machine["name"], ""), gov, sequenceIDs), "replayOf": nil,
			}
			p.dispatch.envelopesCreated++
			created++
			p.broadcast(map[string]any{
				"type": "trigger.envelope.created", "envelopeId": envelopeID, "correlationId": correlationID,
				"dispatchId": record["id"], "target": b.agent, "mode": p.dispatch.mode,
			})
			p.dispatch.store(record)
		}
	}
	p.dispatch.droppedNoGov += noGov
	p.dispatch.droppedNoDisp += noDisp
	p.dispatch.droppedCold += cold
	return map[string]any{
		"enabled": p.dispatch.enabled, "mode": p.dispatch.mode, "mergeOps": mergeOps,
		"envelopesCreated": created, "dispatchRecordsCreated": created,
		"droppedNoGovernance": noGov, "droppedNoDispatch": noDisp, "errors": 0,
	}
}

// TriggerStatus is GET /api/triggers/status.
func (p *PE) TriggerStatus() map[string]any {
	p.mu.RLock()
	defer p.mu.RUnlock()
	d := p.dispatch
	participation := "not-active"
	if d.enabled {
		participation = "active"
	}
	return map[string]any{
		"participation": participation, "enabled": d.enabled, "mode": d.mode, "graphqlEndpoint": d.graphqlEndpoint,
		"records": len(d.records), "envelopesCreated": d.envelopesCreated, "droppedNoGovernance": d.droppedNoGov,
		"droppedNoDispatch": d.droppedNoDisp, "droppedCatalogCold": d.droppedCold, "replaysCreated": d.replaysCreated,
		"dispatchErrors": 0, "machineCatalogCold": !p.catalogLoaded, "machineCatalogRefreshedAt": p.catalogRefreshedAt,
		"machineCatalogSize": len(p.catalog),
	}
}

// Ledger is GET /api/dispatch/ledger: oldest first.
func (p *PE) Ledger() map[string]any {
	p.mu.RLock()
	defer p.mu.RUnlock()
	records := []any{}
	for _, id := range p.dispatch.order {
		if r, ok := p.dispatch.records[id]; ok {
			records = append(records, r)
		}
	}
	return map[string]any{"enabled": p.dispatch.enabled, "mode": p.dispatch.mode, "records": records}
}

// Record is GET /api/dispatch/records/:id.
func (p *PE) Record(id string) (map[string]any, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	r, ok := p.dispatch.records[id]
	if !ok {
		return nil, &Error{404, "Dispatch record not found"}
	}
	return map[string]any{"record": r}, nil
}

// UpdateRecord is PATCH /api/dispatch/records/:id: delivery state only.
func (p *PE) UpdateRecord(id string, body map[string]any) (map[string]any, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	r, ok := p.dispatch.records[id]
	if !ok {
		return nil, &Error{404, "Dispatch record not found"}
	}
	if s, ok := body["status"].(string); ok {
		r["status"] = s
	}
	if s, ok := body["error"].(string); ok {
		r["error"] = nullable(s)
	}
	if b, _ := body["clearError"].(bool); b {
		r["error"] = nil
	}
	if f, ok := body["attempts"].(float64); ok {
		r["attempts"] = int64(f)
	} else if b, _ := body["incrementAttempts"].(bool); b {
		r["attempts"] = attempts(r["attempts"]) + 1
	}
	receipt := map[string]any{}
	if old, ok := r["providerReceipt"].(map[string]any); ok {
		for k, v := range old {
			receipt[k] = v
		}
	}
	if pr, ok := body["providerReceipt"].(map[string]any); ok {
		for k, v := range pr {
			receipt[k] = v
		}
	}
	for _, key := range []string{"provider", "adapter", "externalRunId"} {
		if s, ok := body[key].(string); ok {
			receipt[key] = s
		}
	}
	if len(receipt) > 0 {
		r["providerReceipt"] = receipt
	}
	r["updatedAt"] = nowMS()
	p.broadcast(map[string]any{
		"type": "dispatch.record.updated", "dispatchId": r["id"], "status": r["status"],
		"target": r["target"], "attempts": r["attempts"], "timestamp": r["updatedAt"],
	})
	return map[string]any{"success": true, "record": r}, nil
}

// attempts reads a record's attempt count, however it was stored.
func attempts(v any) int64 {
	switch n := v.(type) {
	case int:
		return int64(n)
	case int64:
		return n
	case float64:
		return int64(n)
	}
	return 0
}

// Replay is POST /api/dispatch/records/:id/replay: the record re-emitted as a
// new one; freshIds mints new envelope and correlation ids.
func (p *PE) Replay(id string, freshIDs bool) (map[string]any, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	orig, ok := p.dispatch.records[id]
	if !ok {
		return nil, &Error{404, "Dispatch record not found"}
	}
	r := deepCopy(orig).(map[string]any)
	now := nowMS()
	r["id"], r["status"], r["mode"], r["attempts"] = mint("dispatch"), "recorded", "replay", 0
	r["providerReceipt"], r["error"], r["replayOf"], r["createdAt"], r["updatedAt"] = nil, nil, id, now, now
	if freshIDs {
		envelopeID, correlationID := mint("trigger-envelope"), mint("trigger-correlation")
		r["envelopeId"], r["correlationId"] = envelopeID, correlationID
		if env, ok := r["envelope"].(map[string]any); ok {
			env["envelopeId"], env["correlationId"], env["emittedAtMs"] = envelopeID, correlationID, now
		}
	}
	p.dispatch.envelopesCreated++
	p.dispatch.replaysCreated++
	p.dispatch.store(r)
	p.broadcast(map[string]any{
		"type": "trigger.envelope.created", "envelopeId": r["envelopeId"], "correlationId": r["correlationId"],
		"dispatchId": r["id"], "target": r["target"], "mode": "replay", "replayOf": id,
	})
	return map[string]any{"success": true, "record": r, "replayOf": id, "freshIds": freshIDs}, nil
}

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
	}
	return v
}

// broadcast publishes an integration's own message when anyone listens.
func (p *PE) broadcast(msg map[string]any) {
	if p.hub.Listened() {
		p.hub.Publish(encode(msg))
	}
}
