package engine

// K-lines: episodes captured at their corpus boundaries while the engine
// steps, kept in a persistent store per engine instance (RealityEngine_CI#603,
// this repo's #17).
//
// SURFACE_SPEC.md governs: "Boundaries: where a K-line starts and ends",
// "Paths that dwell, fork or merge", "Scope", "Identity, clock and parity",
// "Managing K-lines during operation", "The K-line store" and "Record". This
// is the RE half: the episode, its active lists, OSRE, OSRE(m) and scope.
//
// The tracker observes committed steps and never re-derives the transition
// rule. It follows C++ kline.cpp (and RealityEngine_Rust's kline.rs) line for
// line, so the voters' records and this follower's are built the same way.

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode/utf16"

	"github.com/jateeter/RealityEngine_GO/internal/instance"
	"github.com/jateeter/RealityEngine_GO/internal/machine"
)

// ── JCS, SHA-256, the corpus fingerprint ────────────────────────────────────

// jcs is RFC 8785: keys in code-point order, numbers in ECMAScript form, the
// minimal escapes.
func jcs(v any) string {
	var b strings.Builder
	appendJCS(&b, v)
	return b.String()
}

func appendJCS(b *strings.Builder, v any) {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if x {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case string:
		appendJCSString(b, x)
	case float64:
		b.WriteString(ecmaNumber(x))
	case int:
		b.WriteString(strconv.Itoa(x))
	case int64:
		b.WriteString(strconv.FormatInt(x, 10))
	case uint64:
		b.WriteString(strconv.FormatUint(x, 10))
	case json.Number:
		f, _ := x.Float64()
		b.WriteString(ecmaNumber(f))
	case []any:
		b.WriteByte('[')
		for i, item := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			appendJCS(b, item)
		}
		b.WriteByte(']')
	case []string:
		items := make([]any, len(x))
		for i, s := range x {
			items[i] = s
		}
		appendJCS(b, items)
	case []float64:
		items := make([]any, len(x))
		for i, f := range x {
			items[i] = f
		}
		appendJCS(b, items)
	case []map[string]any:
		items := make([]any, len(x))
		for i, m := range x {
			items[i] = m
		}
		appendJCS(b, items)
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return utf16Less(keys[i], keys[j]) })
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			appendJCSString(b, k)
			b.WriteByte(':')
			appendJCS(b, x[k])
		}
		b.WriteByte('}')
	default:
		// Anything else goes through encoding/json and back as a generic value.
		raw, err := json.Marshal(x)
		if err != nil {
			b.WriteString("null")
			return
		}
		var generic any
		_ = json.Unmarshal(raw, &generic)
		appendJCS(b, generic)
	}
}

func utf16Less(a, b string) bool {
	ua, ub := utf16.Encode([]rune(a)), utf16.Encode([]rune(b))
	for i := 0; i < len(ua) && i < len(ub); i++ {
		if ua[i] != ub[i] {
			return ua[i] < ub[i]
		}
	}
	return len(ua) < len(ub)
}

func appendJCSString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
}

// ecmaNumber is ECMAScript Number::toString, which encoding/json writes for a
// float64.
func ecmaNumber(f float64) string {
	raw, err := json.Marshal(f)
	if err != nil {
		return "null"
	}
	return string(raw)
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// corpusFingerprint is sha256/resident-declared-v1 over (name, declared
// machine object) pairs.
func corpusFingerprint(declared [][2]any) string {
	type line struct{ name, hex string }
	lines := make([]line, len(declared))
	for i, d := range declared {
		lines[i] = line{d[0].(string), sha256Hex([]byte(jcs(d[1])))}
	}
	sort.Slice(lines, func(i, j int) bool {
		if lines[i].name != lines[j].name {
			return lines[i].name < lines[j].name
		}
		return lines[i].hex < lines[j].hex
	})
	var all strings.Builder
	for _, l := range lines {
		all.WriteString(l.name + "\t" + l.hex + "\n")
	}
	return sha256Hex([]byte(all.String()))
}

// ── Values ──────────────────────────────────────────────────────────────────

type stamp struct {
	instance        string
	lamport, commit uint64
	step            int
}

func (s stamp) json() map[string]any {
	return map[string]any{"instance": s.instance, "lamport": s.lamport, "commit": s.commit, "step": s.step}
}

func stampFrom(v any) stamp {
	m, _ := v.(map[string]any)
	s, _ := m["instance"].(string)
	return stamp{s, uint64(num(m["lamport"])), uint64(num(m["commit"])), int(num(m["step"]))}
}

func num(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int:
		return float64(x)
	case int64:
		return float64(x)
	case uint64:
		return float64(x)
	case json.Number:
		f, _ := x.Float64()
		return f
	}
	return 0
}

const (
	klineFeedbackHopsDefault = 3
	klineFeedbackHopsMax     = 16
	klineStoreMaxDefault     = 2 * 1024 * 1024 * 1024
	klineStoreMaxMax         = 9007199254740991
)

type klineControls struct {
	capture       bool
	feedbackHops  int
	storeMaxBytes uint64
}

func defaultKlineControls() klineControls {
	return klineControls{false, klineFeedbackHopsDefault, klineStoreMaxDefault}
}

type kRegion struct{ offset, length int }

// klineOp is one machine's operation in a step's mergeBatch, by corpus name.
type klineOp struct {
	machine     string
	region      kRegion
	values      []float64
	sequenceIDs []string
}

type activeEntry struct {
	machine, sequence, event string
	justMatched              bool
}

func equalActives(a, b []activeEntry) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// stepView is what the tracker reads at a step's completion point.
type stepView struct {
	step        int
	commit      uint64
	machines    map[string]*machine.Machine
	results     map[string]map[string]any
	isre, osre  trajectoryEntry
	ops         []klineOp
	fingerprint string
}

// regionsOf is the machine's (input, output) regions, when it declares a mapping.
func regionsOf(m *machine.Machine) (kRegion, kRegion, bool) {
	in, out, ok := m.Regions()
	if !ok {
		return kRegion{}, kRegion{}, false
	}
	return kRegion{in.Offset, in.Length}, kRegion{out.Offset, out.Length}, true
}

func overlaps(a, b kRegion) bool {
	return a.length > 0 && b.length > 0 && a.offset < b.offset+b.length && b.offset < a.offset+a.length
}

func entryJSON(e trajectoryEntry, keep []kRegion) map[string]any {
	nz := []any{}
	for _, c := range e.nonZero {
		in := len(keep) == 0
		for _, r := range keep {
			if c.index >= r.offset && c.index < r.offset+r.length {
				in = true
				break
			}
		}
		if in {
			nz = append(nz, map[string]any{"index": c.index, "value": c.value})
		}
	}
	return map[string]any{"stepNumber": e.stepNumber, "length": e.length, "nonZero": nz}
}

func activesJSON(list []activeEntry) []any {
	sorted := append([]activeEntry{}, list...)
	sort.SliceStable(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		if a.machine != b.machine {
			return a.machine < b.machine
		}
		if a.sequence != b.sequence {
			return a.sequence < b.sequence
		}
		return a.event < b.event
	})
	out := make([]any, len(sorted))
	for i, e := range sorted {
		out[i] = map[string]any{"machine": e.machine, "sequence": e.sequence, "event": e.event, "justMatched": e.justMatched}
	}
	return out
}

