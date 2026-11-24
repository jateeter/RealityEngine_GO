package pe

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
)

// The dispatch adapters (RealityEngine_GO#3, slice C): POST
// /api/integrations/ollama/dispatch, /openai/dispatch and /acp/dispatch. C++
// dispatch_ollama, dispatch_openai and dispatch_acp are the concrete form,
// Rust's pe/adapters.rs prior art. Each takes a dispatch record (dispatchId,
// else id) and delivers its trigger envelope:
//
//   - Ollama: POST {base}/api/chat. The reply's content is parsed as JSON
//     and, with a completion source mapping and commitCompletion (default on),
//     its values are committed through the completion ingest. format is sent,
//     or the completion contract put in the system prompt, by model family.
//   - OpenAI: POST {base}/responses, with the bearer key when one is set,
//     constrained by a JSON schema built from the mapping, or for a model that
//     cannot take one with the contract in its instructions; then the same
//     commit. The key is optional (owner ruling, 2026-11-01).
//   - ACP: no call at all. The PE records an OpenClaw handoff receipt and
//     answers 202; the harness commits its own completion later.
//
// The record moves delivering -> delivered, or failed with the error, and a
// failure answers 502.

const (
	ollamaSystem       = "You are a local PE-controlled Ollama adapter. Analyze the trigger envelope and respond as JSON. When committing a PE completion, include a numeric values array matching the configured source mapping."
	openaiInstructions = "You are a PE-controlled OpenAI adapter. Analyze the trigger envelope and respond as JSON. When committing a PE completion, include a numeric values array matching the configured source mapping."
	reviewPrompt       = "Review this RealityEngine trigger envelope and produce a concise JSON result for PE completion ingest."
	acpPrompt          = "Handle this RealityEngine trigger envelope through the configured OpenClaw ACP session and return a PE completion values array."
)

// noStructuredOutput are the model families that cannot take format.
var noStructuredOutput = []string{"nemotron-3-nano"}

func flag(m map[string]any, key string, def bool) bool {
	if b, ok := m[key].(bool); ok {
		return b
	}
	return def
}

func parseOrNull(raw string) any {
	var v any
	if json.Unmarshal([]byte(raw), &v) != nil {
		return nil
	}
	return v
}

// pointer resolves a JSON pointer (RFC 6901).
func pointer(doc any, p string) (any, bool) {
	if p == "" {
		return doc, true
	}
	if !strings.HasPrefix(p, "/") {
		return nil, false
	}
	cursor := doc
	for _, raw := range strings.Split(p[1:], "/") {
		token := strings.ReplaceAll(strings.ReplaceAll(raw, "~1", "/"), "~0", "~")
		switch c := cursor.(type) {
		case map[string]any:
			v, ok := c[token]
			if !ok {
				return nil, false
			}
			cursor = v
		case []any:
			if token == "" || strings.TrimLeft(token, "0123456789") != "" {
				return nil, false
			}
			i, err := strconv.Atoi(token)
			if err != nil || i >= len(c) {
				return nil, false
			}
			cursor = c[i]
		default:
			return nil, false
		}
	}
	return cursor, true
}

func topLevelKey(p string) string {
	if !strings.HasPrefix(p, "/") {
		return "value"
	}
	token, _, _ := strings.Cut(p[1:], "/")
	return strings.ReplaceAll(strings.ReplaceAll(token, "~1", "/"), "~0", "~")
}

func completionNumber(v any) (float64, bool) {
	var n float64
	switch t := v.(type) {
	case float64:
		n = t
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	case string:
		raw := strings.TrimLeft(t, " \t\n\r")
		if raw == "" {
			return 0, false
		}
		f, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return 0, false
		}
		n = f
	default:
		return 0, false
	}
	return n, !math.IsNaN(n) && !math.IsInf(n, 0)
}

func validateValues(v any) ([]any, error) {
	items, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("provider completion values must be an array")
	}
	out := make([]any, len(items))
	for i, x := range items {
		n, ok := completionNumber(x)
		if !ok {
			return nil, fmt.Errorf("provider completion value is not a finite number")
		}
		out[i] = n
	}
	return out, nil
}

