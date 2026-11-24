package server

import (
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// integrationsPE is a PE over a real Go RE (the corpus fixture plus a bound
// machine), trigger dispatch on, an integrations config naming provider as
// its Ollama, OpenAI and localAI.
func integrationsPE(t *testing.T, provider string) (http.Handler, http.Handler) {
	t.Helper()
	re := machinesHandler(t)
	send(t, re, "POST", "/api/machines", `{"version": "1.0.0", "machine": {"id": "machine-bound", "name": "Bound",
	  "outputMergeTransformation": "join",
	  "metadata": {"agentBinding": {"agent": "nurse", "trigger": "t", "allowedActions": ["watch", "page"]},
	    "governance": {"ownerTeam": "care"},
	    "triggerConfig": {"rules": [{"sequenceId": "s", "outputMatches": [1], "ragStatusCode": "RED"}]},
	    "inputSequences": [{"name": "go", "events": [[1]]}]},
	  "perceptualMapping": {"input": {"offset": 20, "length": 1}, "output": {"offset": 21, "length": 1}},
	  "sequences": [{"id": "s", "name": "s", "events": [{"id": "e", "isInitial": true, "elements": [{"value": 1}], "outputEvents": [{"id": "o", "vector": [1]}]}]}]}}`)
	srv := httptest.NewServer(re)
	t.Cleanup(srv.Close)
	config := filepath.Join(t.TempDir(), "integrations.json")
	doc := `{"integrations": [{"kind": "localai", "allowedOperations": [{"id": "health", "method": "GET", "path": "/health"}]}],
	  "sourceMappings": [{"id": "nurse", "sensorId": "agent.nurse.completion", "region": {"offset": 30, "length": 2},
	    "extract": {"type": "json", "pointers": ["/risk", "/flag"]}, "normalize": {"mode": "minmax", "min": 0, "max": 10}},
	   {"id": "healthkit:HKQuantityTypeIdentifierHeartRate", "region": {"offset": 32, "length": 1}},
	   {"id": "carekit-task", "region": {"offset": 33, "length": 1}}]}`
	if err := os.WriteFile(config, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RE_URL", srv.URL)
	t.Setenv("PE_SOURCE_BOOTSTRAP", "")
	t.Setenv("TRIGGERS_ENABLED", "true")
	t.Setenv("INTEGRATIONS_CONFIG", config)
	t.Setenv("OLLAMA_BASE_URL", provider)
	t.Setenv("OLLAMA_COMPLETION_SOURCE_MAPPING_ID", "nurse")
	t.Setenv("OPENAI_BASE_URL", provider)
	t.Setenv("OPENAI_COMPLETION_SOURCE_MAPPING_ID", "nurse")
	t.Setenv("LOCAL_AI_API_URL", provider)
	return mustHandler(t, "PE"), re
}

func fakeProvider(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/chat":
			io.WriteString(w, `{"created_at": "2026-11-01T00:00:00Z", "message": {"role": "assistant", "content": "{\"risk\": 3, \"flag\": true}"}}`)
		case "/responses":
			io.WriteString(w, `{"id": "resp_1", "status": "completed", "output": [{"content": [{"type": "output_text", "text": "{\"risk\": 8, \"flag\": false}"}]}]}`)
		case "/health":
			io.WriteString(w, `{"status": "ok", "confidence": 0.9}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestTriggerDispatchAdaptersAndLedger(t *testing.T) {
	h, _ := integrationsPE(t, fakeProvider(t))
	_, p := send(t, h, "POST", "/api/push", `{"compact": true}`)
	d := p["dispatch"].(map[string]any)
	if p["success"] != true || d["dispatchRecordsCreated"] != 1.0 || d["enabled"] != true {
		t.Fatalf("one bound, governed machine fired: %v", d)
	}
	_, ledger := send(t, h, "GET", "/api/dispatch/ledger", "")
	rec := ledger["records"].([]any)[0].(map[string]any)
	id := rec["id"].(string)
	if rec["target"] != "nurse" || rec["ragStatusCode"] != "RED" || rec["envelope"].(map[string]any)["dispatch"].(map[string]any)["action"] != "watch" {
		t.Fatalf("%v", rec)
	}

	code, o := send(t, h, "POST", "/api/integrations/ollama/dispatch", `{"dispatchId": "`+id+`"}`)
	if code != 200 || o["completionCommitted"] != true {
		t.Fatalf("%d %v", code, o)
	}
	_, r := send(t, h, "GET", "/api/dispatch/records/"+id, "")
	if rr := r["record"].(map[string]any); rr["status"] != "delivered" || rr["attempts"] != 1.0 {
		t.Fatalf("%v", rr)
	}
	_, st := send(t, h, "GET", "/api/state", "")
	if v := st["assembledVector"].([]any); v[30] != 0.3 || v[31] != 0.1 {
		t.Fatalf("the completion landed, minmax-normalised: %v", v[30:32])
	}
	if code, o := send(t, h, "POST", "/api/integrations/openai/dispatch", `{"id": "`+id+`"}`); code != 200 || o["completionCommitted"] != true {
		t.Fatalf("%d %v", code, o)
	}
	if code, a := send(t, h, "POST", "/api/integrations/acp/dispatch", `{"dispatchId": "`+id+`"}`); code != 202 || a["noWaitDispatch"] != true {
		t.Fatalf("%d %v", code, a)
	}
	_, rep := send(t, h, "POST", "/api/dispatch/records/"+id+"/replay", `{"freshIds": true}`)
	if rep["record"].(map[string]any)["replayOf"] != id {
		t.Fatalf("%v", rep)
	}
	for path, want := range map[string]int{
		"/api/integrations/acp/dispatch":    400,
		"/api/integrations/ollama/dispatch": 400,
	} {
		if code, _ := send(t, h, "POST", path, `{}`); code != want {
			t.Fatalf("%s: %d", path, code)
		}
	}
	if code, _ := send(t, h, "POST", "/api/integrations/acp/dispatch", `[1]`); code != 400 {
		t.Fatalf("a non-object body: %d", code)
	}
	if code, _ := send(t, h, "GET", "/api/dispatch/records/nope", ""); code != 404 {
		t.Fatal(code)
	}
	_, tr := send(t, h, "GET", "/api/triggers/status", "")
	if tr["participation"] != "active" || tr["replaysCreated"] != 1.0 {
		t.Fatalf("%v", tr)
	}
}

func TestIngestRoutes(t *testing.T) {
	h, _ := integrationsPE(t, fakeProvider(t))
	if code, c := send(t, h, "POST", "/api/integrations/completions", `{"agent": "nurse", "sourceMappingId": "nurse", "values": [0.5, 0.25]}`); code != 200 || c["completion"].(map[string]any)["sensorId"] != "agent.nurse.completion" {
		t.Fatalf("%d %v", code, c)
	}
	if code, _ := send(t, h, "POST", "/api/integrations/completions", `{"sourceMappingId": "nope", "values": [1]}`); code != 404 {
		t.Fatal(code)
	}
	code, hk := send(t, h, "POST", "/api/integrations/healthkit/ingest", `{"samples": [{"type": "HKQuantityTypeIdentifierHeartRate", "value": 1}, {"type": "HKNope", "value": 1}]}`)
	if code != 207 || len(hk["resolved"].([]any)) != 1 || hk["resolved"].([]any)[0].(map[string]any)["sensorId"] != "hk.heartrate" {
		t.Fatalf("%d %v", code, hk)
	}
	if code, _ := send(t, h, "POST", "/api/integrations/healthkit/ingest", `{"type": "HKNope", "value": 1}`); code != 400 {
		t.Fatal(code)
	}
	if code, ck := send(t, h, "POST", "/api/integrations/carekit/ingest", `{"sampleType": "task-event", "values": [1]}`); code != 200 || ck["success"] != true {
		t.Fatalf("%d %v", code, ck)
	}
	if code, _ := send(t, h, "POST", "/api/integrations/carekit/ingest", `{"sourceMappingId": "nope", "values": [1]}`); code != 400 {
		t.Fatal(code)
	}
	t.Setenv("HEALTHKIT_BRIDGE_TOKEN", "secret")
	h2, _ := integrationsPE(t, fakeProvider(t))
	if code, _ := send(t, h2, "POST", "/api/integrations/healthkit/ingest", `{"type": "HKQuantityTypeIdentifierHeartRate", "value": 1}`); code != 401 {
		t.Fatalf("token required: %d", code)
	}
	if code, _ := send(t, h2, "POST", "/api/integrations/healthkit/ingest", `{"bridgeToken": "secret", "type": "HKQuantityTypeIdentifierHeartRate", "value": 1}`); code != 200 {
		t.Fatalf("token accepted: %d", code)
	}
}

func TestLocalAIInvokeAndStatus(t *testing.T) {
	h, _ := integrationsPE(t, fakeProvider(t))
	if code, out := send(t, h, "POST", "/api/integrations/localai/invoke", `{"endpoint": "/health", "correlationId": "c"}`); code != 200 || out["method"] != "GET" {
		t.Fatalf("the allowed operation's method: %d %v", code, out)
	}
	for body, want := range map[string]int{`{"endpoint": "/admin"}`: 403, `{"endpoint": "/../health", "method": "GET"}`: 403, `{}`: 400} {
		if code, _ := send(t, h, "POST", "/api/integrations/localai/invoke", body); code != want {
			t.Fatalf("%s: %d", body, code)
		}
	}
	_, ledger := send(t, h, "GET", "/api/integrations/localai/ledger", "")
	recs := ledger["records"].([]any)
	if len(recs) != 3 || recs[0].(map[string]any)["confidence"] != 0.9 || recs[0].(map[string]any)["operationId"] != "health" {
		t.Fatalf("every attempt is recorded, refused ones too: %v", recs)
	}
	if _, s := send(t, h, "GET", "/api/integrations/localai/status", ""); s["reachable"] != false {
		t.Fatalf("the fake provider has no root: %v", s)
	}
	if _, s := send(t, h, "GET", "/api/integrations/status", ""); s["loaded"] != true || s["sourceMappingCount"] != 3.0 {
		t.Fatalf("%v", s)
	}
}

// fakeBroker accepts one MQTT client, acknowledges its CONNECT, waits for its
// SUBSCRIBE and publishes one message.
func fakeBroker(t *testing.T, topic, payload string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 1024)
		conn.Read(buf)                    // CONNECT
		conn.Write([]byte{0x20, 2, 0, 0}) // CONNACK accepted
		conn.Read(buf)                    // SUBSCRIBE
		body := binary.BigEndian.AppendUint16(nil, uint16(len(topic)))
		body = append(append(body, topic...), payload...)
		conn.Write(append([]byte{0x30, byte(len(body))}, body...))
		time.Sleep(2 * time.Second)
	}()
	return ln.Addr().String()
}

func TestMQTTBridgeMapsAMessageOntoASensor(t *testing.T) {
	h, _ := integrationsPE(t, fakeProvider(t))
	if _, s := send(t, h, "GET", "/api/mqtt/status", ""); s["enabled"] != false {
		t.Fatal(s)
	}
	if code, _ := send(t, h, "PUT", "/api/mqtt/mappings", `{"mappings": []}`); code != 409 {
		t.Fatalf("no broker: %d", code)
	}
	addr := fakeBroker(t, "farm/north/temp", "20")
	body, _ := json.Marshal(map[string]any{
		"brokerUrl": "mqtt://" + addr,
		"mappings": map[string]any{"mappings": []any{map[string]any{
			"id": "temp", "topicFilter": "farm/+/temp", "sensorIdTemplate": "zone.{1}.temp",
			"region": map[string]any{"offset": 40, "length": 1}, "normalize": map[string]any{"mode": "minmax", "min": 0, "max": 40},
			"pushMode": "manual",
		}}},
	})
	if code, out := send(t, h, "POST", "/api/mqtt/enable", string(body)); code != 200 || out["mappings"] != 1.0 {
		t.Fatalf("%d %v", code, out)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, l := send(t, h, "GET", "/api/sources", "")
		for _, sv := range l["sources"].([]any) {
			s := sv.(map[string]any)
			if s["sensorId"] == "zone.north.temp" {
				if s["name"] != "mqtt:farm/north/temp" || s["lastValue"].([]any)[0] != 0.5 || s["origin"] != "mqtt" {
					t.Fatalf("%v", s)
				}
				_, m := send(t, h, "GET", "/api/mqtt/mappings", "")
				if c := m["mappings"].([]any)[0].(map[string]any)["counters"].(map[string]any); c["mapped"] != 1.0 {
					t.Fatalf("%v", c)
				}
				if _, d := send(t, h, "POST", "/api/mqtt/disable", ""); d["enabled"] != false {
					t.Fatal(d)
				}
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no MQTT sensor arrived: %v", l)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestMQTTRegistryRefusals(t *testing.T) {
	h, _ := integrationsPE(t, fakeProvider(t))
	for body, want := range map[string]string{
		`{}`:                          "brokerUrl is required",
		`{"brokerUrl": "mqtt://x:1"}`: "schema: mqtt-mappings: missing top-level \"mappings\" array",
		`{"brokerUrl": "mqtt://x:1", "mappings": {"mappings": []}}`:                                                            "mappings array is empty — at least one rule is required",
		`{"brokerUrl": "mqtt://x:1", "mappings": {"mappings": [{"id": "a", "topicFilter": "t", "extract": {"type": "xml"}}]}}`: "schema: unknown extract.type: xml",
	} {
		if code, out := send(t, h, "POST", "/api/mqtt/enable", body); code != 400 || !strings.Contains(out["error"].(string), want) {
			t.Fatalf("%s: %d %v", body, code, out)
		}
	}
}
