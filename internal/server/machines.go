package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/jateeter/RealityEngine_GO/internal/engine"
	"github.com/jateeter/RealityEngine_GO/internal/machine"
	"github.com/jateeter/RealityEngine_GO/internal/pe"
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
	var pErr *pe.Error
	if errors.As(err, &pErr) {
		writeJSON(w, pErr.Status, map[string]string{"error": pErr.Message})
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

// clearAudit is the reset's opt-in flag, from the query string ("true" or
// "1") or, without one, a JSON body's boolean clearAudit. A malformed body is
// not a reason to refuse a reset: the flag keeps its default, false.
func clearAudit(r *http.Request) bool {
	if q := r.URL.Query(); q.Has("clearAudit") {
		v := q.Get("clearAudit")
		return v == "true" || v == "1"
	}
	data, err := readBody(r)
	if err != nil {
		return false
	}
	var body map[string]any
	if json.Unmarshal(data, &body) != nil {
		return false
	}
	flag, _ := body["clearAudit"].(bool)
	return flag
}

// queryValue is a query parameter, nil when absent.
func queryValue(r *http.Request, key string) *string {
	q := r.URL.Query()
	if !q.Has(key) {
		return nil
	}
	v := q.Get(key)
	return &v
}

// trajectory serves an ISRE or OSRE history: from and limit are integers,
// negative read as 0.
func trajectory(eng *engine.Engine, isre bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var bounds [2]int
		for i, key := range []string{"from", "limit"} {
			raw := r.URL.Query().Get(key)
			if raw == "" {
				continue
			}
			n, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				writeError(w, machine.BadRequest("%s must be an integer", key))
				return
			}
			bounds[i] = int(max(n, 0))
		}
		writeJSON(w, http.StatusOK, eng.Trajectory(isre, bounds[0], bounds[1]))
	}
}

// root is GET / and GET /api: what is answering.
var root = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"name": "Reality Engine", "version": version + "-go", "status": "running"})
})

func demo(eng *engine.Engine, name, file string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		body, err := eng.Demo(name, file)
		answer(w, body, err)
	}
}

