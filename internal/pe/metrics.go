package pe

// GET /api/metrics: the Prometheus exposition RealityEngine_CI
// docs/PE_METRICS_CONTRACT.md defines, as C++ semantic_metrics_text and
// mqtt_metrics_text write it.
//
// The whole text must be byte-identical across runtimes once the runtime
// label's value is normalized, so the writer is literal: every metric is
// three lines (HELP, TYPE, sample), labels sorted by key, integer values,
// multi-series counters in ascending label order and absent until a label
// value is seen. The HELP strings are the contract's.
//
//   - Core gauges: sources, globalStep, the vector dimension, and the last
//     push's step timestamp (0 before any push).
//   - Semantic block: the corpus ABox manifest (resolved, machines), the audit
//     records (capped at 1000), one perception event per active source after
//     each push by its integration (origin, else kind) and whether its machine
//     resolved to an IRI, dispatch records with a semantics link, and
//     escalation-class dispatches by RAG status ("unstated" when absent).
//   - MQTT block: the two gauges at 0 when no bridge runs; otherwise the
//     bridge's counters and its rule count.

import (
	"sort"
	"strconv"
	"strings"
)

// metricsRuntime is the runtime label this engine writes; the parity check
// normalizes it.
const metricsRuntime = "go"

// auditCapacity bounds the audit ring buffer, as C++ caps
// semanticAuditRecords.
const auditCapacity = 1000

// tally holds the semantic guardrail counters: monotonic for the process
// lifetime, bumped where records are made, so no eviction loses a count.
type tally struct {
	events, eventsJoined, escalations map[string]int64
	dispatchTotal, dispatchJoined     int64
	auditRecords                      int64
}

func newTally() tally {
	return tally{events: map[string]int64{}, eventsJoined: map[string]int64{}, escalations: map[string]int64{}}
}

// perceptionEvent is C++ record_perception_event.
func (t *tally) perceptionEvent(integration string, joined bool) {
	t.events[integration]++
	t.eventsJoined[integration] += 0 // the series exists once the integration is seen
	if joined {
		t.eventsJoined[integration]++
	}
	if t.auditRecords < auditCapacity {
		t.auditRecords++
	}
}

// dispatchRecord counts a dispatch record's semantics link, and an
// escalation-class action (emergency-dispatch, urgent-intervention) by the
// RAG status behind it, as C++ dispatch_semantics does.
func (t *tally) dispatchRecord(joined bool, action, rag string) {
	t.dispatchTotal++
	if joined {
		t.dispatchJoined++
	}
	if action == "emergency-dispatch" || action == "urgent-intervention" {
		if rag == "" {
			rag = "unstated"
		}
		t.escalations[rag]++
	}
}

// recordPerceptionEvents emits one re:PerceptionEvent per active source after
// a push, attributed to the integration that feeds it and joined when its
// machine names a corpus ABox IRI. The caller holds p.mu.
func (p *PE) recordPerceptionEvents() {
	bases := p.dispatch.manifest()
	for _, s := range p.sources {
		if !s.Active {
			continue
		}
		integration, machine := s.kind(), ""
		if s.sensor != nil && s.sensor.origin != "" {
			integration = s.sensor.origin
		}
		if s.test != nil {
			machine = s.test.machineName
		}
		_, joined := bases[machine]
		p.dispatch.tally.perceptionEvent(integration, machine != "" && joined)
	}
}

// metric writes one metric's three lines, the runtime label among the others
// by key.
func metric(out *strings.Builder, name, help, kind string, labels [][2]string, value int64) {
	all := append(append([][2]string{}, labels...), [2]string{"runtime", metricsRuntime})
	sort.SliceStable(all, func(i, j int) bool { return all[i][0] < all[j][0] })
	out.WriteString("# HELP " + name + " " + help + "\n# TYPE " + name + " " + kind + "\n" + name + "{")
	for i, l := range all {
		if i > 0 {
			out.WriteByte(',')
		}
		out.WriteString(l[0] + "=\"" + l[1] + "\"")
	}
	out.WriteString("} " + strconv.FormatInt(value, 10) + "\n")
}

// series writes a multi-series counter in ascending label-value order.
func series(out *strings.Builder, name, help, label string, counts map[string]int64) {
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		metric(out, name, help, "counter", [][2]string{{label, k}}, counts[k])
	}
}

