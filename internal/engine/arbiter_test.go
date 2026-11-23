package engine

import "testing"

func contestant(value float64, rag string, origin string) contribution {
	return contribution{value: value, provider: "machine", originID: origin, cesID: "s", outputVectorID: "0", ragStatusCode: rag}
}

func TestContendedCellsResolveUnderTheDeclaredRule(t *testing.T) {
	cs := []contribution{contestant(1, "GREEN", "m1"), contestant(3, "", "m3"), contestant(2, "RED", "m2")}
	entry := func(rule, within string) *arbitrationEntry {
		return &arbitrationEntry{rule: rule, withinRank: within, providerRanks: map[string]float64{}}
	}
	for _, c := range []struct {
		entry *arbitrationEntry
		want  float64
	}{
		{nil, 3}, {entry("PRECEDENCE", "SEVERITY"), 2}, {entry("PRECEDENCE", "MIN"), 1},
		{entry("SEVERITY", ""), 2}, {entry("AND", ""), 1}, {entry("OR", ""), 3}, {entry("MEAN", ""), 2},
	} {
		if got, _ := resolveCell(7, 0, cs, c.entry); got != c.want {
			t.Errorf("%+v: %v, want %v", c.entry, got, c.want)
		}
	}
	if v, rec := resolveCell(7, 0, cs[:1], entry("AND", "")); v != 1 || rec != nil {
		t.Fatal("one contestant is the value and needs no record")
	}
	_, rec := resolveCell(7, 3, cs, entry("PRECEDENCE", "SEVERITY"))
	if rec.instant != 3 || rec.cell != 7 || rec.rule != "PRECEDENCE" || rec.resolved != 2 ||
		len(rec.contributors) != 3 || len(rec.suppressed) != 2 || rec.suppressed[0].value != 1 || rec.suppressed[1].value != 3 {
		t.Fatalf("%+v", rec)
	}
	if _, mean := resolveCell(7, 3, cs, entry("MEAN", "")); len(mean.suppressed) != 0 || mean.contributors[0].originID != "m1" {
		t.Fatalf("MEAN sums in canonical order and suppresses nothing: %+v", mean)
	}
	ranked := &arbitrationEntry{rule: "PRECEDENCE", providerRanks: map[string]float64{"ui": 9}}
	ui := contribution{value: 0.25, provider: "ui", originID: "src"}
	if got, _ := resolveCell(7, 0, append([]contribution{ui}, cs...), ranked); got != 0.25 {
		t.Fatalf("a declared provider rank outranks determinism: %v", got)
	}
}

func TestATrajectoryEntryIsSparseAndLossless(t *testing.T) {
	e := sparse(4, []float64{0, 1, 0, 2.5})
	if e.stepNumber != 4 || e.length != 4 || len(e.nonZero) != 2 || e.nonZero[1] != (cellValue{3, 2.5}) {
		t.Fatalf("%+v", e)
	}
}
