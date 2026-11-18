package analytics

import (
	"testing"
	"time"
)

func TestReportValuesPositionsAndClosedDayPerformance(t *testing.T) {
	input := reportFixture(t)
	report, err := buildReport(input, testPolicy(), "7d")
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "complete" || report.AccountStatus != "current" || report.FreeCashUSD != "947.5" {
		t.Fatalf("account summary: %+v", report)
	}
	checkDecimal(t, report.NAVUSD, "1013.5")
	checkDecimal(t, report.PositionsValueUSD, "66")
	checkDecimal(t, report.UnrealizedPnLUSD, "8.25")
	checkDecimal(t, report.RealizedPnLUSD, "5.25")
	if len(report.Positions) != 1 || report.Positions[0].InstrumentID != "equity:ABC" {
		t.Fatalf("positions: %+v", report.Positions)
	}
	p := report.Performance
	if p.Status != "complete" || p.Observations != 3 || p.BenchmarkStatus != "complete" || p.OperatingStatus != "complete" {
		t.Fatalf("performance: %+v", p)
	}
	checkDecimal(t, p.PnLUSD, "13.5")
	checkDecimal(t, p.RealizedPnLUSD, "5.25")
	checkDecimal(t, p.UnrealizedChangeUSD, "8.25")
	checkDecimal(t, p.ReturnPct, "1.35")
	checkDecimal(t, p.BenchmarkReturnPct, "1")
	checkDecimal(t, p.ExcessPctPoints, "0.35")
	checkDecimal(t, p.MaxDrawdownPct, "0")
	checkDecimal(t, p.ExecutionFeesUSD, "1.5")
	checkDecimal(t, p.OperatingCostsUSD, "1")
	checkDecimal(t, p.OperatingPnLUSD, "12.5")
	checkDecimal(t, p.OperatingExcessPctPoints, "0.25")
}

func TestReportSuppressesIncompleteValuationAndGaps(t *testing.T) {
	input := reportFixture(t)
	input.valuations[0].AvailableAt = input.now.Add(time.Second)
	report, err := buildReport(input, testPolicy(), "30d")
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "incomplete" || report.NAVUSD != nil || report.Positions[0].Status != "mark_missing" ||
		report.Performance.PnLUSD != nil || report.Performance.MaxDrawdownPct != nil {
		t.Fatalf("future mark entered report: %+v", report)
	}
	checkDecimal(t, report.RealizedPnLUSD, "5.25")
	input = reportFixture(t)
	input.head.AccountVersion++
	report, err = buildReport(input, testPolicy(), "all")
	if err != nil {
		t.Fatal(err)
	}
	if report.AccountStatus != "gap" || report.Status != "stale" || report.NAVUSD != nil || report.Performance.PnLUSD != nil {
		t.Fatalf("missing ledger tail entered report: %+v", report)
	}
}

func TestPerformanceChecksAllDaysAndCostCoverage(t *testing.T) {
	input := reportFixture(t)
	input.valuations = append(input.valuations,
		mark("low", "20", input.now.AddDate(0, 0, -1).Add(-3*time.Hour)),
	)
	// With an older mark below the previous day's peak, the close-to-close drawdown is nonzero.
	report, err := buildReport(input, testPolicy(), "all")
	if err != nil {
		t.Fatal(err)
	}
	checkDecimal(t, report.Performance.MaxDrawdownPct, "0.59200789")
	input.valuations[4].Coverage.Categories["llm"] = "zero"
	report, err = buildReport(input, testPolicy(), "all")
	if err != nil {
		t.Fatal(err)
	}
	if report.Performance.OperatingStatus != "coverage_missing" || report.Performance.OperatingPnLUSD != nil {
		t.Fatalf("cost conflicts with declared zero coverage: %+v", report.Performance)
	}
}

func TestHistoricalRowsUseRecordingTime(t *testing.T) {
	input := reportFixture(t)
	input.events[6].RecordedAt = input.now.Add(-time.Minute)
	state := newState()
	for _, event := range input.events {
		if err := state.apply(event); err != nil {
			t.Fatal(err)
		}
	}
	input.state = state
	report, err := buildReport(input, testPolicy(), "all")
	if err != nil {
		t.Fatal(err)
	}
	checkDecimal(t, report.RealizedPnLUSD, "5.25")
	checkDecimal(t, report.Performance.RealizedPnLUSD, "0")
	checkDecimal(t, report.Performance.PnLUSD, "11")
}

func checkDecimal(t *testing.T, actual *string, want string) {
	t.Helper()
	if actual == nil {
		t.Fatalf("expected decimal %s, got null", want)
	}
	if *actual != want {
		t.Fatalf("expected decimal %s, got %s", want, *actual)
	}
}

func testPolicy() Policy {
	return Policy{MarkMaxAge: 72 * time.Hour, HeadMaxAge: 2 * time.Minute, BenchmarkID: "test-total-return"}
}

func reportFixture(t *testing.T) reportInput {
	t.Helper()
	input := reportInput{
		accountID: testAccount,
		state:     newState(),
		events:    scenario(),
		now:       time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC),
	}
	for _, event := range input.events {
		if err := input.state.apply(event); err != nil {
			t.Fatal(err)
		}
	}
	input.head = AccountHead{AccountVersion: input.state.Version, ObservedAt: input.now.Add(-time.Second)}
	input.seen = input.state.Version
	first := input.events[0].RecordedAt
	input.valuations = []Valuation{
		mark("abc", "22", first.Add(8*time.Hour)),
		{ID: "bench-start", Kind: "benchmark", ObservedAt: first.Add(-time.Hour), AvailableAt: first.Add(-time.Hour),
			Benchmark: &Benchmark{SeriesID: "test-total-return", Level: "100"}},
		{ID: "bench-close", Kind: "benchmark", ObservedAt: first.Add(30 * time.Hour), AvailableAt: first.Add(30 * time.Hour),
			Benchmark: &Benchmark{SeriesID: "test-total-return", Level: "101"}},
		{ID: "expense", Kind: "operating_cost", ObservedAt: first.Add(2 * time.Hour), AvailableAt: first.Add(2 * time.Hour),
			Cost: &Cost{Category: "llm", AmountUSD: "1"}},
		{ID: "closure", Kind: "cost_coverage", ObservedAt: input.now.Add(-time.Hour), AvailableAt: input.now.Add(-time.Hour),
			Coverage: &Coverage{FirstDay: "2026-11-16", LastDay: "2026-11-17",
				Categories: map[string]string{"llm": "complete", "data": "zero", "additional_execution_fees": "zero", "chain_fees": "zero",
					"fx": "zero", "infrastructure": "zero", "issuer_drag": "zero", "other_operating": "zero"}}},
	}
	return input
}

func mark(id, price string, at time.Time) Valuation {
	return Valuation{ID: id, Kind: "mark", SourceID: "test-marks", SourceKind: "synthetic_fixture", ObservedAt: at, AvailableAt: at,
		Mark: &Mark{Instrument: Instrument{InstrumentID: "equity:ABC", RegistryVersion: "v1"}, PriceUSD: price}}
}
