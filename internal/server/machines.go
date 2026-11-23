package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"

	"github.com/jateeter/RealityEngine_GO/internal/engine"
	"github.com/jateeter/RealityEngine_GO/internal/machine"
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

// writeError answers a route's error: its status and {"error": message}; an
// error that is not a machine.Error is a 500.
func writeError(w http.ResponseWriter, err error) {
	var ve *engine.ValidationError
	if errors.As(err, &ve) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": ve.Errors})
		return
	}
	var me *machine.Error
	if errors.As(err, &me) {
		writeJSON(w, me.Status, map[string]string{"error": me.Message})
		return
	}
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
}

// answer writes body, or err when there is one.
func answer(w http.ResponseWriter, body any, err error) {
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, body)
}

// maxBody bounds a request body: the largest corpus machine is well under it.
const maxBody = 32 << 20

func readBody(r *http.Request) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		return nil, machine.BadRequest("could not read the request body: %v", err)
	}
	if len(data) > maxBody {
		return nil, &machine.Error{Status: http.StatusRequestEntityTooLarge, Message: "request body is too large"}
	}
	return data, nil
}

// jsonBody is the request body as a JSON object: an empty body is {}, and a
// body that is not an object reads as one with no keys.
func jsonBody(r *http.Request) (map[string]any, error) {
	data, err := readBody(r)
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return map[string]any{}, nil
	}
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, machine.BadRequest("invalid JSON body: %v", err)
	}
	obj, _ := v.(map[string]any)
	if obj == nil {
		obj = map[string]any{}
	}
	return obj, nil
}

// numbers reads an array leniently, as the what-if and -universal routes do:
// a value that is not a number reads 0; a missing key is empty.
func numbers(v any) []float64 {
	l, _ := v.([]any)
	out := make([]float64, len(l))
	for i, x := range l {
		out[i], _ = x.(float64)
	}
	return out
}

// inputEvent reads the input the transition route takes, strictly.
func inputEvent(body map[string]any) ([]float64, error) {
	v, ok := body["inputEvent"]
	if !ok {
		return nil, machine.BadRequest("request must include inputEvent")
	}
	l, ok := v.([]any)
	if !ok {
		return nil, machine.BadRequest("inputEvent must be an array")
	}
	out := make([]float64, len(l))
	for i, x := range l {
		f, ok := x.(float64)
		if !ok {
			return nil, machine.BadRequest("inputEvent values must be finite numbers")
		}
		out[i] = f
	}
	return out, nil
}

// withBody runs f on the request's JSON body, answering a body error itself.
func withBody(f func(r *http.Request, body map[string]any) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := jsonBody(r)
		if err != nil {
			writeError(w, err)
			return
		}
		result, err := f(r, body)
		answer(w, result, err)
	}
}

// withRaw runs f on the request's raw body.
func withRaw(f func(r *http.Request, body []byte) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := readBody(r)
		if err != nil {
			writeError(w, err)
			return
		}
		result, err := f(r, body)
		answer(w, result, err)
	}
}

// machineRoutes are the RE's Machines and Sequences routes (RealityEngine_GO#10).
func machineRoutes(eng *engine.Engine) map[string]http.Handler {
	return map[string]http.Handler{
		"RE POST /api/machines": withRaw(func(_ *http.Request, body []byte) (any, error) {
			return eng.Add(body)
		}),
		"RE PUT /api/machines/:id": withRaw(func(r *http.Request, body []byte) (any, error) {
			return eng.Replace(r.PathValue("id"), body)
		}),
		"RE PATCH /api/machines/:id": withBody(func(r *http.Request, body map[string]any) (any, error) {
			return eng.Patch(r.PathValue("id"), body)
		}),
		"RE DELETE /api/machines/:id": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, err := eng.Delete(r.PathValue("id"))
			answer(w, body, err)
		}),
		"RE GET /api/machines/json/:name": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, err := eng.LoadByName(r.PathValue("name"))
			answer(w, body, err)
		}),
		"RE POST /api/machines/json/import": withRaw(func(_ *http.Request, body []byte) (any, error) {
			return eng.Import(body)
		}),
		"RE POST /api/machines/:id/process": withBody(func(r *http.Request, body map[string]any) (any, error) {
			input, err := inputEvent(body)
			if err != nil {
				return nil, err
			}
			return eng.Process(r.PathValue("id"), input)
		}),
		"RE POST /api/machines/:id/process-universal": withBody(func(r *http.Request, body map[string]any) (any, error) {
			return eng.ProcessUniversal(r.PathValue("id"), numbers(body["universalInputSpace"]))
		}),
		"RE POST /api/machines/process-universal/all": withBody(func(_ *http.Request, body map[string]any) (any, error) {
			return eng.ProcessUniversalAll(numbers(body["universalInputSpace"]))
		}),
		"RE POST /api/machines/:id/whatif": withBody(func(r *http.Request, body map[string]any) (any, error) {
			return eng.WhatIf(r.PathValue("id"), numbers(body["inputEvent"]), false)
		}),
		"RE POST /api/machines/:id/whatif-universal": withBody(func(r *http.Request, body map[string]any) (any, error) {
			return eng.WhatIf(r.PathValue("id"), numbers(body["universalInputSpace"]), true)
		}),
		"RE GET /api/machines/:id/checkpoints": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, eng.Checkpoints(r.PathValue("id")))
		}),
		"RE POST /api/machines/:id/checkpoints": withBody(func(r *http.Request, body map[string]any) (any, error) {
			label, _ := body["label"].(string)
			return eng.Checkpoint(r.PathValue("id"), label)
		}),
		"RE POST /api/machines/:machineId/checkpoints/:cpId/restore": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, err := eng.Restore(r.PathValue("machineId"), r.PathValue("cpId"))
			answer(w, body, err)
		}),
		"RE DELETE /api/machines/:machineId/checkpoints/:cpId": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, eng.DeleteCheckpoint(r.PathValue("machineId"), r.PathValue("cpId")))
		}),
		"RE GET /api/sequences": http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, eng.Sequences())
		}),
		"RE POST /api/sequences": withBody(func(_ *http.Request, body map[string]any) (any, error) {
			return eng.CreateSequence(body)
		}),
		"RE GET /api/sequences/:id": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, err := eng.Sequence(r.PathValue("id"))
			answer(w, body, err)
		}),
		"RE DELETE /api/sequences/:id": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, err := eng.DeleteSequence(r.PathValue("id"))
			answer(w, body, err)
		}),
		"RE POST /api/sequences/:id/reset": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, err := eng.ResetSequence(r.PathValue("id"))
			answer(w, body, err)
		}),
		"RE POST /api/sequences/:id/vectors": withBody(func(r *http.Request, body map[string]any) (any, error) {
			return eng.AddEvent(r.PathValue("id"), body)
		}),
		"RE POST /api/sequences/persist": http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, map[string]any{"success": true})
		}),
		"RE GET /api/buses/semantic": http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			body, err := eng.SemanticBusRegistry()
			answer(w, body, err)
		}),
		"RE GET /api/buses/semantic/:id": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, err := eng.SemanticBus(r.PathValue("id"))
			answer(w, body, err)
		}),
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
