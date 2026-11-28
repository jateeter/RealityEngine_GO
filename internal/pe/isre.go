package pe

// The PE half of a K-line: one ISRE composition record per committed step
// (re-isre-composition/1), kept in a persistent store per PE instance
// (RealityEngine_CI#603; SURFACE_SPEC "The PE half: ISRE composition"). A port
// of C++ isre.cpp.
//
// Which source won each input cell, of what kind, under the versioned
// source-kind taxonomy the corpus carries
// (<MACHINES_DIR>/../schemas/source-kinds/re-source-kinds-<N>.json).

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jateeter/RealityEngine_GO/internal/engine"
)

// taxonomyVersion is the version the PEs record (owner, 2026-11-27).
const taxonomyVersion = 2

const (
	isreStoreMaxDefault = 2 * 1024 * 1024 * 1024
	isreStoreMaxMax     = 9007199254740991
)

type kindEntry struct {
	kind     string
	provider *string
	class    string
}

func (e kindEntry) equal(o kindEntry) bool {
	if e.kind != o.kind || e.class != o.class || (e.provider == nil) != (o.provider == nil) {
		return false
	}
	return e.provider == nil || *e.provider == *o.provider
}

type taxonomy struct {
	version         int
	providerMapping map[string]string
	kindClass       map[string]string            // kinds without providers
	providerClass   map[string]map[string]string // kind -> provider -> class
	unknownProvider string
	unknownClass    string
}

// taxonomyDir is <MACHINES_DIR>/../schemas/source-kinds.
func taxonomyDir(machinesDir string) string {
	if machinesDir == "" {
		machinesDir = "../RealityEngine_Machines/machines"
	}
	abs, err := filepath.Abs(machinesDir)
	if err != nil {
		abs = machinesDir
	}
	return filepath.Join(filepath.Dir(abs), "schemas", "source-kinds")
}

// loadTaxonomy reads re-source-kinds-<version>.json from dir; nil when it is
// not there or does not parse.
func loadTaxonomy(dir string, version int) *taxonomy {
	data, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("re-source-kinds-%d.json", version)))
	if err != nil {
		return nil
	}
	var doc map[string]any
	if json.Unmarshal(data, &doc) != nil || doc["format"] != "re-source-kinds" || doc["version"] != float64(version) {
		return nil
	}
	t := &taxonomy{version: version, providerMapping: map[string]string{}, kindClass: map[string]string{},
		providerClass: map[string]map[string]string{}, unknownProvider: "unattributed", unknownClass: "generated"}
	if m, ok := doc["providerMapping"].(map[string]any); ok {
		for k, v := range m {
			if s, ok := v.(string); ok {
				t.providerMapping[k] = s
			}
		}
	}
	kinds, _ := doc["kinds"].([]any)
	for _, raw := range kinds {
		k, _ := raw.(map[string]any)
		kind, _ := k["kind"].(string)
		if providers, ok := k["providers"].([]any); ok {
			per := map[string]string{}
			for _, p := range providers {
				pm, _ := p.(map[string]any)
				name, _ := pm["provider"].(string)
				class, _ := pm["class"].(string)
				per[name] = class
			}
			t.providerClass[kind] = per
		} else if class, ok := k["class"].(string); ok {
			t.kindClass[kind] = class
		}
	}
	if u, ok := doc["unknownProvider"].(map[string]any); ok {
		if p, ok := u["provider"].(string); ok {
			t.unknownProvider = p
		}
		if c, ok := u["class"].(string); ok {
			t.unknownClass = c
		}
	}
	return t
}

func (t *taxonomy) name() string { return fmt.Sprintf("re-source-kinds/%d", t.version) }

// sourceEntry: a source's kind; a sensor's provider from its origin (first
// `.` segment, lowercased, through providerMapping; empty is signal; unlisted
// is the unknown provider); and the version's class.
func (t *taxonomy) sourceEntry(kind, origin string) kindEntry {
	if class, ok := t.kindClass[kind]; ok {
		return kindEntry{kind: kind, class: class}
	}
	// A kind with providers (sensor), or one the version does not list, read
	// as a sensor of its origin.
	asKind := kind
	if _, ok := t.providerClass[kind]; !ok {
		asKind = "sensor"
	}
	head, _, _ := strings.Cut(origin, ".")
	provider := strings.ToLower(head)
	if provider == "" {
		provider = "signal"
	}
	if mapped, ok := t.providerMapping[provider]; ok {
		provider = mapped
	}
	if class, ok := t.providerClass[asKind][provider]; ok {
		return kindEntry{asKind, &provider, class}
	}
	unknown := t.unknownProvider
	return kindEntry{asKind, &unknown, t.unknownClass}
}

