package server

import (
	"log"
	"net/http"

	"github.com/jateeter/RealityEngine_GO/internal/pe"
	"github.com/jateeter/RealityEngine_GO/internal/stream"
)

// answerOf answers a route that takes no request input.
func answerOf(f func() map[string]any) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, f()) }
}

// status answers with the status and body a route decides.
func status(f func(*http.Request, map[string]any) (int, map[string]any)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := jsonBody(r)
		if err != nil {
			writeError(w, err)
			return
		}
		code, out := f(r, body)
		writeJSON(w, code, out)
	}
}

// objectBody refuses a body that is not a JSON object: "<what> body must be
// a JSON object", 400.
func objectBody(what string, f func(*http.Request, map[string]any) (int, map[string]any)) func(*http.Request, map[string]any) (int, map[string]any) {
	return func(r *http.Request, body map[string]any) (int, map[string]any) {
		if body == nil {
			return http.StatusBadRequest, map[string]any{"error": what + " body must be a JSON object"}
		}
		return f(r, body)
	}
}

// peRoutes are the PE's routes: info, state, configuration and layer-local
// reset, sources and sensors, signals and the RE machines proxy (#8); the
// push cycle and streams (#11); dispatch, triggers and integrations (#3).
func peRoutes(p *pe.PE) map[string]http.Handler {
	// changed answers a mutation and, when it succeeded, broadcasts the new
	// state to the event streams.
	changed := func(f func(*http.Request, map[string]any) (any, error)) http.HandlerFunc {
		return withBody(func(r *http.Request, body map[string]any) (any, error) {
			out, err := f(r, body)
			if err == nil {
				p.BroadcastState()
			}
			return out, err
		})
	}
	return map[string]http.Handler{
		"PE GET /": http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, map[string]any{"service": "Perception Engine (Go)", "status": "running"})
		}),
		// The PE's participant clock (RealityEngine_CI#600).
		"PE GET /api/clock": http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, p.ClockJSON())
		}),
		"PE GET /api/state": http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, p.State())
		}),
		"PE PATCH /api/config": changed(func(_ *http.Request, body map[string]any) (any, error) {
			return p.Configure(body)
		}),
		"PE POST /api/reset": http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			body := p.Reset()
			p.BroadcastState()
			writeJSON(w, http.StatusOK, body)
		}),
		"PE GET /api/sources": http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, p.Sources())
		}),
		"PE POST /api/sources": changed(func(_ *http.Request, body map[string]any) (any, error) {
			return p.CreateSource(body)
		}),
		"PE PATCH /api/sources/:id": changed(func(r *http.Request, body map[string]any) (any, error) {
			return p.UpdateSource(r.PathValue("id"), body)
		}),
		"PE DELETE /api/sources/:id": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, err := p.DeleteSource(r.PathValue("id"))
			if err == nil {
				p.BroadcastState()
			}
			answer(w, body, err)
		}),
		"PE POST /api/sources/bootstrap-from-machines": http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, p.BootstrapFromRE())
		}),
		"PE GET /api/sources/contention": http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, p.Contention())
		}),
		"PE POST /api/sensors/:sensorId": changed(func(r *http.Request, body map[string]any) (any, error) {
			return p.Sensor(r.PathValue("sensorId"), body)
		}),
		"PE POST /api/signals": status(func(_ *http.Request, body map[string]any) (int, map[string]any) {
			return p.SignalReply(body)
		}),
		"PE POST /api/push": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, err := jsonBody(r)
			if err != nil {
				writeError(w, err)
				return
			}
			include := true
			if compact, _ := body["compact"].(bool); compact {
				include = false
			}
			if b, ok := body["includeMachineResults"].(bool); ok {
				include = b
			}
			async, _ := body["async"].(bool)
			status, out := p.Push(include, async, body["only"])
			writeJSON(w, status, out)
		}),
		"PE GET /api/push/:id": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, err := p.Job(r.PathValue("id"))
			answer(w, body, err)
		}),
		"PE POST /api/auto/start": withBody(func(_ *http.Request, body map[string]any) (any, error) {
			return p.AutoStart(body), nil
		}),
		"PE POST /api/auto/stop": http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, p.AutoStop())
		}),
		"PE GET /api/integrations/status":             answerOf(p.IntegrationsStatus),
		"PE GET /api/integrations/acp/status":         answerOf(p.ACPStatus),
		"PE GET /api/integrations/healthkit/status":   answerOf(p.HealthKitStatus),
		"PE GET /api/integrations/carekit/status":     answerOf(p.CareKitStatus),
		"PE GET /api/integrations/ollama/status":      answerOf(p.OllamaStatus),
		"PE GET /api/integrations/openai/status":      answerOf(p.OpenAIStatus),
		"PE GET /api/integrations/localai/status":     answerOf(p.LocalAIStatus),
		"PE GET /api/integrations/localai/catalog":    answerOf(p.LocalAICatalog),
		"PE GET /api/integrations/localai/ledger":     answerOf(p.LocalAILedger),
		"PE POST /api/integrations/localai/bootstrap": answerOf(p.LocalAIBootstrap),
		"PE POST /api/integrations/localai/invoke": status(func(_ *http.Request, body map[string]any) (int, map[string]any) {
			return p.LocalAIInvoke(body)
		}),
		"PE POST /api/integrations/completions": status(objectBody("completion", func(_ *http.Request, body map[string]any) (int, map[string]any) {
			return p.Completion(body)
		})),
		"PE POST /api/integrations/healthkit/ingest": status(objectBody("HealthKit ingest", p.HealthKit)),
		"PE POST /api/integrations/carekit/ingest": status(objectBody("CareKit ingest", func(_ *http.Request, body map[string]any) (int, map[string]any) {
			return p.CareKit(body)
		})),
		"PE POST /api/integrations/ollama/dispatch": status(objectBody("Ollama dispatch", func(_ *http.Request, body map[string]any) (int, map[string]any) {
			return p.OllamaDispatch(body)
		})),
		"PE POST /api/integrations/openai/dispatch": status(objectBody("OpenAI dispatch", func(_ *http.Request, body map[string]any) (int, map[string]any) {
			return p.OpenAIDispatch(body)
		})),
		"PE POST /api/integrations/acp/dispatch": status(objectBody("ACP dispatch", func(_ *http.Request, body map[string]any) (int, map[string]any) {
			return p.ACPDispatch(body)
		})),
		"PE GET /api/mqtt/status":   answerOf(p.MQTTStatus),
		"PE GET /api/mqtt/mappings": answerOf(p.MQTTMappings),
		"PE PUT /api/mqtt/mappings": withBody(func(_ *http.Request, body map[string]any) (any, error) {
			return p.ReplaceMQTTMappings(body)
		}),
		"PE POST /api/mqtt/enable": withBody(func(_ *http.Request, body map[string]any) (any, error) {
			return p.EnableMQTT(body)
		}),
		"PE POST /api/mqtt/disable": answerOf(p.DisableMQTT),
		"PE GET /api/triggers/status": http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, p.TriggerStatus())
		}),
		"PE GET /api/dispatch/ledger": http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, p.Ledger())
		}),
		"PE GET /api/dispatch/records/:id": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, err := p.Record(r.PathValue("id"))
			answer(w, body, err)
		}),
		"PE PATCH /api/dispatch/records/:id": withBody(func(r *http.Request, body map[string]any) (any, error) {
			return p.UpdateRecord(r.PathValue("id"), body)
		}),
		"PE POST /api/dispatch/records/:id/replay": withBody(func(r *http.Request, body map[string]any) (any, error) {
			fresh, _ := body["freshIds"].(bool)
			return p.Replay(r.PathValue("id"), fresh)
		}),
		"PE SSE /api/events": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			stream.ServeSSE(w, r, p.Hub())
		}),
		"PE WS /ws": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			stream.ServeWS(w, r, p.Hub(), p.StateUpdate())
		}),
		"PE GET /api/machines": http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			data, err := p.Machines()
			if err != nil {
				writeError(w, err)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			if _, err := w.Write(data); err != nil {
				log.Printf("write machines: %v", err)
			}
		}),
	}
}
