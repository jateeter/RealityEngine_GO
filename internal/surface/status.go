package surface

import (
	"fmt"
	"sort"
	"strings"
)

// Implemented names, by route id, every specified route this runtime serves.
// Listing a route here establishes presence, not behavioral parity: agreement
// with the voters is RealityEngine_CI's follower lane (#12).
var Implemented = map[string]bool{
	"RE GET /api/health": true,
	"PE GET /api/health": true,
	// PE info, state, config, reset, sources, sensors, signals and the
	// machines proxy (#8).
	"PE GET /":                                     true,
	"PE GET /api/state":                            true,
	"PE PATCH /api/config":                         true,
	"PE POST /api/reset":                           true,
	"PE GET /api/sources":                          true,
	"PE POST /api/sources":                         true,
	"PE PATCH /api/sources/:id":                    true,
	"PE DELETE /api/sources/:id":                   true,
	"PE POST /api/sources/bootstrap-from-machines": true,
	"PE GET /api/sources/contention":               true,
	"PE POST /api/sensors/:sensorId":               true,
	"PE POST /api/signals":                         true,
	"PE GET /api/machines":                         true,
	// The push cycle and the event streams (#11).
	"PE POST /api/push":         true,
	"PE GET /api/push/:id":      true,
	"PE POST /api/auto/start":   true,
	"PE POST /api/auto/stop":    true,
	"PE SSE /api/events":        true,
	"PE WS /ws":                 true,
	"RE SSE /api/engine/stream": true,
	// Dispatch, triggers and integrations (#3).
	"PE GET /api/integrations/status":             true,
	"PE POST /api/integrations/completions":       true,
	"PE GET /api/integrations/ollama/status":      true,
	"PE POST /api/integrations/ollama/dispatch":   true,
	"PE GET /api/integrations/openai/status":      true,
	"PE POST /api/integrations/openai/dispatch":   true,
	"PE GET /api/integrations/acp/status":         true,
	"PE POST /api/integrations/acp/dispatch":      true,
	"PE GET /api/integrations/healthkit/status":   true,
	"PE POST /api/integrations/healthkit/ingest":  true,
	"PE GET /api/integrations/carekit/status":     true,
	"PE POST /api/integrations/carekit/ingest":    true,
	"PE GET /api/integrations/localai/status":     true,
	"PE GET /api/integrations/localai/catalog":    true,
	"PE POST /api/integrations/localai/bootstrap": true,
	"PE POST /api/integrations/localai/invoke":    true,
	"PE GET /api/integrations/localai/ledger":     true,
	"PE GET /api/dispatch/ledger":                 true,
	"PE GET /api/dispatch/records/:id":            true,
	"PE PATCH /api/dispatch/records/:id":          true,
	"PE POST /api/dispatch/records/:id/replay":    true,
	"PE GET /api/triggers/status":                 true,
	"PE GET /api/mqtt/status":                     true,
	"PE GET /api/mqtt/mappings":                   true,
	"PE PUT /api/mqtt/mappings":                   true,
	"PE POST /api/mqtt/enable":                    true,
	"PE POST /api/mqtt/disable":                   true,
	"RE GET /api/machines":                        true,
	"RE GET /api/machines/:id":                    true,
	"RE GET /api/machines/:id/export":             true,
	"RE GET /api/machines/json/list":              true,
	// Machine writes, transitions and checkpoints (#10).
	"RE POST /api/machines":                                      true,
	"RE PUT /api/machines/:id":                                   true,
	"RE PATCH /api/machines/:id":                                 true,
	"RE DELETE /api/machines/:id":                                true,
	"RE GET /api/machines/json/:name":                            true,
	"RE POST /api/machines/json/import":                          true,
	"RE POST /api/machines/:id/process":                          true,
	"RE POST /api/machines/:id/process-universal":                true,
	"RE POST /api/machines/process-universal/all":                true,
	"RE POST /api/machines/:id/whatif":                           true,
	"RE POST /api/machines/:id/whatif-universal":                 true,
	"RE GET /api/machines/:id/checkpoints":                       true,
	"RE POST /api/machines/:id/checkpoints":                      true,
	"RE POST /api/machines/:machineId/checkpoints/:cpId/restore": true,
	"RE DELETE /api/machines/:machineId/checkpoints/:cpId":       true,
	"RE GET /api/buses/semantic":                                 true,
	"RE GET /api/buses/semantic/:id":                             true,
	// The engine's sequence store (#10).
	"RE GET /api/sequences":              true,
	"RE POST /api/sequences":             true,
	"RE GET /api/sequences/:id":          true,
	"RE DELETE /api/sequences/:id":       true,
	"RE POST /api/sequences/:id/reset":   true,
	"RE POST /api/sequences/:id/vectors": true,
	"RE POST /api/sequences/persist":     true,
	// Engine-wide processing, its audit trail and the clock (#13, slice 1).
	"RE POST /api/engine/process": true,
	"RE POST /api/engine/reset":   true,
	"RE GET /api/engine/history":  true,
	"RE GET /api/engine/stats":    true,
	"RE GET /api/engine/active":   true,
	"RE GET /api/engine/clock":    true,
	// The step path and arbitration (#13, slice 2).
	"RE POST /api/perceive":            true,
	"RE POST /api/perception/observe":  true,
	"RE GET /api/engine/isre-history":  true,
	"RE GET /api/engine/osre-history":  true,
	"RE GET /api/engine/steps/:n/pair": true,
	"RE GET /api/arbitration":          true,
	// The control pathway, the runtime views and the diagnostic (#13, slice 3).
	"RE GET /api/engine/config":             true,
	"RE GET /api/engine/config/:control":    true,
	"RE PUT /api/engine/config/:control":    true,
	"RE DELETE /api/engine/config/:control": true,
	"RE GET /api/runtime/options":           true,
	"RE PATCH /api/runtime/options":         true,
	"RE GET /api/runtime/metrics":           true,
	"RE GET /api/runtime/vector-space":      true,
	"RE GET /api/runtime/storage-footprint": true,
	"RE POST /api/perception/diagnostic":    true,
	// Configuration, vectors, the machine graph, governance and demos (#2,
	// slice A).
	"RE GET /":                     true,
	"RE GET /api":                  true,
	"RE GET /api/config":           true,
	"RE PUT /api/config/dimension": true,
	"RE PUT /api/config/threshold": true,
	"RE POST /api/vectors":         true,
	"RE POST /api/vectors/search":  true,
	"RE GET /api/vectors/:id":      true,
	"RE DELETE /api/vectors/:id":   true,
	"RE GET /api/machine-graph":    true,
	"RE GET /api/governance/route": true,
	"RE GET /api/demo/multi-step":  true,
	"RE GET /api/demo/data-center": true,
	"RE GET /api/demo/kleene-star": true,
	// Perceptual simulation and the sampler (#2, slice B).
	"RE POST /api/perceptual-simulation/configure/chunk":  true,
	"RE POST /api/perceptual-simulation/configure/commit": true,
	"RE POST /api/perceptual-simulation/start":            true,
	"RE POST /api/perceptual-simulation/stop":             true,
	"RE POST /api/perceptual-simulation/step":             true,
	"RE POST /api/perceptual-simulation/reset":            true,
	"RE GET /api/perceptual-simulation/state":             true,
	"RE GET /api/perceptual-simulation/history":           true,
	"RE POST /api/sampler/start":                          true,
	"RE POST /api/sampler/stop":                           true,
	"RE POST /api/sampler/sample":                         true,
	"RE GET /api/sampler/stats":                           true,
	// CES coverage and the runtime gauges (#2, slice C).
	"RE GET /api/metrics": true,
}

