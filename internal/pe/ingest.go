package pe

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/jateeter/RealityEngine_GO/internal/instance"
)

// The ingest routes (RealityEngine_GO#3, slice B): POST
// /api/integrations/completions, /healthkit/ingest and /carekit/ingest. C++
// ingest_completion, ingest_healthkit and ingest_carekit are the concrete
// form; localHealthkitBridge/docs/INGEST_CONTRACT.md is HealthKit's contract.
// Each resolves a source mapping, derives a sensorId and a signal from it, and
// sends the signal through POST /api/signals' own path, so a value lands,
// registers and earns activity exactly as a signal does.
//
//   - Completions: an external agent's finished result. The mapping is
//     sourceMappingId (404 when unknown) with sourceMapping over it; the
//     envelope's own region, values and TTL win over the mapping's.
//   - HealthKit: one sample or samples[], each resolved or unmapped: 200 when
//     all resolved, 207 when some, 400 when none. A sample's mapping is
//     healthkit:<type>:<sourceName>, else healthkit:<type>. With
//     HEALTHKIT_BRIDGE_TOKEN set, the body's token or the bearer must match.
//   - CareKit: one sample or samples[] merged over the body, each through its
//     mapping: 200 when all succeeded, else 207. With CAREKIT_BRIDGE_TOKEN
//     set, the body's token must match.

// SignalReply is POST /api/signals: the status and the answer.
func (p *PE) SignalReply(body map[string]any) (int, map[string]any) {
	p.mu.Lock()
	s, err := p.ingestSignal(body)
	var source any
	if s != nil {
		source = s.json(nowMS())
	}
	p.mu.Unlock()
	if err != nil {
		e := err.(*Error)
		return e.Status, map[string]any{"error": e.Message}
	}
	out := map[string]any{"success": true, "timestamp": nowMS(), "source": source}
	if trigger, _ := body["triggerPush"].(bool); trigger {
		compact, _ := body["compactPush"].(bool)
		_, out["push"] = p.Push(!compact, false, nil)
	}
	p.BroadcastState()
	return http.StatusOK, out
}

func merge(base, overlay any) map[string]any {
	out := map[string]any{}
	if b, ok := base.(map[string]any); ok {
		for k, v := range b {
			out[k] = v
		}
	}
	if o, ok := overlay.(map[string]any); ok {
		for k, v := range o {
			out[k] = v
		}
	}
	return out
}

func template(id string, tokens ...string) string {
	for i := 0; i+1 < len(tokens); i += 2 {
		id = strings.ReplaceAll(id, tokens[i], tokens[i+1])
	}
	return id
}

func bearer(r *http.Request) string {
	v := r.Header.Get("Authorization")
	if len(v) > 7 && strings.EqualFold(v[:7], "bearer ") {
		return strings.TrimLeft(v[7:], " \t")
	}
	return ""
}

func firstString(m map[string]any, keys ...string) (string, bool) {
	for _, k := range keys {
		if s, ok := m[k].(string); ok {
			return s, true
		}
	}
	return "", false
}

