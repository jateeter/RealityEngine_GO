package pe

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
)

// Integrations (RealityEngine_GO#3, slice B): the integrations
// configuration and the status routes over it. C++ load_integration_registry,
// configure_ollama_from_environment and the *_status functions are the
// concrete form, Rust's pe/integrations.rs prior art.
//
//   - The configuration is INTEGRATIONS_CONFIG, else config/integrations.json
//     beside the process, read once at boot: its sourceMappings by id, and
//     each integration's settings by kind. The environment then overrides it
//     (OLLAMA_*, OPENAI_*, ACP_* / OPENCLAW_*, HEALTHKIT_*, CAREKIT_*).
//   - Ollama, OpenAI and localAI are asked whether they answer, outside the
//     engine lock; OpenAI is asked with its API key when one is configured.

// localAISensors are the three sensors localAI's bootstrap registers.
var localAISensors = []struct {
	id, name       string
	offset, length int64
}{
	{"localai_rag_retrieval", "localai/rag_retrieval", 52, 4},
	{"localai_rag_grading", "localai/rag_grading", 56, 4},
	{"localai_agent_activity", "localai/agent_activity", 64, 4},
}

// Integrations is the PE's integrations configuration.
type Integrations struct {
	path                                    *string
	loaded                                  bool
	err                                     *string
	config                                  map[string]any
	sourceMappings                          map[string]map[string]any
	ollamaBaseURL, ollamaModel, ollamaMap   string
	openaiBaseURL, openaiModel, openaiMap   string
	openaiAPIKey                            string
	acpEnabled                              bool
	acpPlatform, acpSurface, acpCommand     string
	acpGatewayURL, acpSessionKey, acpTarget string
	acpMap                                  string
	healthkitBridge, healthkitMap, hkToken  string
	carekitBridge, carekitMap, carekitToken string
	localAIBaseURL, localAIMachinesDir      string
}

func defaultIntegrations() *Integrations {
	return &Integrations{
		config: map[string]any{}, sourceMappings: map[string]map[string]any{},
		ollamaBaseURL: "http://localhost:11434", ollamaModel: "nemotron-3-nano:4b-mlx", ollamaMap: "agent-completion-risk",
		openaiBaseURL: "http://localhost:11434/v1", openaiModel: "nemotron-3-nano:4b-mlx", openaiMap: "agent-completion-risk",
		acpEnabled: true, acpPlatform: "OpenClaw", acpSurface: "xACP", acpCommand: "openclaw acp",
		acpGatewayURL: "ws://127.0.0.1:18789", acpSessionKey: "agent:main:main", acpTarget: "openclaw",
		acpMap: "acp-openclaw-completion", healthkitBridge: "healthkit-ios-bridge", healthkitMap: "healthkit-activity",
		carekitBridge: "carekit-ios-bridge", carekitMap: "carekit-task",
		localAIBaseURL: "http://localhost:4000", localAIMachinesDir: "../localAIStack/data/machines",
	}
}