// comparatorOf is the comparator an element is held to, under C++'s names.
func comparatorOf(declared, algorithm string) string {
	name := declared
	if name == "" {
		name = algorithm
	}
	switch name {
	case "equals", "custom":
		return "equals"
	case "threshold", "pattern":
		return name
	}
	return "gte"
}

// differenceOf is the first comparison that failed for ev against input.
func differenceOf(ev *machine.Event, algorithm string, input []float64, r kRegion) map[string]any {
	idx := 0
	if len(input) == len(ev.Elements) {
		for i, el := range ev.Elements {
			v, actual := el.Value, input[i]
			var ok bool
			switch comparatorOf(el.Comparator, algorithm) {
			case "equals":
				ok = actual == v
			case "threshold":
				ok = abs(v-actual) <= thresholdOr(el.Threshold, 0.1)
			case "pattern":
				ok = 1-abs(v-actual) >= thresholdOr(el.Threshold, 0.5)
			default:
				t := thresholdOr(el.Threshold, 0.5)
				ok = (actual >= t) == (v >= t)
			}
			if !ok {
				idx = i
				break
			}
		}
	}
	comparator := comparatorOf("", algorithm)
	var threshold any
	if idx < len(ev.Elements) {
		el := ev.Elements[idx]
		comparator = comparatorOf(el.Comparator, algorithm)
		switch comparator {
		case "threshold":
			threshold = thresholdOr(el.Threshold, 0.1)
		case "pattern", "gte":
			threshold = thresholdOr(el.Threshold, 0.5)
		}
	}
	value := 0.0
	if idx < len(input) {
		value = input[idx]
	}
	return map[string]any{
		"expected": []any{ev.ID}, "elementIndex": idx, "cell": r.offset + idx, "value": value,
		"comparator": comparator, "threshold": threshold, "cells": []any{r.offset, r.offset + r.length - 1},
	}
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

func thresholdOr(t *float64, d float64) float64 {
	if t == nil {
		return d
	}
	return *t
}

func initialActives(m *machine.Machine) []activeEntry {
	var out []activeEntry
	for _, sid := range m.SequenceIDs() {
		seq := m.Sequences[sid]
		for _, eid := range seq.EventIDs() {
			if seq.Events[eid].Initial {
				out = append(out, activeEntry{m.Name, sid, eid, false})
			}
		}
	}
	return out
}

func activesOf(m *machine.Machine) []activeEntry {
	var out []activeEntry
	for _, sid := range m.SequenceIDs() {
		seq := m.Sequences[sid]
		for _, eid := range seq.EventIDs() {
			if ev := seq.Events[eid]; ev.Active {
				out = append(out, activeEntry{m.Name, sid, eid, ev.WasJustMatched})
			}
		}
	}
	return out
}

// ── The store ───────────────────────────────────────────────────────────────

const sealBytes = 64 * 1024 * 1024

type klineMeta struct {
	id, machine, sequence, outcome string
	closeCommit                    uint64
	line                           int
	segment                        string
	offset                         int64
}

type klineSegment struct {
	name                       string
	bytes                      int64
	sealed, exported, archived bool
	sha256, fingerprint        string
}

type klineEvent struct {
	through int
	raw     string
}

// klineStore is the append-only store under <INSTANCE_KLINE_DIR>/<uuid>/;
// without a directory it keeps its lines in memory (a minted UUID).
type klineStore struct {
	dir          string
	segments     []klineSegment
	metas        []klineMeta
	byID         map[string]int
	events       map[string][]klineEvent
	lines        []string
	appends      int
	warnMu       sync.Mutex
	warnings     []string
	lastControls klineControls
}

func writeDurable(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("cannot write %s: %w", tmp, err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("write failed: %s: %w", tmp, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	f.Close()
	return os.Rename(tmp, path)
}

// segmentCommit is segment-<commit>.jsonl's commit, or -1.
func segmentCommit(name string) int64 {
	s, ok := strings.CutPrefix(name, "segment-")
	if !ok {
		return -1
	}
	s, ok = strings.CutSuffix(s, ".jsonl")
	if !ok {
		return -1
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return -1
	}
	return n
}

func openKlineStore(dir string) (*klineStore, error) {
	s := &klineStore{dir: dir, byID: map[string]int{}, events: map[string][]klineEvent{}, lastControls: defaultKlineControls()}
	if dir == "" {
		return s, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("cannot create %s: %w", dir, err)
	}
	return s, s.load()
}

func (s *klineStore) load() error {
	known := map[string]klineSegment{}
	if data, err := os.ReadFile(filepath.Join(s.dir, "manifest.json")); err == nil {
		var m map[string]any
		if json.Unmarshal(data, &m) == nil {
			list, _ := m["segments"].([]any)
			for _, raw := range list {
				o, _ := raw.(map[string]any)
				seg := klineSegment{}
				seg.name, _ = o["segment"].(string)
				seg.sealed, _ = o["sealed"].(bool)
				seg.exported, _ = o["exported"].(bool)
				seg.sha256, _ = o["sha256"].(string)
				seg.fingerprint, _ = o["fingerprint"].(string)
				seg.archived, _ = o["archived"].(bool)
				known[seg.name] = seg
			}
		}
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}
	var names []string
	for _, e := range entries {
		if segmentCommit(e.Name()) >= 0 {
			names = append(names, e.Name())
		}
	}
	sort.Slice(names, func(i, j int) bool { return segmentCommit(names[i]) < segmentCommit(names[j]) })
	for _, name := range names {
		path := filepath.Join(s.dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		// A half-written last record is truncated: everything after the last newline.
		good := bytes.LastIndexByte(data, '\n') + 1
		if good != len(data) {
			if err := os.Truncate(path, int64(good)); err != nil {
				return err
			}
			data = data[:good]
		}
		seg := known[name]
		seg.name = name
		seg.bytes = int64(len(data))
		var offset int64
		scanner := bufio.NewScanner(bytes.NewReader(data))
		scanner.Buffer(make([]byte, 1<<20), 1<<30)
		for scanner.Scan() {
			line := scanner.Text()
			s.indexLine(line, name, offset, 0)
			offset += int64(len(line)) + 1
		}
		s.segments = append(s.segments, seg)
	}
	return s.writeIndex()
}

func (s *klineStore) indexLine(raw, segment string, offset int64, line int) {
	var j map[string]any
	if json.Unmarshal([]byte(raw), &j) != nil {
		return // an unparseable line inside a segment is not a record
	}
	str := func(v any) string { x, _ := v.(string); return x }
	obj := func(v any) map[string]any { x, _ := v.(map[string]any); return x }
	switch j["format"] {
	case "re-kline/1":
		m := klineMeta{
			id: str(j["id"]), machine: str(obj(j["focal"])["machine"]), sequence: str(obj(j["focal"])["sequence"]),
			outcome: str(j["outcome"]), closeCommit: uint64(num(obj(j["close"])["commit"])),
			line: line, segment: segment, offset: offset,
		}
		s.byID[m.id] = len(s.metas)
		s.metas = append(s.metas, m)
	case "re-isre-composition/1":
		commit := uint64(num(obj(j["re"])["commit"]))
		m := klineMeta{id: strconv.FormatUint(commit, 10), closeCommit: commit, line: line, segment: segment, offset: offset}
		s.byID[m.id] = len(s.metas)
		s.metas = append(s.metas, m)
	case "re-kline-event/1":
		id := str(j["kline"])
		s.events[id] = append(s.events[id], klineEvent{int(num(j["through"])), raw})
	}
}

func (s *klineStore) activeSegment(fingerprint string, commit uint64) (int, error) {
	if n := len(s.segments); n > 0 && !s.segments[n-1].sealed {
		last := &s.segments[n-1]
		otherCorpus := last.fingerprint != "" && last.fingerprint != fingerprint && last.bytes > 0
		if !otherCorpus && last.bytes < sealBytes {
			if last.fingerprint == "" {
				last.fingerprint = fingerprint
			}
			return n - 1, nil
		}
		if err := s.seal(n - 1); err != nil {
			return 0, err
		}
	}
	s.segments = append(s.segments, klineSegment{name: fmt.Sprintf("segment-%d.jsonl", commit), fingerprint: fingerprint})
	return len(s.segments) - 1, s.writeManifest()
}

func (s *klineStore) seal(i int) error {
	s.segments[i].sealed = true
	if s.dir != "" {
		data, _ := os.ReadFile(filepath.Join(s.dir, s.segments[i].name))
		s.segments[i].sha256 = sha256Hex(data)
	}
	return s.writeManifest()
}

// append writes the step's lines (records and events, in order) and makes
// them durable before returning.
func (s *klineStore) append(lines []map[string]any, fingerprint string, commit uint64) error {
	if len(lines) == 0 {
		return nil
	}
	si, err := s.activeSegment(fingerprint, commit)
	if err != nil {
		return err
	}
	name := s.segments[si].name
	var buf strings.Builder
	type rawAt struct {
		raw    string
		offset int64
	}
	var raws []rawAt
	offset := s.segments[si].bytes
	for _, l := range lines {
		raw := jcs(l)
		raws = append(raws, rawAt{raw, offset})
		offset += int64(len(raw)) + 1
		buf.WriteString(raw)
		buf.WriteByte('\n')
	}
	if s.dir != "" {
		path := filepath.Join(s.dir, name)
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
		if err != nil {
			return fmt.Errorf("cannot append to %s: %w", path, err)
		}
		if _, err := f.WriteString(buf.String()); err != nil {
			f.Close()
			return fmt.Errorf("append failed: %s: %w", path, err)
		}
		// Durable before the step's completion is published.
		if err := f.Sync(); err != nil {
			f.Close()
			return err
		}
		f.Close()
	}
	for _, r := range raws {
		line := 0
		if s.dir == "" {
			s.lines = append(s.lines, r.raw)
			line = len(s.lines) - 1
		}
		s.indexLine(r.raw, name, r.offset, line)
	}
	s.segments[si].bytes += int64(buf.Len())
	// index.json is a cache rebuilt at boot: every 64 appends, not every step.
	s.appends++
	if s.appends%64 == 0 {
		return s.writeIndex()
	}
	return nil
}

func (s *klineStore) checkpoint(open map[string]any) error {
	if s.dir == "" {
		return nil
	}
	return writeDurable(filepath.Join(s.dir, "open.json"), []byte(jcs(open)+"\n"))
}

func (s *klineStore) readCheckpoint() map[string]any {
	if s.dir == "" {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(s.dir, "open.json"))
	if err != nil {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(data, &m) != nil {
		return nil
	}
	return m
}

func (s *klineStore) rawAt(m klineMeta) string {
	if s.dir == "" {
		if m.line < len(s.lines) {
			return s.lines[m.line]
		}
		return ""
	}
	data, err := os.ReadFile(filepath.Join(s.dir, m.segment))
	if err != nil || m.offset > int64(len(data)) {
		return ""
	}
	rest := data[m.offset:]
	if i := bytes.IndexByte(rest, '\n'); i >= 0 {
		rest = rest[:i]
	}
	return string(rest)
}

func (s *klineStore) recordAt(m klineMeta) map[string]any {
	var j map[string]any
	_ = json.Unmarshal([]byte(s.rawAt(m)), &j)
	// A sustain-end appended later belongs on the record as served.
	if list := s.events[m.id]; len(list) > 0 && j != nil {
		j["sustain"] = map[string]any{"through": list[len(list)-1].through, "ended": true}
	}
	return j
}

func (s *klineStore) record(id string) (map[string]any, bool) {
	i, ok := s.byID[id]
	if !ok {
		return nil, false
	}
	return s.recordAt(s.metas[i]), true
}

func (s *klineStore) stats(c klineControls) map[string]any {
	byOutcome := map[string]any{}
	byMachine := map[string]any{}
	for _, m := range s.metas {
		byOutcome[m.outcome] = num(byOutcome[m.outcome]) + 1
		per, _ := byMachine[m.machine].(map[string]any)
		if per == nil {
			per = map[string]any{}
			byMachine[m.machine] = per
		}
		per[m.outcome] = num(per[m.outcome]) + 1
	}
	var total int64
	live := 0
	for _, seg := range s.segments {
		if !seg.archived {
			total += seg.bytes
			live++
		}
	}
	warnings := []any{}
	for _, w := range s.computeWarnings(c) {
		warnings = append(warnings, w)
	}
	return map[string]any{"byOutcome": byOutcome, "byMachine": byMachine, "segments": live, "bytes": total,
		"budget": c.storeMaxBytes, "warnings": warnings}
}

func (s *klineStore) computeWarnings(c klineControls) []string {
	var sealed uint64
	unexported := 0
	for _, seg := range s.segments {
		if seg.archived || !seg.sealed {
			continue
		}
		sealed += uint64(seg.bytes)
		if !seg.exported {
			unexported++
		}
	}
	if sealed > c.storeMaxBytes && unexported > 0 {
		return []string{fmt.Sprintf("klineStoreMaxBytes reached: %d unexported sealed segment(s) are kept; export them to free space", unexported)}
	}
	return nil
}

func (s *klineStore) markExported(names map[string]bool) error {
	for i := range s.segments {
		if names[s.segments[i].name] {
			s.segments[i].exported = true
		}
	}
	if err := s.writeManifest(); err != nil {
		return err
	}
	s.refreshWarnings(s.lastControls)
	return nil
}

func (s *klineStore) refreshWarnings(c klineControls) {
	s.lastControls = c
	w := s.computeWarnings(c)
	s.warnMu.Lock()
	s.warnings = w
	s.warnMu.Unlock()
}

func (s *klineStore) cachedWarnings() []string {
	s.warnMu.Lock()
	defer s.warnMu.Unlock()
	return append([]string{}, s.warnings...)
}

func (s *klineStore) enforceBudget(c klineControls) error {
	sealedBytes := func() uint64 {
		var b uint64
		for _, seg := range s.segments {
			if seg.sealed && !seg.archived {
				b += uint64(seg.bytes)
			}
		}
		return b
	}
	moved := false
	for sealedBytes() > c.storeMaxBytes {
		i := -1
		for k, seg := range s.segments {
			if seg.sealed && seg.exported && !seg.archived {
				i = k
				break
			}
		}
		if i < 0 {
			break // never evict an unexported segment
		}
		if s.dir != "" {
			if err := os.MkdirAll(filepath.Join(s.dir, "archive"), 0o755); err != nil {
				return err
			}
			name := s.segments[i].name
			if err := os.Rename(filepath.Join(s.dir, name), filepath.Join(s.dir, "archive", name)); err != nil {
				return err
			}
		}
		s.segments[i].archived = true
		moved = true
	}
	if moved {
		gone := map[string]bool{}
		for _, seg := range s.segments {
			if seg.archived {
				gone[seg.name] = true
			}
		}
		var kept []klineMeta
		s.byID = map[string]int{}
		for _, m := range s.metas {
			if gone[m.segment] {
				continue
			}
			s.byID[m.id] = len(kept)
			kept = append(kept, m)
		}
		s.metas = kept
		if err := s.writeManifest(); err != nil {
			return err
		}
		if err := s.writeIndex(); err != nil {
			return err
		}
	}
	s.refreshWarnings(c)
	return nil
}

func (s *klineStore) writeManifest() error {
	if s.dir == "" {
		return nil
	}
	segs := []any{}
	for _, seg := range s.segments {
		segs = append(segs, map[string]any{"segment": seg.name, "sealed": seg.sealed, "exported": seg.exported,
			"sha256": seg.sha256, "fingerprint": seg.fingerprint, "archived": seg.archived, "bytes": seg.bytes})
	}
	return writeDurable(filepath.Join(s.dir, "manifest.json"), []byte(jcs(map[string]any{"segments": segs})+"\n"))
}

func (s *klineStore) writeIndex() error {
	if s.dir == "" {
		return nil
	}
	ids := map[string]any{}
	counts := map[string]any{}
	for _, m := range s.metas {
		ids[m.id] = map[string]any{"segment": m.segment, "offset": m.offset}
		key := m.machine + "\t" + m.sequence
		c, _ := counts[key].(map[string]any)
		if c == nil {
			c = map[string]any{}
			counts[key] = c
		}
		c[m.outcome] = num(c[m.outcome]) + 1
	}
	return writeDurable(filepath.Join(s.dir, "index.json"), []byte(jcs(map[string]any{"ids": ids, "counts": counts})+"\n"))
}

// ── The tracker ─────────────────────────────────────────────────────────────

type pathEntry struct {
	event     string
	step      int
	milestone bool
	outputs   []any
}

func (p pathEntry) json() map[string]any {
	return map[string]any{"event": p.event, "step": p.step, "milestone": p.milestone, "outputs": p.outputs}
}

func pathJSON(path []pathEntry) []any {
	out := make([]any, len(path))
	for i, p := range path {
		out[i] = p.json()
	}
	return out
}

type openKline struct {
	id, machine, sequence string
	activator             string // the event that activated what it holds first
	open                  stamp
	held                  map[string]bool
	path                  []pathEntry
	refusedBy             string
	focalPairs            []any // projected onto the focal machine: the shutdown record's pairs
	focalActiveBefore     []any
	input, output         kRegion
}

func (k *openKline) heldIDs() []string {
	ids := make([]string, 0, len(k.held))
	for id := range k.held {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func (k *openKline) clone() *openKline {
	c := *k
	c.held = map[string]bool{}
	for e := range k.held {
		c.held[e] = true
	}
	c.path = append([]pathEntry{}, k.path...)
	c.focalPairs = append([]any{}, k.focalPairs...)
	return &c
}

type sustainKline struct {
	id, machine, sequence, event string
	through                      int
}

type closedKline struct{ openStep, closeStep int }

type regionPair struct {
	input  kRegion
	output *kRegion
}

type stepRecord struct {
	step       int
	isre, osre trajectoryEntry
	ops        []klineOp
	changed    map[string][]activeEntry // machines whose active list changed (deltas)
	asserted   map[[2]string]string     // (machine, sequence) -> the K-line that asserted
	regions    map[string]regionPair
}

type touch struct {
	openIndex      int
	isNew          bool
	milestones     int
	closeOutcome   string
	difference     any
	subsumedBy     string
	sustainEndOf   string
	sustainThrough int
	machine        string
	sequence       string
	openStep       int
}

type klineTracker struct {
	instance    string
	clock       *instance.Clock
	store       *klineStore
	controls    klineControls
	longestPath int
	fingerprint string
	lastStep    int
	lastCommit  uint64
	open        []*openKline
	sustain     []*sustainKline
	closed      map[string]closedKline
	ring        []*stepRecord
	baseline    map[string][]activeEntry
	current     map[string][]activeEntry
}

func newKlineTracker(clock *instance.Clock, store *klineStore) *klineTracker {
	t := &klineTracker{clock: clock, store: store, controls: defaultKlineControls(), longestPath: 7, lastStep: -1}
	if clock != nil {
		t.instance = clock.Instance()
	}
	t.clear()
	return t
}

func (t *klineTracker) clear() {
	t.open, t.sustain = nil, nil
	t.closed = map[string]closedKline{}
	t.ring = nil
	t.baseline = map[string][]activeEntry{}
	t.current = map[string][]activeEntry{}
	t.lastStep = -1
}

func (t *klineTracker) tick() (uint64, error) { return t.clock.Send() }

func (t *klineTracker) stamp(lamport uint64, step int, commit uint64) stamp {
	return stamp{t.instance, lamport, commit, step}
}

func (t *klineTracker) ringDepth() int { return t.longestPath * (t.controls.feedbackHops + 1) }

func (t *klineTracker) refreshBudget() { t.store.refreshWarnings(t.controls) }

func (t *klineTracker) recordFor(step int) *stepRecord {
	for _, r := range t.ring {
		if r.step == step {
			return r
		}
	}
	return nil
}

// activeAt is the newest change at or before step, else the state before the ring.
func (t *klineTracker) activeAt(machineName string, step int) []activeEntry {
	for i := len(t.ring) - 1; i >= 0; i-- {
		r := t.ring[i]
		if r.step > step {
			continue
		}
		if list, ok := r.changed[machineName]; ok {
			return list
		}
	}
	return t.baseline[machineName]
}

func stringSet(v any) map[string]bool {
	out := map[string]bool{}
	switch x := v.(type) {
	case []string:
		for _, s := range x {
			out[s] = true
		}
	case []any:
		for _, s := range x {
			if str, ok := s.(string); ok {
				out[str] = true
			}
		}
	}
	return out
}

func floats(v any) []float64 {
	switch x := v.(type) {
	case []float64:
		return x
	case []any:
		out := make([]float64, 0, len(x))
		for _, f := range x {
			out = append(out, num(f))
		}
		return out
	}
	return nil
}

// onStep is a committed step (its commit tick already taken).
func (t *klineTracker) onStep(v stepView) error {
	if !t.controls.capture {
		return nil
	}
	k := v.step
	t.fingerprint = v.fingerprint

	// Machines by corpus name.
	byName := map[string]*machine.Machine{}
	resultOf := map[string]map[string]any{}
	for id, m := range v.machines {
		byName[m.Name] = m
		resultOf[m.Name] = v.results[id]
	}
	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	sort.Strings(names)
	if len(t.ring) == 0 && len(t.current) == 0 {
		for _, n := range names {
			t.baseline[n] = initialActives(byName[n])
		}
	}

	// ── The step record (pushed now: closes this step read it) ──
	rec := &stepRecord{step: k, isre: v.isre, osre: v.osre, ops: v.ops,
		changed: map[string][]activeEntry{}, asserted: map[[2]string]string{}, regions: map[string]regionPair{}}
	for _, n := range names {
		m := byName[n]
		if in, out, ok := regionsOf(m); ok {
			o := out
			rec.regions[n] = regionPair{in, &o}
		}
		now := activesOf(m)
		before, ok := t.current[n]
		if !ok {
			if b, has := t.baseline[n]; has {
				before = b
			} else {
				before = initialActives(m)
			}
		}
		if !equalActives(now, before) {
			rec.changed[n] = now
		}
		t.current[n] = now
	}

	// ── Boundaries, per sequence ──
	var touches []*touch
	var opened []*openKline
	holder := map[[3]string]string{}
	for _, kl := range t.open {
		for e := range kl.held {
			holder[[3]string{kl.machine, kl.sequence, e}] = kl.id
		}
	}
	for _, s := range t.sustain {
		holder[[3]string{s.machine, s.sequence, s.event}] = s.id
	}
	stillOpen := make([]bool, len(t.open))
	for i := range stillOpen {
		stillOpen[i] = true
	}

	for _, name := range names {
		m := byName[name]
		result := resultOf[name]
		if result == nil {
			continue
		}
		transition, _ := result["transitionResult"].(map[string]any)
		seqResults, _ := transition["sequenceResults"].(map[string]any)
		input := floats(result["inputEvent"])
		inRegion, outRegion, _ := regionsOf(m)
		for _, sid := range m.SequenceIDs() {
			sr, ok := seqResults[sid].(map[string]any)
			if !ok {
				continue
			}
			matched := stringSet(sr["matchedEvents"])
			activated := stringSet(sr["activatedEvents"])
			seq := m.Sequences[sid]
			events := seq.Events
			isActive := func(e string) bool { ev, ok := events[e]; return ok && ev.Active }
			claimed := map[string]string{}
			entry := func(ev *machine.Event) pathEntry {
				outs := []any{}
				for _, o := range ev.Outputs {
					outs = append(outs, nonNilFloats(o.Vector))
				}
				return pathEntry{ev.ID, k, len(ev.Outputs) > 0 && len(ev.NextIDs) > 0, outs}
			}

			// Existing K-lines of this sequence, older first.
			var mine []int
			for i, kl := range t.open {
				if kl.machine == name && kl.sequence == sid {
					mine = append(mine, i)
				}
			}
			sort.SliceStable(mine, func(a, b int) bool {
				x, y := t.open[mine[a]], t.open[mine[b]]
				if x.open.step != y.open.step {
					return x.open.step < y.open.step
				}
				return x.activator < y.activator
			})
			for _, i := range mine {
				kl := t.open[i]
				tc := &touch{openIndex: i, machine: name, sequence: sid, openStep: kl.open.step}
				next := map[string]bool{}
				completedBy, firstFailed := "", ""
				for _, e := range kl.heldIDs() {
					ev, ok := events[e]
					if !ok {
						continue
					}
					if matched[e] {
						kl.path = append(kl.path, entry(ev))
						if kl.path[len(kl.path)-1].milestone {
							tc.milestones++
						}
						if len(ev.NextIDs) == 0 {
							if completedBy == "" {
								completedBy = e
							}
							continue
						}
						for _, n := range ev.NextIDs {
							if n == e { // a dwell: re-activated, still this K-line's
								if isActive(n) {
									next[n] = true
								}
								continue
							}
							if activated[n] {
								if c, taken := claimed[n]; !taken {
									claimed[n] = kl.id
									next[n] = true
								} else if kl.refusedBy == "" {
									kl.refusedBy = c
								}
							} else if h, held := holder[[3]string{name, sid, n}]; held {
								if h != kl.id {
									if kl.refusedBy == "" {
										kl.refusedBy = h
									}
								} else {
									next[n] = true // already this K-line's
								}
							}
						}
						// An event with outputs stays active after it matches (a milestone).
						if len(ev.Outputs) > 0 && isActive(e) {
							next[e] = true
						}
					} else if firstFailed == "" {
						firstFailed = e
					}
				}
				if completedBy != "" {
					tc.closeOutcome = "completed"
					t.sustain = append(t.sustain, &sustainKline{"", name, sid, completedBy, k})
					tc.sustainThrough = k
				} else if len(next) == 0 {
					if kl.refusedBy != "" {
						tc.closeOutcome = "subsumed"
						tc.subsumedBy = kl.refusedBy
					} else {
						tc.closeOutcome = "abandoned"
						e := firstFailed
						if e == "" {
							if ids := kl.heldIDs(); len(ids) > 0 {
								e = ids[0]
							}
						}
						if ev, ok := events[e]; ok {
							tc.difference = differenceOf(ev, m.MatchAlgorithm, input, inRegion)
						}
					}
				}
				kl.held = next
				if tc.closeOutcome != "" {
					stillOpen[i] = false
				}
				touches = append(touches, tc)
			}

			// Sustains of this sequence.
			for _, s := range t.sustain {
				if s.machine != name || s.sequence != sid || s.id == "" {
					continue
				}
				if matched[s.event] {
					s.through = k
				} else {
					openStep := s.through
					if c, ok := t.closed[s.id]; ok {
						openStep = c.openStep
					}
					touches = append(touches, &touch{machine: name, sequence: sid, sustainEndOf: s.id,
						sustainThrough: s.through, openStep: openStep})
					s.through = -1 // removed below
				}
			}

			// A departure: an initial event matched.
			for _, eid := range seq.EventIDs() {
				ev := events[eid]
				if !ev.Initial || !matched[eid] {
					continue
				}
				sustaining := false
				for _, s := range t.sustain {
					if s.machine == name && s.sequence == sid && s.event == eid && s.through == k {
						sustaining = true
					}
				}
				if sustaining {
					continue
				}
				kl := &openKline{machine: name, sequence: sid, activator: eid, open: t.stamp(0, k, v.commit),
					held: map[string]bool{}, input: inRegion, output: outRegion}
				kl.path = append(kl.path, entry(ev))
				tc := &touch{isNew: true, machine: name, sequence: sid, openStep: k}
				if kl.path[0].milestone {
					tc.milestones++
				}
				if len(ev.NextIDs) == 0 {
					// a one-event sequence opens and completes
					tc.closeOutcome = "completed"
					t.sustain = append(t.sustain, &sustainKline{"", name, sid, eid, k})
					tc.sustainThrough = k
				} else {
					for _, n := range ev.NextIDs {
						if n == eid {
							continue // an initial event that lists itself
						}
						if _, taken := claimed[n]; activated[n] && !taken {
							claimed[n] = "#new"
							kl.held[n] = true
						}
					}
					if len(kl.held) == 0 {
						continue // no successor activated: no departure
					}
				}
				// Its focal start state is the list after k-1.
				kl.focalActiveBefore = activesJSON(t.activeAt(name, k-1))
				tc.openIndex = len(opened)
				opened = append(opened, kl)
				touches = append(touches, tc)
			}
		}
	}
	kept := t.sustain[:0]
	for _, s := range t.sustain {
		if s.through >= 0 {
			kept = append(kept, s)
		}
	}
	t.sustain = kept

	t.ring = append(t.ring, rec)
	now := rec

	// ── Ticks, in record order ──
	sort.SliceStable(touches, func(i, j int) bool {
		a, b := touches[i], touches[j]
		if a.machine != b.machine {
			return a.machine < b.machine
		}
		if a.sequence != b.sequence {
			return a.sequence < b.sequence
		}
		if a.openStep != b.openStep {
			return a.openStep < b.openStep
		}
		return !a.isNew && b.isNew
	})
	ref := func(tc *touch) *openKline {
		if tc.isNew {
			return opened[tc.openIndex]
		}
		return t.open[tc.openIndex]
	}
	var lines []map[string]any
	for _, tc := range touches {
		if tc.sustainEndOf != "" {
			l, err := t.tick()
			if err != nil {
				return err
			}
			lines = append(lines, map[string]any{"format": "re-kline-event/1", "kline": tc.sustainEndOf,
				"event": "sustain-end", "through": tc.sustainThrough, "at": t.stamp(l, k, v.commit).json()})
			continue
		}
		kl := ref(tc)
		if tc.isNew {
			l, err := t.tick()
			if err != nil {
				return err
			}
			kl.open.lamport = l
			kl.id = fmt.Sprintf("%s:%d", t.instance, l)
		}
		for i := 0; i < tc.milestones; i++ {
			if _, err := t.tick(); err != nil {
				return err
			}
		}
		// The pair of this step, projected onto the focal machine (shutdown record).
		contribs := []any{}
		for _, op := range now.ops {
			if op.machine == kl.machine {
				contribs = append(contribs, opJSON(op))
			}
		}
		kl.focalPairs = append(kl.focalPairs, map[string]any{"stepNumber": k,
			"isre": entryJSON(now.isre, []kRegion{kl.input}), "osre": entryJSON(now.osre, []kRegion{kl.output}),
			"contributions": contribs})
		if tc.closeOutcome != "" {
			l, err := t.tick()
			if err != nil {
				return err
			}
			closeAt := t.stamp(l, k, v.commit)
			if tc.closeOutcome == "completed" {
				for _, s := range t.sustain {
					if s.id == "" && s.machine == kl.machine && s.sequence == kl.sequence {
						s.id = kl.id
						break
					}
				}
			}
			t.closed[kl.id] = closedKline{kl.open.step, k}
			lines = append(lines, t.buildRecord(kl, tc.closeOutcome, nil, closeAt, tc.difference, tc.subsumedBy, tc.sustainThrough, true))
		}
	}
	// What asserted this step: contributor searches at later steps read it.
	for _, tc := range touches {
		if tc.sustainEndOf != "" {
			continue
		}
		kl := ref(tc)
		if tc.milestones > 0 || tc.closeOutcome == "completed" {
			now.asserted[[2]string{kl.machine, kl.sequence}] = kl.id
		}
	}
	for _, s := range t.sustain {
		if s.through == k && s.id != "" {
			now.asserted[[2]string{s.machine, s.sequence}] = s.id
		}
	}

	// Keep what is still open.
	var keep []*openKline
	for i, kl := range t.open {
		if stillOpen[i] {
			keep = append(keep, kl)
		}
	}
	for _, tc := range touches {
		if tc.isNew && tc.closeOutcome == "" {
			keep = append(keep, opened[tc.openIndex])
		}
	}
	t.open = keep

	t.lastStep = k
	t.lastCommit = v.commit
	if err := t.persistStep(lines); err != nil {
		return err
	}
	t.trimRing()
	return nil
}

func nonNilFloats(f []float64) []any {
	out := make([]any, len(f))
	for i, x := range f {
		out[i] = x
	}
	return out
}

func opJSON(op klineOp) map[string]any {
	return map[string]any{"machine": op.machine,
		"region": map[string]any{"offset": op.region.offset, "length": op.region.length},
		"values": nonNilFloats(op.values)}
}

func (t *klineTracker) buildRecord(k *openKline, outcome string, reason any, closeAt stamp, difference any,
	subsumedBy string, sustainThrough int, ringAvailable bool) map[string]any {
	scope := []string{k.machine}
	contributors := []any{}
	truncated := false
	pairs := k.focalPairs
	activeBefore := k.focalActiveBefore
	if activeBefore == nil {
		activeBefore = []any{}
	}
	activeAfter := []any{}
	shared := []any{}

	if ringAvailable {
		// Contributors through the recorded OSRE(m), hop by hop.
		type frontierItem struct {
			machine  string
			from, to int
		}
		type found struct {
			hops     int
			machine  string
			openStep int
			id       string
		}
		frontier := []frontierItem{{k.machine, k.open.step, closeAt.step}}
		seen := map[string]bool{k.id: true}
		var all []found
		for hop := 1; len(frontier) > 0; hop++ {
			var next []frontierItem
			for _, f := range frontier {
				any := t.recordFor(f.to)
				r := kRegion{}
				if f.machine == k.machine {
					r = k.input
				} else if any != nil {
					if rg, ok := any.regions[f.machine]; ok {
						r = rg.input
					}
				}
				for s := f.from; s <= f.to; s++ {
					prev := t.recordFor(s - 1)
					if prev == nil {
						if s > 0 {
							truncated = true // the ring no longer holds it
						}
						continue
					}
					for _, op := range prev.ops {
						if op.machine == f.machine || !overlaps(op.region, r) {
							continue
						}
						for _, sq := range op.sequenceIDs {
							a, ok := prev.asserted[[2]string{op.machine, sq}]
							if !ok || seen[a] {
								continue
							}
							if hop > t.controls.feedbackHops {
								truncated = true
								continue
							}
							seen[a] = true
							from, to := s-1, s-1
							if c, ok := t.closed[a]; ok {
								from, to = c.openStep, c.closeStep
							}
							all = append(all, found{hop, op.machine, from, a})
							next = append(next, frontierItem{op.machine, from, to})
						}
					}
				}
			}
			frontier = next
		}
		sort.Slice(all, func(i, j int) bool {
			a, b := all[i], all[j]
			if a.hops != b.hops {
				return a.hops < b.hops
			}
			if a.machine != b.machine {
				return a.machine < b.machine
			}
			if a.openStep != b.openStep {
				return a.openStep < b.openStep
			}
			return a.id < b.id
		})
		for _, f := range all {
			contributors = append(contributors, map[string]any{"kline": f.id, "machine": f.machine, "hops": f.hops})
			scope = append(scope, f.machine)
		}
		sort.Strings(scope)
		scope = dedupSorted(scope)

		// Regions of the scope, as they stood at close.
		var inputs, outputs []kRegion
		inScope := map[string]bool{}
		for _, s := range scope {
			inScope[s] = true
		}
		if last := t.recordFor(closeAt.step); last != nil {
			for _, name := range scope {
				rg, ok := last.regions[name]
				if !ok {
					continue
				}
				inputs = append(inputs, rg.input)
				if rg.output != nil {
					outputs = append(outputs, *rg.output)
				}
			}
			// Output cells shared with a machine outside the scope.
			cells := map[int]bool{}
			for _, o := range outputs {
				for name, rg := range last.regions {
					if inScope[name] || rg.output == nil {
						continue
					}
					x := *rg.output
					for c := max(o.offset, x.offset); c < min(o.offset+o.length, x.offset+x.length); c++ {
						cells[c] = true
					}
				}
			}
			list := make([]int, 0, len(cells))
			for c := range cells {
				list = append(list, c)
			}
			sort.Ints(list)
			for _, c := range list {
				shared = append(shared, c)
			}
		}
		var ps []any
		for s := k.open.step; s <= closeAt.step; s++ {
			r := t.recordFor(s)
			if r == nil {
				continue
			}
			var ops []klineOp
			for _, op := range r.ops {
				if inScope[op.machine] {
					ops = append(ops, op)
				}
			}
			sort.SliceStable(ops, func(i, j int) bool {
				if ops[i].region.offset != ops[j].region.offset {
					return ops[i].region.offset < ops[j].region.offset
				}
				return ops[i].machine < ops[j].machine
			})
			contribs := []any{}
			for _, op := range ops {
				contribs = append(contribs, opJSON(op))
			}
			ps = append(ps, map[string]any{"stepNumber": s, "isre": entryJSON(r.isre, inputs),
				"osre": entryJSON(r.osre, outputs), "contributions": contribs})
		}
		if len(ps) > 0 {
			pairs = ps
		}
		var before, after []activeEntry
		for _, name := range scope {
			before = append(before, t.activeAt(name, k.open.step-1)...)
			after = append(after, t.activeAt(name, closeAt.step)...)
		}
		activeBefore = activesJSON(before)
		activeAfter = activesJSON(after)
	} else {
		truncated = true // the ring did not survive: scope is the focal machine alone
	}
	if pairs == nil {
		pairs = []any{}
	}
	completed := outcome == "completed"
	through := closeAt.step
	if completed {
		through = sustainThrough
	}
	var subsumed any
	if subsumedBy != "" {
		subsumed = subsumedBy
	}
	scopeJSON := make([]any, len(scope))
	for i, s := range scope {
		scopeJSON[i] = s
	}
	return map[string]any{
		"format":            "re-kline/1",
		"id":                k.id,
		"corpus":            map[string]any{"algorithm": "sha256/resident-declared-v1", "fingerprint": t.fingerprint},
		"focal":             map[string]any{"machine": k.machine, "sequence": k.sequence},
		"outcome":           outcome,
		"reason":            reason,
		"open":              k.open.json(),
		"close":             closeAt.json(),
		"path":              pathJSON(k.path),
		"difference":        difference,
		"subsumedBy":        subsumed,
		"sustain":           map[string]any{"through": through, "ended": !completed},
		"scope":             scopeJSON,
		"contributors":      contributors,
		"feedbackTruncated": truncated,
		"sharedCells":       shared,
		"activeBefore":      activeBefore,
		"activeAfter":       activeAfter,
		"pairs":             pairs,
		"outputs":           map[string]any{"outputVector": nil, "mergedOutputVector": nil},
	}
}

func dedupSorted(s []string) []string {
	out := s[:0]
	for i, x := range s {
		if i == 0 || x != s[i-1] {
			out = append(out, x)
		}
	}
	return out
}

func (t *klineTracker) sustainEnd(s *sustainKline) (map[string]any, error) {
	l, err := t.tick()
	if err != nil {
		return nil, err
	}
	return map[string]any{"format": "re-kline-event/1", "kline": s.id, "event": "sustain-end", "through": s.through,
		"at": t.stamp(l, max(t.lastStep, 0), t.lastCommit).json()}, nil
}

// onReset closes every open K-line interrupted ("reset") at the last
// committed step, and ends every sustain there.
func (t *klineTracker) onReset() error {
	if !t.controls.capture {
		return nil
	}
	var lines []map[string]any
	order := append([]*openKline{}, t.open...)
	sort.SliceStable(order, func(i, j int) bool {
		a, b := order[i], order[j]
		if a.machine != b.machine {
			return a.machine < b.machine
		}
		if a.sequence != b.sequence {
			return a.sequence < b.sequence
		}
		return a.open.step < b.open.step
	})
	for _, kl := range order {
		l, err := t.tick()
		if err != nil {
			return err
		}
		closeAt := t.stamp(l, max(t.lastStep, 0), t.lastCommit)
		t.closed[kl.id] = closedKline{kl.open.step, closeAt.step}
		lines = append(lines, t.buildRecord(kl, "interrupted", "reset", closeAt, nil, "", closeAt.step, true))
	}
	sustains := append([]*sustainKline{}, t.sustain...)
	sort.SliceStable(sustains, func(i, j int) bool {
		a, b := sustains[i], sustains[j]
		if a.machine != b.machine {
			return a.machine < b.machine
		}
		if a.sequence != b.sequence {
			return a.sequence < b.sequence
		}
		return a.id < b.id
	})
	for _, s := range sustains {
		line, err := t.sustainEnd(s)
		if err != nil {
			return err
		}
		lines = append(lines, line)
	}
	t.open, t.sustain = nil, nil
	if err := t.persistStep(lines); err != nil {
		return err
	}
	t.ring = nil
	t.baseline = map[string][]activeEntry{}
	t.current = map[string][]activeEntry{}
	t.lastStep = -1
	return nil
}

// onMachineRemoved closes the machine's open K-lines interrupted
// ("machine-removed") and ends its sustains.
func (t *klineTracker) onMachineRemoved(name string) error {
	if !t.controls.capture {
		return nil
	}
	var lines []map[string]any
	var keep, gone []*openKline
	for _, kl := range t.open {
		if kl.machine == name {
			gone = append(gone, kl)
		} else {
			keep = append(keep, kl)
		}
	}
	sort.SliceStable(gone, func(i, j int) bool {
		if gone[i].sequence != gone[j].sequence {
			return gone[i].sequence < gone[j].sequence
		}
		return gone[i].open.step < gone[j].open.step
	})
	for _, kl := range gone {
		l, err := t.tick()
		if err != nil {
			return err
		}
		closeAt := t.stamp(l, max(t.lastStep, 0), t.lastCommit)
		t.closed[kl.id] = closedKline{kl.open.step, closeAt.step}
		lines = append(lines, t.buildRecord(kl, "interrupted", "machine-removed", closeAt, nil, "", closeAt.step, true))
	}
	t.open = keep
	var sk []*sustainKline
	for _, s := range t.sustain {
		if s.machine != name {
			sk = append(sk, s)
			continue
		}
		line, err := t.sustainEnd(s)
		if err != nil {
			return err
		}
		lines = append(lines, line)
	}
	t.sustain = sk
	delete(t.current, name)
	return t.persistStep(lines)
}

func (t *klineTracker) checkpointJSON() map[string]any {
	open := []any{}
	for _, kl := range t.open {
		held := []any{}
		for _, e := range kl.heldIDs() {
			held = append(held, e)
		}
		before := kl.focalActiveBefore
		if before == nil {
			before = []any{}
		}
		pairs := kl.focalPairs
		if pairs == nil {
			pairs = []any{}
		}
		open = append(open, map[string]any{
			"id": kl.id, "machine": kl.machine, "sequence": kl.sequence, "activator": kl.activator,
			"open": kl.open.json(), "held": held, "path": pathJSON(kl.path), "refusedBy": kl.refusedBy,
			"focalPairs": pairs, "focalActiveBefore": before, "focalActiveAfter": activesJSON(t.current[kl.machine]),
			"input":  map[string]any{"offset": kl.input.offset, "length": kl.input.length},
			"output": map[string]any{"offset": kl.output.offset, "length": kl.output.length},
		})
	}
	sustains := []any{}
	for _, s := range t.sustain {
		sustains = append(sustains, map[string]any{"id": s.id, "machine": s.machine, "sequence": s.sequence,
			"event": s.event, "through": s.through})
	}
	return map[string]any{"format": "re-kline-open/1", "step": t.lastStep, "commit": t.lastCommit,
		"fingerprint": t.fingerprint, "open": open, "sustain": sustains}
}

func (t *klineTracker) persistStep(lines []map[string]any) error {
	if err := t.store.append(lines, t.fingerprint, t.lastCommit); err != nil {
		return err
	}
	if err := t.store.checkpoint(t.checkpointJSON()); err != nil {
		return err
	}
	return t.store.enforceBudget(t.controls)
}

// recover closes every K-line in the checkpoint interrupted ("shutdown") at
// its step, and ends every sustain there.
func (t *klineTracker) recover() error {
	cp := t.store.readCheckpoint()
	if cp == nil {
		return nil
	}
	step := int(num(cp["step"]))
	commit := uint64(num(cp["commit"]))
	t.fingerprint, _ = cp["fingerprint"].(string)
	t.lastStep, t.lastCommit = step, commit
	var lines []map[string]any
	list, _ := cp["open"].([]any)
	for _, raw := range list {
		o, _ := raw.(map[string]any)
		str := func(k string) string { s, _ := o[k].(string); return s }
		kl := &openKline{id: str("id"), machine: str("machine"), sequence: str("sequence"), activator: str("activator"),
			open: stampFrom(o["open"]), held: map[string]bool{}}
		path, _ := o["path"].([]any)
		for _, p := range path {
			pm, _ := p.(map[string]any)
			ev, _ := pm["event"].(string)
			ms, _ := pm["milestone"].(bool)
			outs, _ := pm["outputs"].([]any)
			kl.path = append(kl.path, pathEntry{ev, int(num(pm["step"])), ms, outs})
		}
		kl.focalPairs, _ = o["focalPairs"].([]any)
		kl.focalActiveBefore, _ = o["focalActiveBefore"].([]any)
		l, err := t.tick()
		if err != nil {
			return err
		}
		rec := t.buildRecord(kl, "interrupted", "shutdown", t.stamp(l, step, commit), nil, "", step, false)
		rec["activeAfter"] = o["focalActiveAfter"]
		lines = append(lines, rec)
	}
	sustains, _ := cp["sustain"].([]any)
	for _, raw := range sustains {
		s, _ := raw.(map[string]any)
		id, _ := s["id"].(string)
		if id == "" {
			continue
		}
		l, err := t.tick()
		if err != nil {
			return err
		}
		lines = append(lines, map[string]any{"format": "re-kline-event/1", "kline": id, "event": "sustain-end",
			"through": s["through"], "at": t.stamp(l, step, commit).json()})
	}
	t.open, t.sustain = nil, nil
	if err := t.persistStep(lines); err != nil {
		return err
	}
	t.lastStep = -1 // the engine's step count starts over
	return nil
}

// startCapture: capture starts afresh, first closing what the checkpoint
// holds, as at boot.
func (t *klineTracker) startCapture() error {
	t.clear()
	t.controls.capture = true
	return t.recover()
}

func (t *klineTracker) trimRing() {
	keepFrom := t.lastStep - max(1, t.ringDepth())
	for _, kl := range t.open {
		keepFrom = min(keepFrom, kl.open.step-1)
	}
	for len(t.ring) > 0 && t.ring[0].step < keepFrom {
		for name, list := range t.ring[0].changed {
			t.baseline[name] = list
		}
		t.ring = t.ring[1:]
	}
	// Closed K-lines are needed while their span is in the ring.
	for id, c := range t.closed {
		if c.closeStep < keepFrom-1 {
			delete(t.closed, id)
		}
	}
}

// ── Routes' bodies ──────────────────────────────────────────────────────────

func (t *klineTracker) openJSON() map[string]any {
	list := append([]*openKline{}, t.open...)
	sort.SliceStable(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if a.machine != b.machine {
			return a.machine < b.machine
		}
		if a.sequence != b.sequence {
			return a.sequence < b.sequence
		}
		return a.open.step < b.open.step
	})
	out := []any{}
	for _, kl := range list {
		held := []any{}
		for _, e := range kl.heldIDs() {
			held = append(held, e)
		}
		out = append(out, map[string]any{"id": kl.id, "focal": map[string]any{"machine": kl.machine, "sequence": kl.sequence},
			"open": kl.open.json(), "path": pathJSON(kl.path), "held": held})
	}
	return map[string]any{"commit": t.lastCommit, "open": out}
}

func (t *klineTracker) list(outcome, machineName, sequence *string, since uint64, limit int) map[string]any {
	if limit <= 0 {
		limit = 100
	}
	limit = min(max(limit, 1), 1000)
	var sel []klineMeta
	for _, m := range t.store.metas {
		if m.closeCommit < since || (outcome != nil && m.outcome != *outcome) ||
			(machineName != nil && m.machine != *machineName) || (sequence != nil && m.sequence != *sequence) {
			continue
		}
		sel = append(sel, m)
	}
	// A page holds whole commits: it stops before the commit that would cross
	// limit, unless that commit is the page's first.
	out := []any{}
	var next any
	for i := 0; i < len(sel); {
		j := i
		for j < len(sel) && sel[j].closeCommit == sel[i].closeCommit {
			j++
		}
		if len(out) > 0 && len(out)+(j-i) > limit {
			next = sel[i].closeCommit
			break
		}
		for _, m := range sel[i:j] {
			out = append(out, t.store.recordAt(m))
		}
		i = j
		if len(out) >= limit && i < len(sel) {
			next = sel[i].closeCommit
			break
		}
	}
	return map[string]any{"klines": out, "next": next}
}

// exportBundle is a re-kline-bundle/1: JSON Lines, header first. It marks the
// segments it covered as exported.
func (t *klineTracker) exportBundle(outcomes map[string]bool, since uint64, machines map[string]bool) (string, error) {
	var body strings.Builder
	count := 0
	segments := map[string]bool{}
	for _, m := range t.store.metas {
		if m.closeCommit < since || (len(outcomes) > 0 && !outcomes[m.outcome]) || (len(machines) > 0 && !machines[m.machine]) {
			continue
		}
		body.WriteString(t.store.rawAt(m) + "\n")
		count++
		for _, e := range t.store.events[m.id] {
			body.WriteString(e.raw + "\n")
			count++
		}
		segments[m.segment] = true
	}
	header := map[string]any{
		"format": "re-kline-bundle/1",
		"source": map[string]any{"instance": t.instance},
		"corpus": map[string]any{"algorithm": "sha256/resident-declared-v1", "fingerprint": t.fingerprint},
		"controls": map[string]any{"klineCapture": t.controls.capture, "klineFeedbackHops": t.controls.feedbackHops,
			"klineStoreMaxBytes": t.controls.storeMaxBytes},
		"count":  count,
		"sha256": sha256Hex([]byte(body.String())),
	}
	if err := t.store.markExported(segments); err != nil {
		return "", err
	}
	return jcs(header) + "\n" + body.String(), nil
}
