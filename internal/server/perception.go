package server

import (
	"log"
	"net/http"

	"github.com/jateeter/RealityEngine_GO/internal/pe"
	"github.com/jateeter/RealityEngine_GO/internal/stream"
)

// peRoutes are the PE's routes (RealityEngine_GO#8): info, state,
// configuration and layer-local reset, sources and sensors, signals and the
// RE machines proxy.
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
		"PE POST /api/signals": withBody(func(_ *http.Request, body map[string]any) (any, error) {
			out, err := p.Signal(body)
			if err != nil {
				return nil, err
			}
			// triggerPush runs a push now and answers it beside the signal.
			if trigger, _ := body["triggerPush"].(bool); trigger {
				compact, _ := body["compactPush"].(bool)
				_, out["push"] = p.Push(!compact, false, nil)
			}
			p.BroadcastState()
			return out, nil
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
