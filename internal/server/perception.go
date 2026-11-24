package server

import (
	"log"
	"net/http"

	"github.com/jateeter/RealityEngine_GO/internal/pe"
)

// peRoutes are the PE's routes (RealityEngine_GO#8): info, state,
// configuration and layer-local reset, sources and sensors, signals and the
// RE machines proxy.
func peRoutes(p *pe.PE) map[string]http.Handler {
	return map[string]http.Handler{
		"PE GET /": http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, map[string]any{"service": "Perception Engine (Go)", "status": "running"})
		}),
		"PE GET /api/state": http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, p.State())
		}),
		"PE PATCH /api/config": withBody(func(_ *http.Request, body map[string]any) (any, error) {
			return p.Configure(body)
		}),
		"PE POST /api/reset": http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, p.Reset())
		}),
		"PE GET /api/sources": http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, p.Sources())
		}),
		"PE POST /api/sources": withBody(func(_ *http.Request, body map[string]any) (any, error) {
			return p.CreateSource(body)
		}),
		"PE PATCH /api/sources/:id": withBody(func(r *http.Request, body map[string]any) (any, error) {
			return p.UpdateSource(r.PathValue("id"), body)
		}),
		"PE DELETE /api/sources/:id": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, err := p.DeleteSource(r.PathValue("id"))
			answer(w, body, err)
		}),
		"PE POST /api/sources/bootstrap-from-machines": http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, p.BootstrapFromRE())
		}),
		"PE GET /api/sources/contention": http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, p.Contention())
		}),
		"PE POST /api/sensors/:sensorId": withBody(func(r *http.Request, body map[string]any) (any, error) {
			return p.Sensor(r.PathValue("sensorId"), body)
		}),
		"PE POST /api/signals": withBody(func(_ *http.Request, body map[string]any) (any, error) {
			return p.Signal(body)
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