// machineRoutes are the RE's Machines and Sequences routes (RealityEngine_GO#10)
// and its engine routes (#13).
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
		"RE POST /api/engine/process": withBody(func(_ *http.Request, body map[string]any) (any, error) {
			return eng.ProcessAll(numbers(body["vector"])), nil
		}),
		"RE POST /api/engine/reset": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, eng.Reset(clearAudit(r)))
		}),
		"RE GET /api/engine/history": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
			writeJSON(w, http.StatusOK, eng.History(limit))
		}),
		"RE GET /api/engine/stats": http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, eng.Stats())
		}),
		"RE GET /api/engine/active": http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, eng.Active())
		}),
		"RE GET /api/engine/clock": http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, eng.Clock())
		}),
		"RE POST /api/perceive": withBody(func(_ *http.Request, body map[string]any) (any, error) {
			return eng.Perceive(body)
		}),
		"RE POST /api/perception/observe": withBody(func(_ *http.Request, body map[string]any) (any, error) {
			return map[string]any{
				"success": true, "inputEvent": numbers(body["data"]), "transformations": []any{},
				"processingTimestamp": time.Now().UnixMilli(),
			}, nil
		}),
		"RE GET /api/engine/isre-history": trajectory(eng, true),
		"RE GET /api/engine/osre-history": trajectory(eng, false),
		"RE GET /api/engine/steps/:n/pair": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n, err := engine.StepNumber(r.PathValue("n"))
			if err != nil {
				writeError(w, err)
				return
			}
			timeout, err := engine.PairTimeout(queryValue(r, "timeoutMs"))
			if err != nil {
				writeError(w, err)
				return
			}
			body, err := eng.StepPair(n, timeout)
			answer(w, body, err)
		}),
		"RE GET /api/arbitration": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, err := eng.Arbitration(queryValue(r, "step"))
			answer(w, body, err)
		}),
		"RE GET /api/engine/config": http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, eng.Controls())
		}),
		"RE GET /api/engine/config/:control": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, err := eng.Control(r.PathValue("control"))
			answer(w, body, err)
		}),
		"RE PUT /api/engine/config/:control": withBody(func(r *http.Request, body map[string]any) (any, error) {
			return eng.SetControl(r.PathValue("control"), body)
		}),
		"RE DELETE /api/engine/config/:control": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, err := eng.ResetControl(r.PathValue("control"))
			answer(w, body, err)
		}),
		"RE GET /api/runtime/options": http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, eng.RuntimeOptions())
		}),
		"RE PATCH /api/runtime/options": withBody(func(_ *http.Request, body map[string]any) (any, error) {
			return eng.PatchRuntimeOptions(body)
		}),
		"RE GET /api/runtime/metrics": http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, eng.RuntimeMetrics())
		}),
		"RE GET /api/runtime/vector-space": http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, eng.VectorSpace())
		}),
		"RE GET /api/runtime/storage-footprint": http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, eng.StorageFootprint())
		}),
		"RE POST /api/perception/diagnostic": withBody(func(_ *http.Request, body map[string]any) (any, error) {
			return eng.Diagnostic(numbers(body["universalInputSpace"]))
		}),
		"RE GET /":    root,
		"RE GET /api": root,
		"RE GET /api/config": http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, eng.Config())
		}),
		"RE PUT /api/config/dimension": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, err := eng.SetDimension(queryValue(r, "dimension"))
			answer(w, body, err)
		}),
		"RE PUT /api/config/threshold": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, err := eng.SetThreshold(queryValue(r, "threshold"))
			answer(w, body, err)
		}),
		"RE POST /api/vectors": withRaw(func(_ *http.Request, body []byte) (any, error) {
			return eng.StoreVector(body)
		}),
		"RE POST /api/vectors/search": withRaw(func(_ *http.Request, body []byte) (any, error) {
			return eng.SearchVectors(body)
		}),
		"RE GET /api/vectors/:id": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, err := eng.Vector(r.PathValue("id"))
			answer(w, body, err)
		}),
		"RE DELETE /api/vectors/:id": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, eng.DeleteVector(r.PathValue("id")))
		}),
		"RE GET /api/machine-graph": http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, eng.MachineGraph())
		}),
		"RE GET /api/governance/route": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, err := eng.GovernanceRoute(queryValue(r, "machineId"), queryValue(r, "sequenceId"), queryValue(r, "values"))
			answer(w, body, err)
		}),
		"RE GET /api/demo/multi-step":  demo(eng, "Multi-Step State Machine", "MultiStep.json"),
		"RE GET /api/demo/data-center": demo(eng, "Data Center Monitoring", "DataCenterMonitoring.json"),
		"RE GET /api/demo/kleene-star": demo(eng, "Kleene Star Operator", "KleeneStar.json"),
		"RE POST /api/perceptual-simulation/configure/chunk": withBody(func(_ *http.Request, body map[string]any) (any, error) {
			return eng.ConfigureChunk(body), nil
		}),
		"RE POST /api/perceptual-simulation/configure/commit": http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			body, err := eng.ConfigureCommit()
			answer(w, body, err)
		}),
		"RE POST /api/perceptual-simulation/step": http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			body, err := eng.SimulationStep()
			answer(w, body, err)
		}),
		"RE POST /api/perceptual-simulation/start": http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			body, err := eng.SimulationStart()
			answer(w, body, err)
		}),
		"RE POST /api/perceptual-simulation/stop": http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, eng.SimulationStop())
		}),
		"RE POST /api/perceptual-simulation/reset": http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, eng.SimulationReset())
		}),
		"RE GET /api/perceptual-simulation/state": http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, eng.SimulationState())
		}),
		"RE GET /api/perceptual-simulation/history": http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, eng.SimulationHistory())
		}),
		"RE POST /api/sampler/start": withBody(func(_ *http.Request, body map[string]any) (any, error) {
			return eng.SamplerStart(body), nil
		}),
		"RE POST /api/sampler/stop": http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, eng.SamplerStop())
		}),
		"RE POST /api/sampler/sample": withBody(func(_ *http.Request, body map[string]any) (any, error) {
			return eng.SamplerSample(body), nil
		}),
		"RE GET /api/sampler/stats": http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, eng.SamplerStats())
		}),
		"RE GET /api/metrics": http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
			if _, err := w.Write([]byte(eng.Metrics())); err != nil {
				log.Printf("write metrics: %v", err)
			}
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