// groupIssues assigns every route group to the RealityEngine_GO issue that
// implements it. A route not yet implemented must have an issue (#7).
var groupIssues = map[string]map[string]int{
	RE: {
		"Info & Health":         2,
		"Configuration":         2,
		"Demos":                 2,
		"Governance":            2,
		"Machine Graph":         2,
		"Perceptual Simulation": 2,
		"Sampler":               2,
		"Vectors":               2,
		"Machines":              10,
		"Sequences":             10,
		"Engine":                13,
		"Perception":            13,
		"Runtime Introspection": 13,
		"Cloneable machines":    5,
		"Streaming":             11,
	},
	REPhase2: {
		"Routes": 17,
	},
	PE: {
		"Info & Health":         8,
		"Configuration & Reset": 8,
		"Sources & Sensors":     8,
		"Signals":               8,
		"Machines Proxy":        8,
		"Push Cycle":            11,
		"Streaming":             11,
		"Dispatch & Triggers":   3,
		"Integrations":          3,
		"MQTT Bridge":           3,
	},
}

// Issue is the RealityEngine_GO issue that implements a route, or 0.
func Issue(r Route) int { return groupIssues[r.Layer][r.Group] }

// RouteStatus is one route's state in this runtime.
type RouteStatus struct {
	Route
	Status string `json:"status"` // "implemented" or "unsupported"
	Issue  int    `json:"issue,omitempty"`
}