func (t *taxonomy) feedbackEntry() kindEntry {
	class, ok := t.kindClass["feedback"]
	if !ok {
		class = "deterministic"
	}
	return kindEntry{kind: "feedback", class: class}
}

// winner is one cell's winner at a step: a source, or the feedback term.
type winner struct {
	cell             int64
	feedback         bool
	id, kind, origin string
	causedBy         any
}

// sourcesJSON is the step's sources: runs of consecutive cells with one
// winner, ascending.
func sourcesJSON(cells []winner, t *taxonomy) []any {
	type run struct {
		first, last int64
		entry       kindEntry
		id          string
		causedBy    any
	}
	runs := []any{}
	flush := func(r *run) {
		o := map[string]any{"cells": []any{r.first, r.last}, "kind": r.entry.kind, "class": r.entry.class, "causedBy": r.causedBy}
		if r.entry.provider != nil {
			o["provider"] = *r.entry.provider
		}
		if r.id != "" {
			o["id"] = r.id
		}
		runs = append(runs, o)
	}
	sorted := append([]winner{}, cells...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].cell < sorted[j].cell })
	var cur *run
	for _, w := range sorted {
		entry := t.sourceEntry(w.kind, w.origin)
		id, causedBy := w.id, w.causedBy
		if w.feedback {
			entry, id, causedBy = t.feedbackEntry(), "", nil
		}
		if cur != nil && cur.last+1 == w.cell && cur.entry.equal(entry) && cur.id == id && engine.JCS(cur.causedBy) == engine.JCS(causedBy) {
			cur.last = w.cell
			continue
		}
		if cur != nil {
			flush(cur)
		}
		cur = &run{w.cell, w.cell, entry, id, causedBy}
	}
	if cur != nil {
		flush(cur)
	}
	return runs
}

type isreRecorder struct {
	instance      string
	store         *engine.RecordStore
	taxonomy      *taxonomy
	capture       bool
	storeMaxBytes uint64
	reInstance    string
}

func newIsreRecorder(instance string, store *engine.RecordStore, t *taxonomy) *isreRecorder {
	return &isreRecorder{instance: instance, store: store, taxonomy: t, storeMaxBytes: isreStoreMaxDefault}
}

func (r *isreRecorder) refreshBudget() { r.store.RefreshWarnings(r.storeMaxBytes) }

// record is one committed step: re is the RE step response's clock, pe the
// PE's tick for the record, folds the step's fold records.
func (r *isreRecorder) record(re map[string]any, pe map[string]any, cells []winner, folds []any) error {
	if !r.capture || r.taxonomy == nil {
		return nil
	}
	if i, ok := re["instance"].(string); ok {
		r.reInstance = i
	}
	if folds == nil {
		folds = []any{}
	}
	rec := map[string]any{"format": "re-isre-composition/1", "taxonomy": r.taxonomy.name(), "re": re, "pe": pe,
		"sources": sourcesJSON(cells, r.taxonomy), "folds": folds, "delta": false}
	commit, _ := re["commit"].(float64)
	if err := r.store.Append([]map[string]any{rec}, r.taxonomy.name(), uint64(commit)); err != nil {
		return err
	}
	return r.store.EnforceBudget(r.storeMaxBytes)
}

func (r *isreRecorder) list(from uint64, to int64, limit int) map[string]any {
	if limit <= 0 {
		limit = 100
	}
	limit = min(max(limit, 1), 1000)
	out := []any{}
	var next any
	for _, l := range r.store.Between(from, to) {
		if len(out) >= limit {
			next = l.Commit
			break
		}
		var rec any
		_ = json.Unmarshal([]byte(l.Raw), &rec)
		out = append(out, rec)
	}
	return map[string]any{"records": out, "next": next}
}

func (r *isreRecorder) stats() map[string]any {
	s := r.store.Stats(r.storeMaxBytes)
	var name any
	if r.taxonomy != nil {
		name = r.taxonomy.name()
	}
	return map[string]any{"segments": s["segments"], "bytes": s["bytes"], "budget": s["budget"], "warnings": s["warnings"],
		"records": r.store.Len(), "isreCapture": r.capture, "taxonomy": name}
}