var errNoValues = fmt.Errorf("provider response did not include completion values")

func responseValues(content any) ([]any, error) {
	c, _ := content.(map[string]any)
	if _, ok := c["values"].([]any); ok {
		return validateValues(c["values"])
	}
	if comp, ok := c["completion"].(map[string]any); ok {
		if _, ok := comp["values"].([]any); ok {
			return validateValues(comp["values"])
		}
	}
	return nil, errNoValues
}

func pointedNumber(content any, at string) (float64, error) {
	v, ok := pointer(content, at)
	if !ok {
		return 0, fmt.Errorf("missing required JSON pointer: %s", at)
	}
	n, ok := completionNumber(v)
	if !ok {
		return 0, fmt.Errorf("JSON pointer resolved to a non-finite value: %s", at)
	}
	return n, nil
}

func extractValues(content any, mapping map[string]any) ([]any, error) {
	extract, ok := mapping["extract"].(map[string]any)
	if !ok || extract["type"] != "json" {
		return responseValues(content)
	}
	if pointers, ok := extract["pointers"].([]any); ok {
		out := make([]any, len(pointers))
		for i, pv := range pointers {
			n, err := pointedNumber(content, str(pv, ""))
			if err != nil {
				return nil, err
			}
			out[i] = n
		}
		return out, nil
	}
	if at := str(extract["pointer"], ""); at != "" {
		v, ok := pointer(content, at)
		if !ok {
			return nil, fmt.Errorf("missing required JSON pointer: %s", at)
		}
		if _, isArray := v.([]any); isArray {
			return validateValues(v)
		}
		n, err := pointedNumber(content, at)
		if err != nil {
			return nil, err
		}
		return []any{n}, nil
	}
	return responseValues(content)
}

func normalize(values []any, mapping map[string]any) []any {
	norm, ok := mapping["normalize"].(map[string]any)
	if !ok {
		return values
	}
	mode, clamp := str(norm["mode"], "passthrough"), flag(norm, "clamp", false)
	out := make([]any, len(values))
	for i, v := range values {
		n, _ := v.(float64)
		switch mode {
		case "minmax":
			lo, hi := num(norm["min"], 0), num(norm["max"], 0)
			if span := hi - lo; span == 0 {
				n = 0
			} else {
				n = (n - lo) / span
			}
		case "linear":
			n = n*num(norm["scale"], 1) + num(norm["offset"], 0)
		}
		if clamp {
			if math.IsNaN(n) || math.IsInf(n, 0) {
				n = 0
			} else {
				n = clamp01(n)
			}
		}
		out[i] = n
	}
	return out
}

func valuesFromContent(content any, mapping map[string]any) ([]any, error) {
	if content == nil {
		return nil, fmt.Errorf("provider response content is not valid JSON")
	}
	if mapping != nil {
		v, err := extractValues(content, mapping)
		if err != nil {
			return nil, err
		}
		return normalize(v, mapping), nil
	}
	return responseValues(content)
}

func extractKeys(mapping map[string]any) []string {
	extract, ok := mapping["extract"].(map[string]any)
	if mapping == nil || !ok || extract["type"] != "json" {
		return nil
	}
	var pointers []any
	if ps, ok := extract["pointers"].([]any); ok {
		pointers = ps
	} else if p, ok := extract["pointer"].(string); ok {
		pointers = []any{p}
	}
	keys := []string{}
	for _, pv := range pointers {
		key := topLevelKey(str(pv, ""))
		if key == "" {
			key = "value"
		}
		keys = append(keys, key)
	}
	return keys
}

func completionSchema(mapping map[string]any) map[string]any {
	keys := extractKeys(mapping)
	if len(keys) == 0 {
		return map[string]any{
			"type": "object", "additionalProperties": false,
			"properties": map[string]any{"values": map[string]any{"type": "array", "items": map[string]any{"type": "number"}}},
			"required":   []string{"values"},
		}
	}
	props := map[string]any{}
	for _, k := range keys {
		props[k] = map[string]any{"type": []string{"number", "boolean"}}
	}
	return map[string]any{"type": "object", "additionalProperties": false, "properties": props, "required": keys}
}