// Statuses lists every route with its state. It fails when an unimplemented
// route has no issue, or Implemented names a route the inventory lacks.
func Statuses(inv Inventory) ([]RouteStatus, error) {
	known := map[string]bool{}
	var out []RouteStatus
	var missing []string
	for _, r := range inv.Routes {
		known[r.ID] = true
		s := RouteStatus{Route: r, Status: "unsupported", Issue: Issue(r)}
		if Implemented[r.ID] {
			s.Status = "implemented"
		} else if s.Issue == 0 {
			missing = append(missing, r.ID+" ("+r.Group+")")
		}
		out = append(out, s)
	}
	var stale []string
	for id := range Implemented {
		if !known[id] {
			stale = append(stale, id)
		}
	}
	sort.Strings(stale)
	switch {
	case len(missing) > 0:
		return nil, fmt.Errorf("unimplemented routes without an issue (add the group to groupIssues): %v", missing)
	case len(stale) > 0:
		return nil, fmt.Errorf("Implemented names routes the specification no longer has: %v", stale)
	}
	return out, nil
}

// StatusMarkdown renders conformance/SURFACE_STATUS.md.
func StatusMarkdown(inv Inventory, statuses []RouteStatus) string {
	var b strings.Builder
	implemented := 0
	for _, s := range statuses {
		if s.Status == "implemented" {
			implemented++
		}
	}
	fmt.Fprintf(&b, "# Surface status\n\n")
	fmt.Fprintf(&b, "Generated by `go run ./cmd/refresh-surface`; never edit by hand.\n\n")
	fmt.Fprintf(&b, "Specification: `%s` `%s` at commit `%s` (sha256 `%s`). Routes digest `%s`.\n\n",
		inv.Source.Repository, inv.Source.Path, inv.Source.Commit, inv.Source.Sha256, inv.RoutesSha256)
	fmt.Fprintf(&b, "**%d of %d specified routes are implemented.** Every other route answers `501` with "+
		"`participation: \"unsupported\"` and names the issue that implements it. "+
		"\"Implemented\" establishes presence, not behavioral parity with the voters.\n\n", implemented, len(statuses))
	layers := []string{RE, REPhase2, PE}
	for _, layer := range layers {
		fmt.Fprintf(&b, "## %s\n\n| Route | Group | Status | Issue |\n|---|---|---|---|\n", layer)
		for _, s := range statuses {
			if s.Layer != layer {
				continue
			}
			issue := ""
			if s.Issue != 0 {
				issue = fmt.Sprintf("#%d", s.Issue)
			}
			fmt.Fprintf(&b, "| `%s %s` | %s | %s | %s |\n", s.Method, s.Path, s.Group, s.Status, issue)
		}
		b.WriteString("\n")
	}
	return b.String()
}