// exportBundle is a re-isre-bundle/1: JSON Lines, header first. It marks the
// segments it covered as exported.
func (r *isreRecorder) exportBundle(from uint64, to int64) (string, error) {
	var body strings.Builder
	count := 0
	segments := map[string]bool{}
	for _, l := range r.store.Between(from, to) {
		body.WriteString(l.Raw + "\n")
		count++
		segments[l.Segment] = true
	}
	var name any
	if r.taxonomy != nil {
		name = r.taxonomy.name()
	}
	header := map[string]any{"format": "re-isre-bundle/1", "source": map[string]any{"instance": r.instance},
		"re": map[string]any{"instance": r.reInstance}, "taxonomy": name, "count": count,
		"sha256": engine.SHA256Hex([]byte(body.String()))}
	if err := r.store.MarkExported(segments); err != nil {
		return "", err
	}
	return engine.JCS(header) + "\n" + body.String(), nil
}

// isreRecorderFromEnvironment is the recorder for this PE:
// <INSTANCE_ISRE_DIR>/<pe uuid>/, else the clock directory's sibling isre/; a
// minted UUID keeps it in memory. RE_ISRE_CAPTURE=1 turns capture on at boot,
// and then a missing taxonomy refuses the boot.
func isreRecorderFromEnvironment(instanceID, clockDir string) (*isreRecorder, error) {
	uuid := strings.ToLower(os.Getenv("PE_INSTANCE_UUID"))
	dir := ""
	if uuid != "" && uuid == instanceID {
		parent := os.Getenv("INSTANCE_ISRE_DIR")
		if parent == "" {
			parent = filepath.Join(filepath.Dir(clockDir), "isre")
		}
		dir = filepath.Join(parent, uuid)
	}
	tdir := taxonomyDir(os.Getenv("MACHINES_DIR"))
	t := loadTaxonomy(tdir, taxonomyVersion)
	v := os.Getenv("RE_ISRE_CAPTURE")
	capture := v == "1" || v == "true"
	if capture && t == nil {
		return nil, fmt.Errorf("isreCapture is on and re-source-kinds/%d is not in the corpus (%s)", taxonomyVersion, tdir)
	}
	store, err := engine.OpenRecordStore(dir, "isreStoreMaxBytes")
	if err != nil {
		return nil, err
	}
	r := newIsreRecorder(instanceID, store, t)
	r.capture = capture
	r.refreshBudget()
	if capture {
		where := dir
		if where == "" {
			where = "in memory"
		}
		log.Printf("perception-engine: ISRE composition capture on, store %s", where)
	}
	return r, nil
}

// composition is each written input cell's winner in the assembly just
// resolved (C++ PerceptionEngine::composition): the last writer in
// composition order; a folded cell whose fold kept the OSRE term, or an OSRE
// cell no source wrote, is feedback. The caller holds the lock, after
// recordContention.
func (p *PE) composition() []winner {
	now := nowMS()
	n := int64(len(p.persistent))
	writer := make([]*Source, n)
	for _, s := range p.compositionOrder() {
		values := p.sourceValues(s, now)
		for i := 0; i < int(max(s.Region.Length, 0)) && i < len(values); i++ {
			if cell := s.Region.Offset + int64(i); cell >= 0 && cell < n {
				writer[cell] = s
			}
		}
	}
	kept := map[int64]string{}
	for _, f := range p.contention.folds {
		fm, _ := f.(map[string]any)
		k, _ := fm["kept"].(string)
		switch c := fm["cell"].(type) {
		case int64:
			kept[c] = k
		case int:
			kept[int64(c)] = k
		case float64:
			kept[int64(c)] = k
		}
	}
	var out []winner
	for c := int64(0); c < n; c++ {
		_, osreCell := p.osreFold[c]
		switch s := writer[c]; {
		case s != nil && osreCell && kept[c] == "osre":
			out = append(out, winner{cell: c, feedback: true})
		case s != nil:
			origin := ""
			if s.sensor != nil {
				origin = s.sensor.origin
			}
			out = append(out, winner{cell: c, id: s.ID, kind: s.kind(), origin: origin})
		case osreCell:
			out = append(out, winner{cell: c, feedback: true})
		}
	}
	return out
}

// IsreComposition is GET /api/isre-composition.
func (p *PE) IsreComposition(from uint64, to int64, limit int) map[string]any {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.isre.list(from, to, limit)
}

// IsreStats is GET /api/isre-composition/stats.
func (p *PE) IsreStats() map[string]any {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.isre.stats()
}

// IsreExport is POST /api/isre-composition/export.
func (p *PE) IsreExport(from uint64, to int64) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.isre.exportBundle(from, to)
}

// IsreWarnings are the store's budget warnings for /api/health.
func (p *PE) IsreWarnings() []string { return p.isre.store.Warnings() }