// Completion is POST /api/integrations/completions.
func (p *PE) Completion(body map[string]any) (int, map[string]any) {
	in := p.integrations
	mappingID, _ := firstString(body, "sourceMappingId", "mappingId")
	mapping := map[string]any{}
	if mappingID != "" {
		found, ok := in.sourceMappings[mappingID]
		if !ok {
			return http.StatusNotFound, map[string]any{"error": fmt.Sprintf("Unknown sourceMappingId %q", mappingID)}
		}
		mapping = found
	}
	mapping = merge(mapping, body["sourceMapping"])
	provider := str(body["provider"], "external")
	agent, ok := firstString(body, "agent", "agentId")
	if !ok {
		agent = "agent"
	}
	correlation, envelope := str(body["correlationId"], ""), str(body["envelopeId"], "")
	sensorID, _ := firstString(body, "sensorId")
	if sensorID == "" {
		sensorID = str(mapping["sensorId"], "")
	}
	if pattern, ok := mapping["sensorIdTemplate"].(string); ok && sensorID == "" {
		sensorID = template(pattern, "{provider}", idPart(provider), "{agent}", idPart(agent),
			"{correlationId}", idPart(correlation), "{envelopeId}", idPart(envelope))
	}
	if sensorID == "" {
		sensorID = "agent." + idPart(agent) + ".completion"
	}
	name, ok := firstString(body, "name")
	if !ok {
		if name, ok = mapping["name"].(string); !ok {
			name = "agent:" + provider + "/" + agent + "/completion"
		}
	}
	values := mapping["values"]
	if v, ok := body["values"].([]any); ok {
		values = v
	}
	ttl := any(300000.0)
	if t, ok := mapping["ttlMs"].(float64); ok {
		ttl = t
	}
	if t, ok := body["ttlMs"].(float64); ok {
		ttl = t
	}
	trigger, _ := body["triggerPush"].(bool)
	compact := true
	if b, ok := body["compactPush"].(bool); ok {
		compact = b
	}
	signal := map[string]any{
		"sensorId": sensorID, "name": name, "values": values, "ttlMs": ttl,
		"triggerPush": trigger, "compactPush": compact, "origin": provider,
	}
	// The envelope's own region wins, then the mapping's (C++ #24).
	if r, ok := body["region"].(map[string]any); ok {
		signal["region"] = r
	} else if r, ok := mapping["region"].(map[string]any); ok {
		signal["region"] = r
	}
	status, reply := p.SignalReply(signal)
	if status != http.StatusOK {
		return status, reply
	}
	completionID, _ := firstString(body, "completionId", "id")
	// The completion's clock and causedBy (#600): the dispatch's clock while
	// its record is in the ledger, else the sender's well-formed echo, else nil.
	var own any
	if id, ok := body["dispatchId"].(string); ok && id != "" {
		p.mu.RLock()
		if r, found := p.dispatch.records[id]; found {
			own = r["clock"]
		}
		p.mu.RUnlock()
	}
	causedBy := instance.CausedBy(own, body["causedBy"])
	completionClock := p.clock.Event()
	completion := map[string]any{
		"clock": completionClock, "causedBy": causedBy,
		"provider": provider, "agent": agent, "sensorId": sensorID, "sourceMappingId": mappingID,
		"correlationId": correlation, "envelopeId": envelope, "completionId": completionID, "receivedAt": nowMS(),
	}
	if m, ok := body["metadata"].(map[string]any); ok {
		completion["metadata"] = m
	}
	p.broadcast(map[string]any{
		"type": "agent.completion.received", "provider": provider, "agent": agent, "sensorId": sensorID,
		"sourceMappingId": mappingID, "correlationId": correlation, "envelopeId": envelope, "timestamp": nowMS(),
		"clock": completionClock, "causedBy": causedBy,
	})
	return http.StatusOK, map[string]any{"success": true, "completion": completion, "signal": reply}
}

var hkPrefixes = []string{
	"HKQuantityTypeIdentifier", "HKCategoryTypeIdentifier", "HKWorkoutTypeIdentifier", "HKCorrelationTypeIdentifier",
	"HKDocumentTypeIdentifier", "HKClinicalTypeIdentifier", "HKSeriesTypeIdentifier", "HKTypeIdentifier",
}

func compactHKIdentifier(kind string) string {
	for _, p := range hkPrefixes {
		if strings.HasPrefix(kind, p) {
			kind = strings.TrimPrefix(kind, p)
			break
		}
	}
	var b strings.Builder
	for i := 0; i < len(kind); i++ {
		c := kind[i]
		switch {
		case c >= 'A' && c <= 'Z':
			b.WriteByte(c + 32)
		case c >= 'a' && c <= 'z' || c >= '0' && c <= '9':
			b.WriteByte(c)
		}
	}
	if b.Len() == 0 {
		return "unknown"
	}
	return b.String()
}

func hkSensorID(kind, source string, mapping map[string]any) string {
	if fixed, _ := mapping["sensorId"].(string); fixed != "" {
		return fixed
	}
	if pattern, _ := mapping["sensorIdTemplate"].(string); pattern != "" {
		return template(pattern, "{type}", idPart(kind), "{sampleType}", idPart(kind), "{source}", idPart(source),
			"{provider}", "healthkit", "{agent}", idPart(source))
	}
	suffix := ""
	if source != "" {
		suffix = "." + idPart(source)
	}
	return "hk." + compactHKIdentifier(kind) + suffix
}

func unmapped(kind, source, reason string) map[string]any {
	return map[string]any{"unmapped": true, "type": kind, "sourceName": source, "reason": reason}
}