func contractPrompt(mapping map[string]any) string {
	var keys []string
	seen := map[string]bool{}
	for _, k := range extractKeys(mapping) {
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	lead := "\n\nRespond with ONLY one JSON object, with no prose and no markdown fences, that has exactly "
	if len(keys) == 0 {
		return lead + `this key, an array of numbers: values. For example: {"values": [0]}`
	}
	examples := make([]string, len(keys))
	for i, k := range keys {
		examples[i] = fmt.Sprintf(`"%s": 0`, k)
	}
	return lead + "these keys, each a number or boolean: " + strings.Join(keys, ", ") + ". For example: {" + strings.Join(examples, ", ") + "}"
}

func openaiTextFormat(mapping map[string]any) map[string]any {
	return map[string]any{"format": map[string]any{
		"type": "json_schema", "name": "reality_engine_completion", "strict": true, "schema": completionSchema(mapping),
	}}
}

func supportsStructuredOutput(model string) bool {
	family, _, _ := strings.Cut(model, ":")
	for _, f := range noStructuredOutput {
		if f == family {
			return false
		}
	}
	return true
}

// dispatchRequest is a resolved adapter request.
type dispatchRequest struct {
	body   map[string]any
	id     string
	record map[string]any
}

func (p *PE) resolveDispatch(body map[string]any, name string) (*dispatchRequest, *Error) {
	id := str(body["dispatchId"], str(body["id"], ""))
	if id == "" {
		return nil, &Error{400, name + " dispatch requires dispatchId"}
	}
	p.mu.RLock()
	r, ok := p.dispatch.records[id]
	var record map[string]any
	if ok {
		record = deepCopy(r).(map[string]any)
	}
	p.mu.RUnlock()
	if !ok {
		return nil, &Error{404, "Dispatch record not found"}
	}
	return &dispatchRequest{body, id, record}, nil
}

func (p *PE) completionMapping(id string) (map[string]any, *Error) {
	if id == "" {
		return nil, nil
	}
	m, ok := p.integrations.sourceMappings[id]
	if !ok {
		return nil, &Error{404, fmt.Sprintf("Unknown sourceMappingId %q", id)}
	}
	return m, nil
}

func (p *PE) mark(id string, patch map[string]any) {
	_, _ = p.UpdateRecord(id, patch)
}

func (p *PE) commit(req *dispatchRequest, provider, mappingID string, values []any, completionID string, metadata map[string]any) (map[string]any, error) {
	agent := str(req.record["target"], "")
	if agent == "" {
		agent = provider
	}
	status, reply := p.Completion(map[string]any{
		"provider": provider, "agent": agent, "sourceMappingId": mappingID,
		"correlationId": str(req.record["correlationId"], ""), "envelopeId": str(req.record["envelopeId"], ""),
		"completionId": completionID, "values": values, "metadata": metadata,
		"triggerPush": flag(req.body, "triggerPush", false),
	})
	if status != http.StatusOK {
		return nil, fmt.Errorf("completion ingest failed: %s", encode(reply))
	}
	return reply, nil
}

func valuesToCommit(body map[string]any, content any, mapping map[string]any) ([]any, error) {
	if _, ok := body["values"].([]any); ok {
		return validateValues(body["values"])
	}
	return valuesFromContent(content, mapping)
}

func (p *PE) adapterFailure(req *dispatchRequest, provider string, err error, receipt map[string]any, model string) (int, map[string]any) {
	p.mark(req.id, map[string]any{
		"status": "failed", "adapter": provider, "provider": provider, "error": err.Error(), "providerReceipt": receipt,
	})
	stored := merge(receipt, nil)
	delete(stored, "baseUrl")
	return http.StatusBadGateway, map[string]any{
		"success": false, "dispatchId": req.id, "provider": provider, "model": model, "error": err.Error(),
		"receipt": map[string]any{"provider": provider, "adapter": provider, "status": "failed", "error": err.Error(), "providerReceipt": stored},
	}
}

func (p *PE) postJSON(url string, payload any, headers map[string]string) (map[string]any, error) {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader([]byte(encode(payload))))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("POST %s: %w", url, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("POST %s: %w", url, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("POST %s: HTTP %s: %s", url, resp.Status, data)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func ollamaPayload(record, body map[string]any, model string, mapping map[string]any) map[string]any {
	if pl, ok := body["payload"].(map[string]any); ok {
		return pl
	}
	structured := supportsStructuredOutput(model)
	_, hasFormat := body["format"].(string)
	sendFormat := hasFormat || flag(body, "formatJson", structured)
	system := str(body["systemPrompt"], ollamaSystem)
	if !sendFormat {
		system += contractPrompt(mapping)
	}
	prompt := str(body["prompt"], reviewPrompt)
	payload := map[string]any{
		"model": model, "stream": false,
		"messages": []any{
			map[string]any{"role": "system", "content": system},
			map[string]any{"role": "user", "content": prompt + "\n\n" + encode(record["envelope"])},
		},
	}
	if f, ok := body["format"].(string); ok {
		payload["format"] = f
	} else if sendFormat {
		payload["format"] = "json"
	}
	if !structured {
		payload["think"] = false
	}
	if o, ok := body["options"].(map[string]any); ok {
		payload["options"] = o
	}
	return payload
}

// OllamaDispatch is POST /api/integrations/ollama/dispatch.
func (p *PE) OllamaDispatch(body map[string]any) (int, map[string]any) {
	req, e := p.resolveDispatch(body, "Ollama")
	if e != nil {
		return e.Status, map[string]any{"error": e.Message}
	}
	in := p.integrations
	model := str(body["model"], in.ollamaModel)
	mappingID := str(body["sourceMappingId"], in.ollamaMap)
	mapping, e := p.completionMapping(mappingID)
	if e != nil {
		return e.Status, map[string]any{"error": e.Message}
	}
	base := in.ollamaBaseURL
	p.mark(req.id, map[string]any{
		"status": "delivering", "adapter": "ollama", "provider": "ollama", "incrementAttempts": true, "clearError": true,
		"providerReceipt": map[string]any{"baseUrl": base, "model": model},
	})
	response, err := p.postJSON(base+"/api/chat", ollamaPayload(req.record, body, model, mapping), nil)
	var content string
	var contentJSON, completion any
	committed := false
	if err == nil {
		msg, _ := response["message"].(map[string]any)
		content = str(msg["content"], str(response["response"], ""))
		contentJSON = parseOrNull(content)
		if flag(body, "commitCompletion", true) && mappingID != "" {
			var values []any
			if values, err = valuesToCommit(body, contentJSON, mapping); err == nil {
				var c map[string]any
				if c, err = p.commit(req, "ollama", mappingID, values, mint("ollama-completion"),
					map[string]any{"model": model, "dispatchId": req.id, "content": content}); err == nil {
					completion, committed = c, true
				}
			}
		}
	}
	if err != nil {
		return p.adapterFailure(req, "ollama", err, map[string]any{"baseUrl": base, "model": model}, model)
	}
	run, hasRun := response["created_at"].(string)
	external := run
	if !hasRun {
		external = mint("ollama-run")
	}
	p.mark(req.id, map[string]any{
		"status": "delivered", "adapter": "ollama", "provider": "ollama", "externalRunId": external,
		"providerReceipt": map[string]any{"baseUrl": base, "model": model, "content": content, "completionCommitted": committed},
	})
	return http.StatusOK, map[string]any{
		"success": true, "dispatchId": req.id, "provider": "ollama", "model": model, "response": response,
		"contentJson": contentJSON, "completionCommitted": committed, "completion": completion,
		"receipt": map[string]any{
			"provider": "ollama", "adapter": "ollama", "status": "sent", "externalRunId": run,
			"providerReceipt": map[string]any{"model": model, "completionCommitted": committed},
		},
	}
}

func openaiPayload(record, body map[string]any, model string, mapping map[string]any) map[string]any {
	if pl, ok := body["payload"].(map[string]any); ok {
		return pl
	}
	instructions := str(body["instructions"], openaiInstructions)
	prompt := str(body["prompt"], reviewPrompt)
	payload := map[string]any{
		"model": model, "instructions": instructions, "input": prompt + "\n\n" + encode(record["envelope"]),
		"store": flag(body, "store", true), "stream": false,
		"metadata": map[string]any{
			"dispatchId": str(record["id"], ""), "envelopeId": str(record["envelopeId"], ""),
			"correlationId": str(record["correlationId"], ""), "target": str(record["target"], ""),
		},
	}
	if v, ok := body["maxOutputTokens"].(float64); ok {
		payload["max_output_tokens"] = v
	}
	if v, ok := body["temperature"].(float64); ok {
		payload["temperature"] = v
	}
	if r, ok := body["reasoning"].(map[string]any); ok {
		payload["reasoning"] = r
	}
	if t, ok := body["text"].(map[string]any); ok {
		payload["text"] = t
	} else if mode, ok := body["responseFormatMode"].(string); ok {
		if mode == "json-object" {
			payload["text"] = map[string]any{"format": map[string]any{"type": "json_object"}}
		} else {
			payload["text"] = openaiTextFormat(mapping)
		}
	} else if supportsStructuredOutput(model) {
		payload["text"] = openaiTextFormat(mapping)
	} else {
		payload["instructions"] = instructions + contractPrompt(mapping)
	}
	return payload
}

func openaiReady(response map[string]any) error {
	if e, ok := response["error"].(map[string]any); ok {
		return fmt.Errorf("%s", encode(e))
	}
	if status := str(response["status"], ""); status != "" && status != "completed" {
		return fmt.Errorf("OpenAI response did not complete: %s", status)
	}
	if r := str(response["refusal"], ""); r != "" {
		return fmt.Errorf("OpenAI response refused: %s", r)
	}
	output, _ := response["output"].([]any)
	for _, iv := range output {
		item, _ := iv.(map[string]any)
		parts, _ := item["content"].([]any)
		for _, pv := range parts {
			part, _ := pv.(map[string]any)
			if part["type"] == "refusal" {
				return fmt.Errorf("OpenAI response refused")
			}
			if r := str(part["refusal"], ""); r != "" {
				return fmt.Errorf("OpenAI response refused: %s", r)
			}
		}
	}
	return nil
}

func openaiText(response map[string]any) string {
	if t, ok := response["output_text"].(string); ok {
		return t
	}
	var b strings.Builder
	output, _ := response["output"].([]any)
	for _, iv := range output {
		item, _ := iv.(map[string]any)
		parts, _ := item["content"].([]any)
		for _, pv := range parts {
			part, _ := pv.(map[string]any)
			kind := str(part["type"], "")
			if t, ok := part["text"].(string); ok && (kind == "output_text" || kind == "text") {
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(t)
			}
		}
	}
	return b.String()
}

// OpenAIDispatch is POST /api/integrations/openai/dispatch.
func (p *PE) OpenAIDispatch(body map[string]any) (int, map[string]any) {
	req, e := p.resolveDispatch(body, "OpenAI")
	if e != nil {
		return e.Status, map[string]any{"error": e.Message}
	}
	in := p.integrations
	model := str(body["model"], in.openaiModel)
	mappingID := str(body["sourceMappingId"], in.openaiMap)
	mapping, e := p.completionMapping(mappingID)
	if e != nil {
		return e.Status, map[string]any{"error": e.Message}
	}
	base := in.openaiBaseURL
	p.mark(req.id, map[string]any{
		"status": "delivering", "adapter": "openai", "provider": "openai", "incrementAttempts": true, "clearError": true,
		"providerReceipt": map[string]any{"baseUrl": base, "model": model, "endpoint": "/responses"},
	})
	headers := map[string]string{}
	if in.openaiAPIKey != "" {
		headers["Authorization"] = "Bearer " + in.openaiAPIKey
	}
	response, err := p.postJSON(base+"/responses", openaiPayload(req.record, body, model, mapping), headers)
	var contentJSON, completion any
	var responseID string
	committed := false
	if err == nil {
		err = openaiReady(response)
	}
	if err == nil {
		content := openaiText(response)
		contentJSON = parseOrNull(content)
		responseID = str(response["id"], "")
		if flag(body, "commitCompletion", true) && mappingID != "" {
			var values []any
			if values, err = valuesToCommit(body, contentJSON, mapping); err == nil {
				completionID := responseID
				if _, ok := response["id"].(string); !ok {
					completionID = mint("openai-completion")
				}
				var c map[string]any
				if c, err = p.commit(req, "openai", mappingID, values, completionID, map[string]any{
					"model": model, "dispatchId": req.id, "responseId": responseID, "content": content,
				}); err == nil {
					completion, committed = c, true
				}
			}
		}
	}
	if err != nil {
		return p.adapterFailure(req, "openai", err, map[string]any{"baseUrl": base, "model": model, "endpoint": "/responses"}, model)
	}
	p.mark(req.id, map[string]any{
		"status": "delivered", "adapter": "openai", "provider": "openai", "externalRunId": responseID,
		"providerReceipt": map[string]any{
			"baseUrl": base, "model": model, "responseId": responseID, "status": str(response["status"], ""), "completionCommitted": committed,
		},
	})
	return http.StatusOK, map[string]any{
		"success": true, "dispatchId": req.id, "provider": "openai", "model": model, "response": response,
		"contentJson": contentJSON, "completionCommitted": committed, "completion": completion,
		"receipt": map[string]any{
			"provider": "openai", "adapter": "openai", "status": "sent", "externalRunId": responseID,
			"providerReceipt": map[string]any{"model": model, "completionCommitted": committed},
		},
	}
}

// ACPDispatch is POST /api/integrations/acp/dispatch: a handoff receipt,
// 202, no call.
func (p *PE) ACPDispatch(body map[string]any) (int, map[string]any) {
	req, e := p.resolveDispatch(body, "ACP")
	if e != nil {
		return e.Status, map[string]any{"error": e.Message}
	}
	in := p.integrations
	target := str(req.record["target"], "")
	if target == "" {
		target = in.acpTarget
	}
	target = str(body["targetAgent"], str(body["agent"], target))
	session := str(body["sessionKey"], in.acpSessionKey)
	run := str(body["externalRunId"], "")
	if _, ok := body["externalRunId"].(string); !ok {
		run = mint("acp-handoff")
	}
	handoff := map[string]any{
		"protocol": "ACP", "surface": in.acpSurface, "platform": in.acpPlatform, "adapter": "openclaw-xacp",
		"command": str(body["command"], in.acpCommand), "gatewayUrl": str(body["gatewayUrl"], in.acpGatewayURL),
		"sessionKey": nullable(session), "targetAgent": target, "completionEndpoint": "/api/integrations/completions",
		"completionSourceMappingId": str(body["sourceMappingId"], in.acpMap), "noWaitDispatch": true,
		"prompt": str(body["prompt"], acpPrompt), "dispatchId": req.id,
		"envelopeId": str(req.record["envelopeId"], ""), "correlationId": str(req.record["correlationId"], ""),
	}
	if m, ok := body["metadata"].(map[string]any); ok {
		handoff["metadata"] = m
	}
	p.mark(req.id, map[string]any{
		"status": str(body["status"], "accepted"), "adapter": "openclaw-xacp", "provider": "acp", "externalRunId": run,
		"incrementAttempts": flag(body, "incrementAttempts", true), "clearError": true, "providerReceipt": handoff,
	})
	return http.StatusAccepted, map[string]any{
		"success": true, "accepted": true, "dispatchId": req.id, "provider": "acp",
		"platform": in.acpPlatform, "surface": in.acpSurface, "externalRunId": run,
		"noWaitDispatch": true, "handoff": handoff,
	}
}
