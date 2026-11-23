package engine

import (
	"encoding/json"
	"log"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

// The arbiter (docs/ARBITER_CONTRACT.md): a cell one machine writes is stored
// as written; a contended cell resolves under the rule
// domains/arbitration-registry.json declares for it, else PRECEDENCE, and the
// resolution is recorded. C++'s resolve_cell is the concrete form; Rust's
// step.rs is prior art.

type arbitrationEntry struct {
	rule          string
	withinRank    string
	providerRanks map[string]float64
}

type arbitrationRegistry struct {
	entries map[int]arbitrationEntry
	source  string // the file the entries came from; empty when none was found
}

// loadArbitrationRegistry reads ARBITRATION_REGISTRY, else the registry
// beside the corpus, as C++ looks.
func loadArbitrationRegistry(machinesDir string) arbitrationRegistry {
	reg := arbitrationRegistry{entries: map[int]arbitrationEntry{}}
	var candidates []string
	if p := os.Getenv("ARBITRATION_REGISTRY"); p != "" {
		candidates = append(candidates, p)
	}
	if machinesDir != "" {
		// Joined as written, not cleaned: registrySource reports the path as
		// found, which is how C++ spells it.
		candidates = append(candidates,
			strings.TrimSuffix(machinesDir, "/")+"/../domains/arbitration-registry.json",
			strings.TrimSuffix(machinesDir, "/")+"/domains/arbitration-registry.json")
	}
	candidates = append(candidates, filepath.Join("..", "RealityEngine_Machines", "domains", "arbitration-registry.json"))
	found := ""
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && st.Mode().IsRegular() {
			found = c
			break
		}
	}
	if found == "" {
		log.Printf("[arbiter] no arbitration registry found; contended cells fall back to PRECEDENCE")
		return reg
	}
	data, err := os.ReadFile(found)
	var doc struct {
		Entries []map[string]any `json:"entries"`
	}
	if err == nil {
		err = json.Unmarshal(data, &doc)
	}
	if err != nil {
		log.Printf("[arbiter] failed to read %s", found)
		return reg
	}
	for _, e := range doc.Entries {
		cell, ok := e["cell"].(float64)
		if !ok || cell < 0 || cell != math.Trunc(cell) {
			continue
		}
		entry := arbitrationEntry{rule: "PRECEDENCE", providerRanks: map[string]float64{}}
		if r, ok := e["rule"].(string); ok {
			entry.rule = r
		}
		entry.withinRank, _ = e["withinRank"].(string)
		if ranks, ok := e["providerRanks"].(map[string]any); ok {
			for p, r := range ranks {
				if f, ok := r.(float64); ok && f == math.Trunc(f) {
					entry.providerRanks[p] = f
				}
			}
		}
		reg.entries[int(cell)] = entry
	}
	reg.source = found
	log.Printf("[arbiter] loaded %d contended cell declaration(s) from %s", len(reg.entries), found)
	return reg
}

// contribution is one contestant for a contended cell.
type contribution struct {
	value          float64
	provider       string
	originID       string // the machine id, or a PE source id
	cesID          string // the contributing sequences, comma-joined
	outputVectorID string // "0" for a machine
	ragStatusCode  string
}

// arbitrationRecord is how one contended cell resolved (ARBITER_CONTRACT §6).
type arbitrationRecord struct {
	instant      int
	cell         int
	rule         string
	resolved     float64
	contributors []contribution
	suppressed   []contribution
}

// severityRank: GREEN or absent 0 < AMBER 1 < RED 2.
func severityRank(rag string) float64 {
	switch rag {
	case "RED":
		return 2
	case "AMBER":
		return 1
	}
	return 0
}

// determinism is a provider's determinism class. An unregistered provider is
// generated: misclassifying generated content as measured would let it
// outrank a reading.
func determinism(provider string) string {
	switch provider {
	case "machine":
		return "deterministic"
	case "sensor", "mqtt", "healthkit", "stream", "ui", "synthetic":
		return "measured"
	}
	return "generated"
}

func determinismRank(provider string) float64 {
	switch determinism(provider) {
	case "deterministic":
		return 3
	case "measured":
		return 2
	}
	return 1
}

// pick is the contestants holding the best score, in their original order.
func pick(cs []contribution, score func(contribution) float64, highest bool) []contribution {
	best, have := 0.0, false
	for _, c := range cs {
		s := score(c)
		if !have || (highest && s > best) || (!highest && s < best) {
			best, have = s, true
		}
	}
	var out []contribution
	for _, c := range cs {
		if score(c) == best {
			out = append(out, c)
		}
	}
	return out
}

