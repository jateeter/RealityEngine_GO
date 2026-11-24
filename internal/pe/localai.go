package pe

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// localAI (RealityEngine_GO#3, slice D): GET .../catalog, POST .../bootstrap,
// POST .../invoke and GET .../ledger under /api/integrations/localai.
// SURFACE_SPEC "localAI invoke contract" (settled 2026-09-23, 3-of-3) is the
// contract; C++ localai_catalog, bootstrap_localai, invoke_localai and
// localai_ledger are the concrete form, Rust's pe/localai.rs prior art.
//
//   - The allow-list is configuration: allowedOperations on the localai
//     integration of INTEGRATIONS_CONFIG, verbatim. None configured allows
//     nothing.
//   - Invoke matches (method, path) exactly, the query string ignored. A
//     refusal is 403, a missing target 400, a provider failure 502, and every
//     attempt is recorded in the ledger under a correlation id. A record
//     summarises the response (evidence), never stores it.
//   - Bootstrap registers the three bridge sensors (inactive until a value
//     arrives) and loads localAIStack's five bridge machines into this PE's
//     RE, interning each as a test source; one already held, by name, is
//     skipped.

const localAILedgerSize = 256

var localAIMachineFiles = []string{
	"rag_corrective_cycle.json", "session_rag_context.json", "session_agent_context.json",
	"ai_load_bridge.json", "agent_activity_classifier.json",
}

type localAILedger struct {
	mu      sync.Mutex
	records []any
}

func (l *localAILedger) add(r map[string]any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.records = append(l.records, r)
	if len(l.records) > localAILedgerSize {
		l.records = l.records[len(l.records)-localAILedgerSize:]
	}
}

func (p *PE) fetchOrError(url string) any {
	v, err := p.fetchJSON(url, nil)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	return v
}

// LocalAICatalog is GET /api/integrations/localai/catalog.
func (p *PE) LocalAICatalog() map[string]any {
	in := p.integrations
	base := in.localAIBaseURL
	status := p.LocalAIStatus()
	graphSchema := p.fetchOrError(base + "/graph/schema")
	events := p.fetchOrError(base + "/graphql/events")
	allowed, source := in.localAIAllowedOperations()
	p.BroadcastState()
	sensors := make([]any, len(localAISensors))
	for i, s := range localAISensors {
		sensors[i] = s.id
	}
	return map[string]any{
		"success": true, "status": status, "graphSchema": graphSchema, "recentGraphQLEvents": events,
		"invokeEndpoint": "/api/integrations/localai/invoke", "allowedEndpoints": allowed,
		"allowedEndpointsSource": optional(source),
		"realityBridge": map[string]any{
			"sensors": sensors, "bootstrapEndpoint": "/api/integrations/localai/bootstrap", "signalEndpoint": "/api/signals",
		},
	}
}

// LocalAILedger is GET /api/integrations/localai/ledger.
func (p *PE) LocalAILedger() map[string]any {
	p.localAI.mu.Lock()
	records := append([]any{}, p.localAI.records...)
	p.localAI.mu.Unlock()
	return map[string]any{"provider": "localai", "endpoint": p.integrations.localAIBaseURL, "records": records}
}

func (p *PE) do(method, url string, body []byte) ([]byte, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, url, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, url, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("%s %s: HTTP %s: %s", method, url, resp.Status, data)
	}
	return data, nil
}

