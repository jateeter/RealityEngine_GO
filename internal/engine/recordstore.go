package engine

// RecordStore is the K-line store's append-only format for another
// participant: the PE's ISRE composition store (RealityEngine_CI#603,
// SURFACE_SPEC "The PE half: ISRE composition") keeps its records in the same
// segments, index and manifest, under its own budget control.
type RecordStore struct {
	store *klineStore
}

// RecordLine is one stored record: its commit, its line as stored, its segment.
type RecordLine struct {
	Commit  uint64
	Raw     string
	Segment string
}

// OpenRecordStore opens the store under dir ("" keeps it in memory); its
// budget warnings name budgetName.
func OpenRecordStore(dir, budgetName string) (*RecordStore, error) {
	s, err := openKlineStore(dir)
	if err != nil {
		return nil, err
	}
	s.budgetName = budgetName
	return &RecordStore{s}, nil
}

// Append writes one step's records and makes them durable; a new segment
// starts when the fingerprint changes.
func (r *RecordStore) Append(lines []map[string]any, fingerprint string, commit uint64) error {
	return r.store.append(lines, fingerprint, commit)
}

// EnforceBudget archives exported sealed segments past maxBytes and refreshes
// the warnings.
func (r *RecordStore) EnforceBudget(maxBytes uint64) error {
	return r.store.enforceBudget(klineControls{storeMaxBytes: maxBytes})
}

// RefreshWarnings recomputes the budget warnings for maxBytes.
func (r *RecordStore) RefreshWarnings(maxBytes uint64) {
	r.store.refreshWarnings(klineControls{storeMaxBytes: maxBytes})
}

// Warnings are the budget warnings as last computed, without any lock.
func (r *RecordStore) Warnings() []string { return r.store.cachedWarnings() }

// Stats is the store's segments, bytes, budget and warnings.
func (r *RecordStore) Stats(maxBytes uint64) map[string]any {
	return r.store.stats(klineControls{storeMaxBytes: maxBytes})
}

// Len is the number of records stored.
func (r *RecordStore) Len() int { return len(r.store.metas) }

// Between is every record whose commit lies in [from, to] (to < 0: no bound),
// ascending.
func (r *RecordStore) Between(from uint64, to int64) []RecordLine {
	var out []RecordLine
	for _, m := range r.store.metas {
		if m.closeCommit < from || (to >= 0 && m.closeCommit > uint64(to)) {
			continue
		}
		out = append(out, RecordLine{m.closeCommit, r.store.rawAt(m), m.segment})
	}
	return out
}

// MarkExported marks segments exported.
func (r *RecordStore) MarkExported(segments map[string]bool) error {
	return r.store.markExported(segments)
}

// JCS is RFC 8785 serialization, as the stores write it.
func JCS(v any) string { return jcs(v) }

// SHA256Hex is the lower-case hex SHA-256 of b.
func SHA256Hex(b []byte) string { return sha256Hex(b) }
