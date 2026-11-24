// Package pe is the Perception Engine (RealityEngine_GO#8): it holds sources,
// assembles the universal Reality Event from the active ones, and drives its
// Reality Engine with it. SURFACE_SPEC.md's PE sections are the contract; C++
// perception_engine_server.cpp is the concrete form, and Rust's src/pe prior
// art.
//
//   - GET /api/state is the sources in (name, id) order and the vector they
//     assemble now.
//   - POST /api/reset is layer-local (SURFACE_SPEC "Reset is layer-local"): it
//     clears this PE's run state, revalidates each source's activity, and
//     never touches the RE or the source set.
//   - At boot the PE interns its RE's machines as test sources unless
//     PE_SOURCE_BOOTSTRAP says not to (SURFACE_SPEC "Machine ingestion").
package pe

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jateeter/RealityEngine_GO/internal/engine"
	"github.com/jateeter/RealityEngine_GO/internal/stream"
)

// Error is a refused request: its status and message.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return e.Message }

const autoIntervalMS = 1000

type contention struct {
	transition uint64
	cells      []any
	folds      []any
	counters   map[string][2]uint64 // source id -> (contended, suppressed)
}

func newContention() contention {
	return contention{cells: []any{}, folds: []any{}, counters: map[string][2]uint64{}}
}

// PE is the Perception Engine's state, under one lock.
type PE struct {
	mu             sync.RWMutex
	dimension      int
	globalStep     uint64
	persistent     []float64
	matchAlgorithm string
	lastPush       any
	reURL          string
	sources        map[string]*Source
	testStep       map[string]int
	contention     contention
	mergeOnly      bool
	rng            uint64
	walk           map[string][]float64
	autoRunning    bool
	autoIntervalMS int
	client         *http.Client
	// osreFold is the last push's mergeBatch cells: writing machine name and
	// its outputMergeTransformation.
	osreFold map[int64][2]string
	registry engine.Registry
	catalog  map[string]map[string]any
	hub      *stream.Hub
	pushes   *pushes
	// catalogLoaded is false until the RE's machines were first read.
	catalogLoaded      bool
	catalogRefreshedAt int64
	dispatch           dispatchState
	localAIBase        string
	integrations       *Integrations
	localAI            *localAILedger
	mqttMu             sync.Mutex
	mqtt               *mqttBridge
}

func nowMS() int64 { return time.Now().UnixMilli() }

// New is a PE of the given dimension driving the RE at reURL.
func New(dimension int, reURL string) *PE {
	p := &PE{
		dimension: dimension, persistent: make([]float64, dimension), matchAlgorithm: "gte",
		reURL: strings.TrimSuffix(reURL, "/"), sources: map[string]*Source{}, testStep: map[string]int{},
		contention: newContention(), mergeOnly: true, rng: uint64(nowMS()) | 1, walk: map[string][]float64{},
		autoIntervalMS: autoIntervalMS, client: &http.Client{Timeout: 120 * time.Second},
		osreFold: map[int64][2]string{}, catalog: map[string]map[string]any{}, hub: stream.NewHub(), pushes: newPushes(),
	}
	p.integrations = integrationsFromEnvironment()
	p.localAI = &localAILedger{}
	p.localAIBase = strings.TrimSuffix(p.integrations.localAIBaseURL, "/")
	p.dispatch = newDispatch(p.localAIBase)
	return p
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// flagOn reads PE_SOURCE_BOOTSTRAP / PE_SOURCE_MERGE: on unless off, 0,
// false or no.
func flagOn(v string, set bool) bool {
	if !set {
		return true
	}
	switch v {
	case "off", "OFF", "0", "false", "FALSE", "no", "NO":
		return false
	}
	return true
}

// FromEnvironment is the PE VECTOR_DIMENSION (default 7680) and RE_URL
// (default http://localhost:5851, the Go RE's reserved port) describe,
// bootstrapped from its RE unless PE_SOURCE_BOOTSTRAP is off. clientTLS, when
// the lane is on (RE_TLS_*), is every outbound HTTPS call's configuration: the
// instance certificate, the lane CA and its CRL.
func FromEnvironment(clientTLS *tls.Config) *PE {
	dimension := 7680
	if v, err := strconv.Atoi(os.Getenv("VECTOR_DIMENSION")); err == nil && v >= 0 {
		dimension = v
	}
	reURL := os.Getenv("RE_URL")
	if reURL == "" {
		reURL = "http://localhost:5851"
	}
	p := New(dimension, reURL)
	if clientTLS != nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.TLSClientConfig = clientTLS
		p.client.Transport = transport
	}
	machinesDir := os.Getenv("MACHINES_DIR")
	if machinesDir == "" {
		machinesDir = "../RealityEngine_Machines/machines"
	}
	// The fold applies a cell's declared rule, so the PE loads the registry
	// the RE loads.
	p.registry = engine.LoadRegistry(machinesDir)
	if v, ok := os.LookupEnv("PE_SOURCE_MERGE"); ok {
		p.mergeOnly = flagOn(v, true)
	}
	log.Printf("perception-engine: perceptionDimension %d, RE %s", dimension, p.reURL)
	// The catalog is a cache of the RE's machines; reading it changes no
	// source membership, so it is read whatever PE_SOURCE_BOOTSTRAP says.
	p.refreshCatalog()
	v, set := os.LookupEnv("PE_SOURCE_BOOTSTRAP")
	if flagOn(v, set) {
		summary := p.BootstrapFromRE()
		data, _ := json.Marshal(summary)
		log.Printf("perception-engine: boot bootstrap %s", data)
	}
	p.startMQTTFromEnvironment()
	return p
}