// LocalAIInvoke is POST /api/integrations/localai/invoke.
func (p *PE) LocalAIInvoke(body map[string]any) (int, map[string]any) {
	in := p.integrations
	target, ok := firstString(body, "endpoint", "path")
	if !ok {
		return http.StatusBadRequest, map[string]any{"success": false, "error": "localAI invocation requires endpoint or path"}
	}
	endpoint := target
	if !strings.HasPrefix(endpoint, "/") {
		endpoint = "/" + endpoint
	}
	routePath, _, _ := strings.Cut(endpoint, "?")
	ops, _ := in.localAIAllowedOperations()
	method := str(body["method"], "")
	if method == "" {
		for _, ov := range ops {
			op, _ := ov.(map[string]any)
			if op["path"] == routePath {
				method = str(op["method"], "")
				break
			}
		}
	}
	if method == "" {
		method = "POST"
	}
	method = strings.ToUpper(method)
	correlation := str(body["correlationId"], "")
	if _, ok := body["correlationId"].(string); !ok {
		correlation = mint("localai-invocation")
	}
	invocation, started := mint("localai-inv"), nowMS()
	operationID := ""
	for _, ov := range ops {
		op, _ := ov.(map[string]any)
		if strings.ToUpper(str(op["method"], "")) == method && op["path"] == routePath {
			operationID = str(op["id"], "")
			break
		}
	}
	record := func(success bool, operation string, response any, errText string) {
		out := map[string]any{
			"id": invocation, "correlationId": correlation, "provider": "localai", "endpoint": endpoint,
			"method": method, "startedAt": started, "completedAt": nowMS(), "success": success,
		}
		if operation != "" {
			out["operationId"] = operation
		}
		for _, k := range []string{"machineName", "sequenceId", "requestClass", "resultClass"} {
			if v, _ := body[k].(string); v != "" {
				out[k] = v
			}
		}
		if errText != "" {
			out["error"] = errText
		}
		if response != nil {
			shape := "scalar"
			switch response.(type) {
			case map[string]any:
				shape = "object"
			case []any:
				shape = "array"
			}
			out["evidence"] = map[string]any{"uri": in.localAIBaseURL + endpoint, "shape": shape}
			if m, ok := response.(map[string]any); ok {
				if c, ok := m["confidence"].(float64); ok {
					out["confidence"] = c
				}
			}
		}
		p.localAI.add(out)
	}
	refusal := func(status int, errText string) (int, map[string]any) {
		return status, map[string]any{
			"success": false, "endpoint": endpoint, "method": method,
			"correlationId": correlation, "invocationId": invocation, "error": errText,
		}
	}
	if strings.Contains(routePath, "..") || strings.Contains(routePath, "//") || operationID == "" {
		record(false, "", nil, "endpoint is not allowed")
		return refusal(http.StatusForbidden, "localAI endpoint is not allowed")
	}
	url := in.localAIBaseURL + endpoint
	var payload []byte
	if method != "GET" {
		v := body["payload"]
		if v == nil {
			v = body["body"]
		}
		if v == nil {
			v = map[string]any{}
		}
		payload, _ = json.Marshal(v)
	}
	data, err := p.do(method, url, payload)
	var response any
	if err == nil {
		if jerr := json.Unmarshal(data, &response); jerr != nil {
			err = fmt.Errorf("%s %s: %v", method, url, jerr)
		}
	}
	if err != nil {
		record(false, operationID, nil, err.Error())
		return refusal(http.StatusBadGateway, err.Error())
	}
	record(true, operationID, response, "")
	return http.StatusOK, map[string]any{
		"success": true, "endpoint": endpoint, "method": method,
		"correlationId": correlation, "invocationId": invocation, "response": response,
	}
}

// LocalAIBootstrap is POST /api/integrations/localai/bootstrap.
func (p *PE) LocalAIBootstrap() map[string]any {
	in := p.integrations
	registered, skippedSensors := []any{}, []any{}
	p.mu.Lock()
	now := nowMS()
	for _, s := range localAISensors {
		if p.sensorByID(s.id) != nil {
			skippedSensors = append(skippedSensors, s.id)
			continue
		}
		added := p.addSource(&Source{Name: s.name, Region: Region{s.offset, s.length}, sensor: &sensorDetail{
			sensorID: s.id, lastValue: []float64{}, ttlMS: 30000, origin: "localai",
		}})
		registered = append(registered, added.json(now))
	}
	p.mu.Unlock()
	names := map[string]bool{}
	if data, err := p.getRE("/api/machines"); err == nil {
		var listing map[string]any
		if json.Unmarshal(data, &listing) == nil {
			machines, _ := listing["machines"].([]any)
			for _, mv := range machines {
				if m, ok := mv.(map[string]any); ok {
					if n, ok := m["name"].(string); ok {
						names[n] = true
					}
				}
			}
		}
	}
	imported, skippedMachines, failed := []any{}, []any{}, []any{}
	for _, file := range localAIMachineFiles {
		raw, err := os.ReadFile(filepath.Join(in.localAIMachinesDir, file))
		if err != nil {
			failed = append(failed, map[string]any{"file": file, "error": "not found"})
			continue
		}
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			failed = append(failed, map[string]any{"file": file, "error": err.Error()})
			continue
		}
		m, ok := doc["machine"].(map[string]any)
		if !ok {
			m = doc
		}
		name := str(m["name"], "unnamed")
		if names[name] {
			skippedMachines = append(skippedMachines, name)
			continue
		}
		data, err := p.do(http.MethodPost, p.reURL+"/api/machines", raw)
		var answer map[string]any
		if err == nil {
			err = json.Unmarshal(data, &answer)
		}
		if err != nil {
			failed = append(failed, map[string]any{"file": file, "error": err.Error()})
			continue
		}
		machine, ok := answer["machine"].(map[string]any)
		if !ok {
			machine = m
		}
		p.mu.Lock()
		p.intern(machine)
		p.mu.Unlock()
		imported = append(imported, machine)
		names[name] = true
	}
	p.BroadcastState()
	return map[string]any{
		"success": true, "registeredSensors": registered, "skippedSensors": skippedSensors,
		"importedMachines": imported, "skippedMachines": skippedMachines, "failedMachines": failed,
		"localAIBaseUrl": in.localAIBaseURL, "machineDirectory": in.localAIMachinesDir,
	}
}