// integrationsFromEnvironment loads the configuration and applies the
// environment over it.
func integrationsFromEnvironment() *Integrations {
	in := defaultIntegrations()
	path := os.Getenv("INTEGRATIONS_CONFIG")
	if path == "" {
		if _, err := os.Stat("config/integrations.json"); err == nil {
			path = "config/integrations.json"
		}
	}
	if path != "" {
		in.load(path)
	}
	set := func(field *string, names ...string) {
		for _, n := range names {
			if v, ok := os.LookupEnv(n); ok {
				*field = v
			}
		}
	}
	set(&in.ollamaBaseURL, "OLLAMA_BASE_URL")
	set(&in.ollamaModel, "OLLAMA_MODEL")
	set(&in.ollamaMap, "OLLAMA_COMPLETION_SOURCE_MAPPING_ID")
	set(&in.openaiBaseURL, "OPENAI_BASE_URL")
	set(&in.openaiModel, "OPENAI_MODEL")
	set(&in.openaiMap, "OPENAI_COMPLETION_SOURCE_MAPPING_ID")
	set(&in.openaiAPIKey, "OPENAI_API_KEY")
	if v, ok := os.LookupEnv("ACP_ENABLED"); ok {
		in.acpEnabled = truthy(v)
	}
	set(&in.acpPlatform, "ACP_PLATFORM")
	set(&in.acpSurface, "ACP_SURFACE")
	set(&in.acpCommand, "ACP_COMMAND", "OPENCLAW_ACP_COMMAND")
	set(&in.acpGatewayURL, "ACP_GATEWAY_URL", "OPENCLAW_GATEWAY_URL")
	set(&in.acpSessionKey, "ACP_SESSION_KEY", "OPENCLAW_ACP_SESSION")
	set(&in.acpTarget, "ACP_TARGET_AGENT")
	set(&in.acpMap, "ACP_COMPLETION_SOURCE_MAPPING_ID")
	set(&in.healthkitBridge, "HEALTHKIT_BRIDGE_ID")
	set(&in.healthkitMap, "HEALTHKIT_DEFAULT_SOURCE_MAPPING_ID")
	set(&in.hkToken, "HEALTHKIT_BRIDGE_TOKEN")
	set(&in.carekitBridge, "CAREKIT_BRIDGE_ID")
	set(&in.carekitMap, "CAREKIT_DEFAULT_SOURCE_MAPPING_ID")
	set(&in.carekitToken, "CAREKIT_BRIDGE_TOKEN")
	set(&in.localAIBaseURL, "LOCAL_AI_API_URL")
	set(&in.localAIMachinesDir, "LOCAL_AI_MACHINES_DIR")
	in.ollamaBaseURL = strings.TrimRight(in.ollamaBaseURL, "/")
	in.openaiBaseURL = strings.TrimRight(in.openaiBaseURL, "/")
	return in
}

func (in *Integrations) load(path string) {
	in.path = &path
	data, err := os.ReadFile(path)
	var config map[string]any
	if err != nil {
		msg := "unable to open integrations config: " + path
		in.err = &msg
	} else if err := json.Unmarshal(data, &config); err != nil {
		msg := err.Error()
		in.err = &msg
	}
	if in.err != nil {
		log.Printf("perception-engine: integrations config failed to load: %s", *in.err)
		return
	}
	mappings, _ := config["sourceMappings"].([]any)
	for _, mv := range mappings {
		if m, ok := mv.(map[string]any); ok {
			if id := str(m["id"], ""); id != "" {
				in.sourceMappings[id] = m
			}
		}
	}
	items, _ := config["integrations"].([]any)
	for _, iv := range items {
		item, _ := iv.(map[string]any)
		set := func(field *string, key string) {
			if v, ok := item[key].(string); ok {
				*field = v
			}
		}
		switch str(item["kind"], "") {
		case "ollama":
			set(&in.ollamaBaseURL, "baseUrl")
			set(&in.ollamaModel, "model")
			set(&in.ollamaMap, "completionSourceMappingId")
		case "openai":
			set(&in.openaiBaseURL, "baseUrl")
			set(&in.openaiModel, "model")
			set(&in.openaiMap, "completionSourceMappingId")
		case "acp", "openclaw-acp":
			if b, ok := item["enabled"].(bool); ok {
				in.acpEnabled = b
			}
			set(&in.acpPlatform, "platform")
			set(&in.acpSurface, "surface")
			set(&in.acpCommand, "command")
			set(&in.acpGatewayURL, "gatewayUrl")
			set(&in.acpSessionKey, "sessionKey")
			set(&in.acpTarget, "targetAgent")
			set(&in.acpMap, "completionSourceMappingId")
		case "healthkit":
			set(&in.healthkitBridge, "bridgeId")
			set(&in.healthkitMap, "defaultSourceMappingId")
		case "carekit":
			set(&in.carekitBridge, "bridgeId")
			set(&in.carekitMap, "defaultSourceMappingId")
		}
	}
	in.loaded, in.config = true, config
	log.Printf("perception-engine: integrations config loaded, path=%s sourceMappings=%d", path, len(in.sourceMappings))
}

// localAIAllowedOperations is the localai integration's allowedOperations
// and the config path that declared them.
func (in *Integrations) localAIAllowedOperations() ([]any, *string) {
	items, _ := in.config["integrations"].([]any)
	for _, iv := range items {
		item, _ := iv.(map[string]any)
		if item["kind"] != "localai" {
			continue
		}
		if ops, ok := item["allowedOperations"].([]any); ok {
			return ops, in.path
		}
		return []any{}, nil
	}
	return []any{}, nil
}

