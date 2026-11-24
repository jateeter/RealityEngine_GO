package pe

import (
	"strings"
	"testing"
)

// Dispatch records and escalations are counted where semantics are minted, as
// C++ dispatch_semantics counts them; escalation series sort by RAG status.
func TestDispatchSemanticsTally(t *testing.T) {
	t.Setenv("SEMANTICS_MANIFEST", t.TempDir()+"/absent.json")
	p := New(4, "http://127.0.0.1:1")
	p.dispatch.bases = map[string]string{"Known": "urn:x"}
	p.dispatch.semantics("Known", map[string]any{"actionCode": "emergency-dispatch", "ragStatusCode": "RED"}, nil)
	p.dispatch.semantics("Other", map[string]any{"actionCode": "urgent-intervention"}, nil)
	p.dispatch.semantics("Other", map[string]any{"actionCode": "notify"}, nil)
	text := p.Metrics()
	for _, line := range []string{
		"semantic_manifest_available{runtime=\"go\"} 1\n",
		"semantic_dispatch_records_total{runtime=\"go\"} 3\n",
		"semantic_dispatch_records_iri_joined_total{runtime=\"go\"} 1\n",
		"semantic_escalation_dispatches_total{rag=\"RED\",runtime=\"go\"} 1\n" +
			"# HELP semantic_escalation_dispatches_total Escalation-class actions dispatched, by RAG status of the determination.\n" +
			"# TYPE semantic_escalation_dispatches_total counter\n" +
			"semantic_escalation_dispatches_total{rag=\"unstated\",runtime=\"go\"} 1\n",
	} {
		if !strings.Contains(text, line) {
			t.Fatalf("missing %q in\n%s", line, text)
		}
	}
}
