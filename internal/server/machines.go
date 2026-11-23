package server

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"

	"github.com/jateeter/RealityEngine_GO/internal/engine"
)

// writeJSON answers with a JSON body as the voters write it: no HTML escaping
// ("&" stays "&") and no trailing newline. Numbers are in ECMAScript form,
// which encoding/json writes for float64.
func writeJSON(w http.ResponseWriter, status int, v any) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(v); err != nil {
		log.Printf("encode response: %v", err)
		status, buf = http.StatusInternalServerError, *bytes.NewBufferString(`{"error":"response could not be encoded"}` + "\n")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := w.Write(bytes.TrimSuffix(buf.Bytes(), []byte("\n"))); err != nil {
		log.Printf("write response: %v", err)
	}
}

func machineNotFound(w http.ResponseWriter) {
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "Machine not found"})
}

// machineRoutes are the RE's machine read routes (RealityEngine_GO#10).
func machineRoutes(eng *engine.Engine) map[string]http.Handler {
	return map[string]http.Handler{
		"RE GET /api/machines": http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, eng.List())
		}),
		"RE GET /api/machines/:id": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if body, ok := eng.Machine(r.PathValue("id")); ok {
				writeJSON(w, http.StatusOK, body)
				return
			}
			machineNotFound(w)
		}),
		"RE GET /api/machines/:id/export": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if body, ok := eng.Export(r.PathValue("id")); ok {
				writeJSON(w, http.StatusOK, body)
				return
			}
			machineNotFound(w)
		}),
		"RE GET /api/machines/json/list": http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			body, err := eng.Catalog()
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusOK, body)
		}),
	}
}