func (in *Integrations) acpStatus() map[string]any {
	return map[string]any{
		"enabled": in.acpEnabled, "platform": in.acpPlatform, "surface": in.acpSurface,
		"adapter": "openclaw-xacp", "command": in.acpCommand, "gatewayUrl": nullable(in.acpGatewayURL),
		"sessionKey": nullable(in.acpSessionKey), "targetAgent": in.acpTarget,
		"completionSourceMappingId": in.acpMap, "dispatchEndpoint": "/api/integrations/acp/dispatch",
		"completionEndpoint": "/api/integrations/completions", "noWaitDispatch": true,
		"contract": map[string]any{
			"dispatch":   "Record an ACP/OpenClaw handoff receipt only; do not run or wait for the harness in the PE cycle.",
			"completion": "External ACP/OpenClaw adapters commit finished results through /api/integrations/completions.",
		},
	}
}

func optional(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

func (in *Integrations) status() map[string]any {
	integrations := []any{}
	items, _ := in.config["integrations"].([]any)
	for _, iv := range items {
		item, _ := iv.(map[string]any)
		enabled, _ := item["enabled"].(bool)
		integrations = append(integrations, map[string]any{"id": str(item["id"], ""), "kind": str(item["kind"], ""), "enabled": enabled})
	}
	ids := make([]string, 0, len(in.sourceMappings))
	for id := range in.sourceMappings {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	mappings := []any{}
	for _, id := range ids {
		m := in.sourceMappings[id]
		var region, ttl any
		if r, ok := m["region"].(map[string]any); ok {
			region = r
		}
		if t, ok := m["ttlMs"].(float64); ok {
			ttl = t
		}
		mappings = append(mappings, map[string]any{
			"id": id, "sensorId": str(m["sensorId"], ""), "sensorIdTemplate": str(m["sensorIdTemplate"], ""),
			"region": region, "ttlMs": ttl,
		})
	}
	return map[string]any{
		"loaded": in.loaded, "path": optional(in.path), "error": optional(in.err),
		"integrationCount": len(integrations), "sourceMappingCount": len(in.sourceMappings),
		"integrations": integrations, "sourceMappings": mappings, "completionEndpoint": "/api/integrations/completions",
		"ollama": map[string]any{
			"baseUrl": in.ollamaBaseURL, "model": in.ollamaModel, "completionSourceMappingId": in.ollamaMap,
			"statusEndpoint": "/api/integrations/ollama/status", "dispatchEndpoint": "/api/integrations/ollama/dispatch",
		},
		"openai": map[string]any{
			"baseUrl": in.openaiBaseURL, "model": in.openaiModel, "hasApiKey": in.openaiAPIKey != "",
			"completionSourceMappingId": in.openaiMap, "statusEndpoint": "/api/integrations/openai/status",
			"dispatchEndpoint": "/api/integrations/openai/dispatch",
		},
		"acp": in.acpStatus(),
		"healthkit": map[string]any{
			"bridgeId": in.healthkitBridge, "tokenConfigured": in.hkToken != "", "registryKey": "healthkit:<typeIdentifier>",
			"statusEndpoint": "/api/integrations/healthkit/status", "ingestEndpoint": "/api/integrations/healthkit/ingest",
		},
		"carekit": map[string]any{
			"bridgeId": in.carekitBridge, "defaultSourceMappingId": in.carekitMap, "tokenConfigured": in.carekitToken != "",
			"statusEndpoint": "/api/integrations/carekit/status", "ingestEndpoint": "/api/integrations/carekit/ingest",
		},
	}
}

func (in *Integrations) healthkitStatus() map[string]any {
	auth := "none"
	if in.hkToken != "" {
		auth = "bridgeToken|bearer"
	}
	return map[string]any{
		"bridgeId": in.healthkitBridge, "enabled": true, "tokenConfigured": in.hkToken != "",
		"nativeAppRequired": true, "nativeWorkOutsideRepo": true, "registryKey": "healthkit:<typeIdentifier>",
		"statusEndpoint": "/api/integrations/healthkit/status", "ingestEndpoint": "/api/integrations/healthkit/ingest",
		"contract": map[string]any{
			"transport": "https", "singleSample": []string{"type", "value", "sourceName"},
			"batchSamples": []string{"bridgeId", "samples[]"}, "auth": auth,
		},
		"scope": map[string]any{"declared": false, "generation": 0, "types": map[string]any{}, "resyncRequests": []any{}},
	}
}

func (in *Integrations) carekitStatus() map[string]any {
	auth := "external-transport"
	if in.carekitToken != "" {
		auth = "bridgeToken"
	}
	return map[string]any{
		"bridgeId": in.carekitBridge, "enabled": true, "defaultSourceMappingId": in.carekitMap,
		"tokenConfigured": in.carekitToken != "", "nativeAppRequired": true, "nativeWorkOutsideRepo": true,
		"registryKey": "carekit:<sampleType>", "statusEndpoint": "/api/integrations/carekit/status",
		"ingestEndpoint": "/api/integrations/carekit/ingest",
		"contract": map[string]any{
			"transport": "https", "singleSample": []string{"bridgeId", "sampleType", "sourceMappingId", "values"},
			"batchSamples": []string{"bridgeId", "samples[]"}, "auth": auth,
		},
	}
}

// fetchJSON GETs url and decodes the answer.
func (p *PE) fetchJSON(url string, headers map[string]string) (any, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("GET %s: HTTP %s", url, resp.Status)
	}
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	return v, nil
}

// IntegrationsStatus is GET /api/integrations/status.
func (p *PE) IntegrationsStatus() map[string]any { return p.integrations.status() }

// ACPStatus is GET /api/integrations/acp/status.
func (p *PE) ACPStatus() map[string]any { return p.integrations.acpStatus() }

// HealthKitStatus is GET /api/integrations/healthkit/status.
func (p *PE) HealthKitStatus() map[string]any { return p.integrations.healthkitStatus() }

// CareKitStatus is GET /api/integrations/carekit/status.
func (p *PE) CareKitStatus() map[string]any { return p.integrations.carekitStatus() }

// OllamaStatus is GET /api/integrations/ollama/status: whether Ollama answers.
func (p *PE) OllamaStatus() map[string]any {
	in := p.integrations
	tags, err := p.fetchJSON(in.ollamaBaseURL+"/api/tags", nil)
	var errV any
	if err != nil {
		errV = err.Error()
	}
	return map[string]any{
		"baseUrl": in.ollamaBaseURL, "model": in.ollamaModel, "completionSourceMappingId": in.ollamaMap,
		"reachable": err == nil, "tags": tags, "error": errV, "dispatchEndpoint": "/api/integrations/ollama/dispatch",
	}
}

// OpenAIStatus is GET /api/integrations/openai/status.
func (p *PE) OpenAIStatus() map[string]any {
	in := p.integrations
	headers := map[string]string{}
	if in.openaiAPIKey != "" {
		headers["Authorization"] = "Bearer " + in.openaiAPIKey
	}
	models, err := p.fetchJSON(in.openaiBaseURL+"/models", headers)
	reachable := err == nil
	var errV any
	if err != nil {
		errV = err.Error()
	} else if m, ok := models.(map[string]any); ok {
		if e, ok := m["error"].(map[string]any); ok {
			reachable, errV = false, e
		}
	}
	return map[string]any{
		"baseUrl": in.openaiBaseURL, "model": in.openaiModel, "hasApiKey": in.openaiAPIKey != "",
		"completionSourceMappingId": in.openaiMap, "reachable": reachable, "models": models, "error": errV,
		"dispatchEndpoint": "/api/integrations/openai/dispatch",
	}
}

// LocalAIStatus is GET /api/integrations/localai/status.
func (p *PE) LocalAIStatus() map[string]any {
	in := p.integrations
	base := in.localAIBaseURL
	reachable := false
	var root, health any
	if r, err := p.fetchJSON(base+"/", nil); err != nil {
		health = map[string]any{"error": err.Error()}
	} else {
		root = r
		if h, err := p.fetchJSON(base+"/health", nil); err != nil {
			health = map[string]any{"error": err.Error()}
		} else {
			reachable, health = true, h
		}
	}
	p.mu.RLock()
	sensors := []any{}
	for _, s := range localAISensors {
		sensors = append(sensors, map[string]any{
			"sensorId": s.id, "name": s.name, "region": map[string]any{"offset": s.offset, "length": s.length},
			"registered": p.sensorByID(s.id) != nil,
		})
	}
	p.mu.RUnlock()
	return map[string]any{
		"localAIBaseUrl": base, "reachable": reachable, "root": root, "health": health,
		"sensors": sensors, "machineDirectory": in.localAIMachinesDir,
	}
}
