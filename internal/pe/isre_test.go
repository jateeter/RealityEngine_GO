package pe

import (
	"testing"

	"github.com/jateeter/RealityEngine_GO/internal/engine"
)

// Runs join consecutive cells with one winner, under re-source-kinds/2: a
// test source, a mapped provider (OpenClaw is acp), feedback, an empty
// origin (signal) and an unlisted one (unattributed).
func TestIsreRunsJoinConsecutiveCellsWithOneWinner(t *testing.T) {
	// A pinned copy of the corpus file (RealityEngine_Machines
	// schemas/source-kinds/), so the test runs without a sibling checkout.
	tax := loadTaxonomy("testdata", taxonomyVersion)
	if tax == nil {
		t.Fatal("the pinned re-source-kinds/2 does not parse")
	}
	cells := []winner{
		{cell: 3, kind: "test", id: "a"}, {cell: 4, kind: "test", id: "a"},
		{cell: 5, kind: "sensor", origin: "OpenClaw.agent", id: "b"},
		{cell: 6, feedback: true}, {cell: 7, feedback: true},
		{cell: 9, kind: "sensor", id: "c"}, {cell: 10, kind: "sensor", origin: "nobody", id: "c"},
	}
	got := engine.JCS(sourcesJSON(cells, tax))
	want := `[{"causedBy":null,"cells":[3,4],"class":"deterministic","id":"a","kind":"test"},` +
		`{"causedBy":null,"cells":[5,5],"class":"generated","id":"b","kind":"sensor","provider":"acp"},` +
		`{"causedBy":null,"cells":[6,7],"class":"deterministic","kind":"feedback"},` +
		`{"causedBy":null,"cells":[9,9],"class":"measured","id":"c","kind":"sensor","provider":"signal"},` +
		`{"causedBy":null,"cells":[10,10],"class":"generated","id":"c","kind":"sensor","provider":"unattributed"}]`
	if got != want {
		t.Fatalf("\n got %s\nwant %s", got, want)
	}
}