// resolveCell applies the declared rule to a contended cell's contestants.
// One contestant resolves to itself and needs no record.
func resolveCell(cell, instant int, cs []contribution, entry *arbitrationEntry) (float64, *arbitrationRecord) {
	if len(cs) == 1 {
		return cs[0].value, nil
	}
	rule := "PRECEDENCE"
	if entry != nil {
		rule = entry.rule
	}
	value := func(c contribution) float64 { return c.value }
	severity := func(c contribution) float64 { return severityRank(c.ragStatusCode) }
	var winners []contribution
	switch rule {
	case "OR", "MAX":
		winners = pick(cs, value, true)
	case "AND", "MIN":
		winners = pick(cs, value, false)
	case "SEVERITY":
		winners = pick(pick(cs, severity, true), value, true)
	case "MEAN":
		// Floating-point addition is not associative: the sum runs in the
		// canonical order, so every runtime adds the same terms the same way.
		ordered := append([]contribution{}, cs...)
		sort.SliceStable(ordered, func(i, j int) bool {
			a, b := ordered[i], ordered[j]
			if a.originID != b.originID {
				return a.originID < b.originID
			}
			if a.cesID != b.cesID {
				return a.cesID < b.cesID
			}
			return a.outputVectorID < b.outputVectorID
		})
		sum := 0.0
		for _, c := range ordered {
			sum += c.value
		}
		mean := sum / float64(len(ordered))
		return mean, &arbitrationRecord{instant: instant, cell: cell, rule: rule, resolved: mean, contributors: ordered, suppressed: []contribution{}}
	default:
		rank := func(c contribution) float64 {
			if entry != nil {
				if r, ok := entry.providerRanks[c.provider]; ok {
					return r
				}
			}
			return determinismRank(c.provider)
		}
		top := pick(cs, rank, true)
		within := ""
		if entry != nil {
			within = entry.withinRank
		}
		switch within {
		case "SEVERITY":
			winners = pick(pick(top, severity, true), value, true)
		case "MIN", "AND":
			winners = pick(top, value, false)
		default:
			winners = pick(top, value, true)
		}
	}
	resolved := winners[0].value
	suppressed := []contribution{}
	for _, c := range cs {
		won := false
		for _, w := range winners {
			if w == c {
				won = true
				break
			}
		}
		if !won {
			suppressed = append(suppressed, c)
		}
	}
	return resolved, &arbitrationRecord{instant: instant, cell: cell, rule: rule, resolved: resolved, contributors: append([]contribution{}, cs...), suppressed: suppressed}
}

// arbiterShards is ARBITER_SHARDS, else the parallelism available: a
// deployment fact /api/arbitration reports.
func arbiterShards() int {
	if raw, ok := os.LookupEnv("ARBITER_SHARDS"); ok {
		n, _ := strconv.Atoi(strings.TrimSpace(raw))
		return max(n, 1)
	}
	return max(runtime.GOMAXPROCS(0), 1)
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (c contribution) json() map[string]any {
	return map[string]any{
		"provider": c.provider, "determinism": determinism(c.provider), "originId": c.originID,
		"cesId": nullable(c.cesID), "outputVectorId": nullable(c.outputVectorID),
		"ragStatusCode": nullable(c.ragStatusCode), "value": c.value,
	}
}

// recordsJSON renders records; canonical (the retained world) orders them by
// cell, and contributors and suppressed by (provider, originId, cesId,
// outputVectorId).
func recordsJSON(records []arbitrationRecord, canonical bool) []any {
	rs := append([]arbitrationRecord{}, records...)
	if canonical {
		less := func(a, b contribution) bool {
			if a.provider != b.provider {
				return a.provider < b.provider
			}
			if a.originID != b.originID {
				return a.originID < b.originID
			}
			if a.cesID != b.cesID {
				return a.cesID < b.cesID
			}
			return a.outputVectorID < b.outputVectorID
		}
		sort.SliceStable(rs, func(i, j int) bool { return rs[i].cell < rs[j].cell })
		for i := range rs {
			rs[i].contributors = append([]contribution{}, rs[i].contributors...)
			rs[i].suppressed = append([]contribution{}, rs[i].suppressed...)
			sort.SliceStable(rs[i].contributors, func(a, b int) bool { return less(rs[i].contributors[a], rs[i].contributors[b]) })
			sort.SliceStable(rs[i].suppressed, func(a, b int) bool { return less(rs[i].suppressed[a], rs[i].suppressed[b]) })
		}
	}
	out := make([]any, len(rs))
	for i, r := range rs {
		contributors := make([]any, len(r.contributors))
		for j, c := range r.contributors {
			contributors[j] = c.json()
		}
		suppressed := make([]any, len(r.suppressed))
		for j, c := range r.suppressed {
			suppressed[j] = c.json()
		}
		out[i] = map[string]any{
			"instant": r.instant, "cell": r.cell, "rule": r.rule, "resolved": r.resolved,
			"contributors": contributors, "suppressed": suppressed,
		}
	}
	return out
}