// Metrics is GET /api/metrics.
func (p *PE) Metrics() string {
	var out strings.Builder
	p.mu.Lock()
	var lastPushMS int64
	if step, ok := p.lastPush.(map[string]any); ok {
		if t, ok := step["timestamp"].(float64); ok {
			lastPushMS = int64(t)
		}
	}
	machines := int64(len(p.dispatch.manifest()))
	t := &p.dispatch.tally
	available := int64(0)
	if machines > 0 {
		available = 1
	}
	metric(&out, "perception_engine_sources_total", "Total sensor/test/simulated sources registered.", "gauge", nil, int64(len(p.sources)))
	metric(&out, "perception_engine_global_step", "Engine globalStep counter (push count since start).", "gauge", nil, int64(p.globalStep))
	metric(&out, "perception_engine_vector_size", "Configured vector dimension.", "gauge", nil, int64(p.dimension))
	metric(&out, "perception_engine_last_push_ms", "Wall-clock timestamp of the last successful push (0 if never).", "gauge", nil, lastPushMS)
	metric(&out, "semantic_manifest_available", "Corpus OWL semantics manifest resolved (1/0).", "gauge", nil, available)
	metric(&out, "semantic_manifest_machines", "Machines carrying a semantic identity in the manifest.", "gauge", nil, machines)
	metric(&out, "semantic_audit_buffer_records", "re:PerceptionEvent records held in the audit ring buffer.", "gauge", nil, t.auditRecords)
	series(&out, "semantic_perception_events_total", "re:PerceptionEvent records emitted, by originating integration.", "integration", t.events)
	series(&out, "semantic_perception_events_iri_joined_total", "Perception events whose machine resolved to a corpus ABox IRI.", "integration", t.eventsJoined)
	metric(&out, "semantic_dispatch_records_total", "Dispatch records created with a semantics link.", "counter", nil, t.dispatchTotal)
	metric(&out, "semantic_dispatch_records_iri_joined_total", "Dispatch records whose machine resolved to a corpus ABox IRI.", "counter", nil, t.dispatchJoined)
	series(&out, "semantic_escalation_dispatches_total", "Escalation-class actions dispatched, by RAG status of the determination.", "rag", t.escalations)
	p.mu.Unlock()
	p.mqttMetrics(&out)
	return out.String()
}

// mqttMetrics is the MQTT bridge block, in the names, HELP text and order the
// TypeScript PE emits. A disabled bridge emits the two gauges at 0 and
// nothing else.
func (p *PE) mqttMetrics(out *strings.Builder) {
	b := p.bridge()
	if b == nil {
		metric(out, "mqtt_bridge_enabled", "MQTT bridge is configured (1) or disabled (0).", "gauge", nil, 0)
		metric(out, "mqtt_bridge_connected", "MQTT bridge is currently connected to the broker (1/0).", "gauge", nil, 0)
		return
	}
	b.mu.Lock()
	s, connected, rules := b.stats, b.connected, len(b.registry.rules)
	b.mu.Unlock()
	conn := int64(0)
	if connected {
		conn = 1
	}
	metric(out, "mqtt_bridge_enabled", "MQTT bridge is configured (1) or disabled (0).", "gauge", nil, 1)
	metric(out, "mqtt_bridge_connected", "MQTT bridge is currently connected to the broker (1/0).", "gauge", nil, conn)
	metric(out, "mqtt_messages_received_total", "Total MQTT PUBLISH messages received.", "counter", nil, s.received)
	metric(out, "mqtt_messages_mapped_total", "Total messages successfully mapped to a region.", "counter", nil, s.mapped)
	metric(out, "mqtt_messages_rejected_total", "Total messages rejected by mapping/normalize.", "counter", nil, s.rejected)
	metric(out, "mqtt_messages_unmatched_total", "Total messages whose topic matched no rule.", "counter", nil, s.unmatched)
	metric(out, "mqtt_pushes_triggered_total", "Total perceive pushes triggered by MQTT ingest.", "counter", nil, s.pushes)
	metric(out, "mqtt_mappings_loaded", "Number of mapping rules in the registry.", "gauge", nil, int64(rules))
}