// getRE reads a route of this PE's RE.
func (p *PE) getRE(path string) ([]byte, error) {
	url := p.reURL + path
	resp, err := p.client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("GET %s: HTTP %s", url, resp.Status)
	}
	return body, nil
}

// State is GET /api/state.
func (p *PE) State() map[string]any {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return map[string]any{
		"sources": p.sourcesJSON(), "assembledVector": p.assemble(), "globalStep": p.globalStep,
		"auto":     map[string]any{"running": p.autoRunning, "intervalMs": p.autoIntervalMS},
		"lastPush": p.lastPush, "matchAlgorithm": p.matchAlgorithm, "perceptionDimension": p.dimension,
	}
}

// Configure is PATCH /api/config: matchAlgorithm gte or equals.
func (p *PE) Configure(body map[string]any) (map[string]any, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if name, ok := body["matchAlgorithm"].(string); ok {
		if name != "gte" && name != "equals" {
			return nil, &Error{500, "Unknown match algorithm: " + name}
		}
		p.matchAlgorithm = name
	}
	return map[string]any{"success": true, "matchAlgorithm": p.matchAlgorithm}, nil
}

// Reset is POST /api/reset: this PE's run state only.
func (p *PE) Reset() map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.globalStep = 0
	p.persistent = make([]float64, p.dimension)
	p.lastPush = nil
	p.resetSources()
	return map[string]any{"success": true}
}

// Sources is GET /api/sources.
func (p *PE) Sources() map[string]any {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return map[string]any{"sources": p.sourcesJSON()}
}

// CreateSource is POST /api/sources.
func (p *PE) CreateSource(body map[string]any) (map[string]any, error) {
	s, err := sourceFrom(body)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	added := p.addSource(s)
	if b, ok := body["active"].(bool); ok && !b {
		added.Active = false
	}
	return map[string]any{"source": added.json(nowMS())}, nil
}

// UpdateSource is PATCH /api/sources/:id.
func (p *PE) UpdateSource(id string, body map[string]any) (map[string]any, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	existing, ok := p.sources[id]
	if !ok {
		return nil, &Error{404, "Source not found"}
	}
	updated, err := existing.patched(body)
	if err != nil {
		return nil, err
	}
	updated.ID = id
	added := p.addSource(updated)
	if b, ok := body["active"].(bool); ok && !b {
		added.Active = false
	}
	return map[string]any{"source": added.json(nowMS())}, nil
}

// DeleteSource is DELETE /api/sources/:id.
func (p *PE) DeleteSource(id string) (map[string]any, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.removeSource(id) {
		return nil, &Error{404, "Source not found"}
	}
	return map[string]any{"success": true}, nil
}

// Contention is GET /api/sources/contention: what the last push recorded.
func (p *PE) Contention() map[string]any {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.contentionJSON()
}

// Sensor is POST /api/sensors/:sensorId.
func (p *PE) Sensor(sensorID string, body map[string]any) (map[string]any, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.updateSensor(sensorID, numbers(body["values"])) {
		return nil, &Error{404, fmt.Sprintf("No sensor source with sensorId %q", sensorID)}
	}
	return map[string]any{"success": true, "sensorId": sensorID, "timestamp": nowMS()}, nil
}

// BootstrapFromRE is POST /api/sources/bootstrap-from-machines: the RE's
// machines interned as test sources.
func (p *PE) BootstrapFromRE() map[string]any {
	data, err := p.getRE("/api/machines")
	if err != nil {
		return bootstrapFailed(err)
	}
	var listing map[string]any
	if err := json.Unmarshal(data, &listing); err != nil {
		return bootstrapFailed(err)
	}
	p.mu.Lock()
	p.cacheCatalog(listing)
	summary := p.bootstrap(listing)
	p.mu.Unlock()
	p.BroadcastState()
	return summary
}

// Machines is GET /api/machines: the RE's listing, passed through as it came.
func (p *PE) Machines() ([]byte, error) {
	data, err := p.getRE("/api/machines")
	if err != nil {
		return nil, &Error{502, err.Error()}
	}
	var listing map[string]any
	if json.Unmarshal(data, &listing) == nil {
		p.mu.Lock()
		p.cacheCatalog(listing)
		p.mu.Unlock()
	}
	return data, nil
}