func (p *PE) healthkitOne(sample map[string]any) map[string]any {
	in := p.integrations
	kind, _ := firstString(sample, "type", "sampleType")
	source := str(sample["sourceName"], "")
	if kind == "" {
		return unmapped(kind, source, "sample.type is required")
	}
	var values []float64
	if vs, ok := sample["values"].([]any); ok {
		values = numbers(vs)
	} else if v, ok := sample["value"].(float64); ok {
		values = []float64{v}
	}
	if len(values) == 0 {
		return unmapped(kind, source, "sample.value must be a finite number")
	}
	mappingID, _ := firstString(sample, "sourceMappingId", "mappingId")
	var mapping map[string]any
	if mappingID == "" {
		if source != "" {
			mapping = in.sourceMappings["healthkit:"+kind+":"+source]
		}
		if mapping == nil {
			mapping = in.sourceMappings["healthkit:"+kind]
		}
		if mapping != nil {
			mapping = merge(mapping, nil)
		}
	} else {
		found, ok := in.sourceMappings[mappingID]
		if !ok {
			out := unmapped(kind, source, fmt.Sprintf("unknown sourceMappingId %q", mappingID))
			out["sourceMappingId"] = mappingID
			return out
		}
		mapping = merge(found, nil)
		mapping["id"] = mappingID
	}
	if o, ok := sample["sourceMapping"].(map[string]any); ok {
		mapping = merge(mapping, o)
	}
	if mapping == nil {
		return unmapped(kind, source, fmt.Sprintf("no registry mapping (declare healthkit:%s[:<sourceName>])", kind))
	}
	region, ok := mapping["region"].(map[string]any)
	if !ok {
		return unmapped(kind, source, "mapping is missing region.offset/region.length")
	}
	sensorID := hkSensorID(kind, source, mapping)
	ttl := int64(3600000)
	if t, ok := mapping["ttlMs"].(float64); ok {
		ttl = int64(t)
	}
	name, ok := mapping["name"].(string)
	if !ok {
		name = "healthkit:" + kind
	}
	signalValues := any([]any{sample["value"]})
	if vs, ok := sample["values"].([]any); ok {
		signalValues = vs
	}
	status, reply := p.SignalReply(map[string]any{
		"sensorId": sensorID, "name": name, "region": region, "values": signalValues,
		"ttlMs": float64(ttl), "origin": "healthkit",
	})
	if status != http.StatusOK {
		return unmapped(kind, source, str(reply["error"], "ingest_signal failed"))
	}
	return map[string]any{
		"resolved": true, "sensorId": sensorID, "name": name, "type": kind, "sourceName": source,
		"sourceMappingId": str(mapping["id"], ""), "region": region, "values": values, "ttlMs": ttl,
		"source": reply["source"],
	}
}

// HealthKit is POST /api/integrations/healthkit/ingest.
func (p *PE) HealthKit(r *http.Request, body map[string]any) (int, map[string]any) {
	in := p.integrations
	token, _ := firstString(body, "bridgeToken", "token")
	if in.hkToken != "" && token != in.hkToken && bearer(r) != in.hkToken {
		return http.StatusUnauthorized, map[string]any{"error": "HealthKit bridge token rejected"}
	}
	bridgeID := str(body["bridgeId"], in.healthkitBridge)
	samples := []any{body}
	if s, ok := body["samples"].([]any); ok {
		samples = s
	}
	resolved, missed := []any{}, []any{}
	for _, sv := range samples {
		sample, _ := sv.(map[string]any)
		if sample == nil {
			sample = map[string]any{}
		}
		if res := p.healthkitOne(sample); res["resolved"] == true {
			resolved = append(resolved, res)
		} else {
			missed = append(missed, res)
		}
	}
	status := http.StatusOK
	if len(missed) > 0 {
		status = http.StatusMultiStatus
		if len(resolved) == 0 {
			status = http.StatusBadRequest
		}
	}
	p.broadcast(map[string]any{
		"type": "healthkit.ingest", "bridgeId": bridgeID, "samples": len(resolved) + len(missed),
		"resolved": len(resolved), "unmapped": len(missed), "timestamp": nowMS(),
	})
	out := map[string]any{"success": len(missed) == 0, "bridgeId": bridgeID, "resolved": resolved, "unmapped": missed}
	if resync, _ := body["resyncId"].(string); resync != "" {
		out["resyncId"] = resync
	}
	return status, out
}

func (p *PE) carekitOne(body map[string]any) (map[string]any, error) {
	in := p.integrations
	mappingID, ok := firstString(body, "sourceMappingId", "mappingId")
	if !ok {
		mappingID = in.carekitMap
	}
	mapping := map[string]any{}
	if mappingID != "" {
		found, ok := in.sourceMappings[mappingID]
		if !ok {
			return nil, &Error{400, fmt.Sprintf("Unknown sourceMappingId %q", mappingID)}
		}
		mapping = found
	}
	mapping = merge(mapping, body["sourceMapping"])
	if mappingID != "" {
		mapping["id"] = mappingID
	}
	bridgeID := str(body["bridgeId"], in.carekitBridge)
	sampleType, ok := firstString(body, "sampleType", "type")
	if !ok {
		sampleType = "task-event"
	}
	sensorID, _ := firstString(body, "sensorId")
	if sensorID == "" {
		sensorID = str(mapping["sensorId"], "")
	}
	if pattern, ok := mapping["sensorIdTemplate"].(string); ok && sensorID == "" {
		sensorID = template(pattern, "{bridgeId}", idPart(bridgeID), "{sampleType}", idPart(sampleType),
			"{type}", idPart(sampleType), "{taskId}", idPart(str(body["taskId"], sampleType)),
			"{carePlanId}", idPart(str(body["carePlanId"], "care-plan")))
	}
	if sensorID == "" {
		sensorID = "carekit." + idPart(sampleType)
	}
	name, ok := firstString(body, "name")
	if !ok {
		if name, ok = mapping["name"].(string); !ok {
			name = "carekit:" + sampleType
		}
	}
	values := any([]any{})
	if v, ok := body["values"].([]any); ok {
		values = v
	} else if v, ok := body["value"].(float64); ok {
		values = []any{v}
	}
	ttl := any(300000.0)
	if t, ok := mapping["ttlMs"].(float64); ok {
		ttl = t
	}
	if t, ok := body["ttlMs"].(float64); ok {
		ttl = t
	}
	trigger, _ := body["triggerPush"].(bool)
	compact := true
	if b, ok := body["compactPush"].(bool); ok {
		compact = b
	}
	signal := map[string]any{
		"sensorId": sensorID, "name": name, "values": values, "ttlMs": ttl,
		"triggerPush": trigger, "compactPush": compact, "origin": "carekit",
	}
	if r, ok := mapping["region"].(map[string]any); ok {
		signal["region"] = r
	}
	if r, ok := body["region"].(map[string]any); ok {
		signal["region"] = r
	}
	status, reply := p.SignalReply(signal)
	st, _ := firstString(body, "sampleType", "type")
	return map[string]any{
		"success": status == http.StatusOK, "status": status, "sampleType": st,
		"taskId": str(body["taskId"], ""), "carePlanId": str(body["carePlanId"], ""),
		"sourceMappingId": str(mapping["id"], ""), "sensorId": sensorID, "result": reply,
	}, nil
}

// CareKit is POST /api/integrations/carekit/ingest.
func (p *PE) CareKit(body map[string]any) (int, map[string]any) {
	in := p.integrations
	token, _ := firstString(body, "bridgeToken", "token")
	if in.carekitToken != "" && token != in.carekitToken {
		return http.StatusUnauthorized, map[string]any{"error": "CareKit bridge token rejected"}
	}
	samples := []map[string]any{body}
	if s, ok := body["samples"].([]any); ok {
		samples = nil
		for _, sv := range s {
			merged := merge(body, sv)
			delete(merged, "samples")
			delete(merged, "bridgeToken")
			samples = append(samples, merged)
		}
	}
	results, allOK := []any{}, true
	for _, sample := range samples {
		res, err := p.carekitOne(sample)
		if err != nil {
			e := err.(*Error)
			return e.Status, map[string]any{"error": e.Message}
		}
		allOK = allOK && res["success"] == true
		results = append(results, res)
	}
	bridgeID := str(body["bridgeId"], in.carekitBridge)
	p.broadcast(map[string]any{
		"type": "carekit.ingest", "bridgeId": bridgeID, "samples": len(results), "success": allOK, "timestamp": nowMS(),
	})
	status := http.StatusOK
	if !allOK {
		status = http.StatusMultiStatus
	}
	return status, map[string]any{"success": allOK, "bridgeId": bridgeID, "results": results}
}
