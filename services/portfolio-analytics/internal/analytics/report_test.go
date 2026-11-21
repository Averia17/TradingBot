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

func TestPositionWeightsUseExactValuesBeforeReportRounding(t *testing.T) {
	cutoff := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	state := newState()
	marks := []Valuation{mark("a", "0.00000001", cutoff), mark("b", "0.00000001", cutoff)}
	marks[0].Mark.Instrument.Decimals = 1
	marks[1].Mark.Instrument = Instrument{InstrumentID: "equity:XYZ", RegistryVersion: "v1", Decimals: 1}
	for i, quantity := range []string{"3", "4"} {
		instrument := marks[i].Mark.Instrument
		state.Positions[instrument.InstrumentID] = holding{Balance: Balance{Instrument: instrument, QuantityRaw: quantity}}
	}
	index := indexValuations(marks, "")
	current := valuePortfolioAt(state, index, cutoff, time.Hour, true)
	historical := valuePortfolioAt(state, index, cutoff, time.Hour, false)
	if historical.nav.Cmp(current.nav) != 0 || len(historical.positions) != 0 {
		t.Fatal("historical scan must use the same exact NAV without position rows")
	}
	if len(current.positions) != 2 || current.positions[0].InstrumentID != "equity:ABC" {
		t.Fatalf("positions with identical displayed value must sort by instrument ID: %+v", current.positions)
	}
	checkDecimal(t, current.positions[0].MarketValueUSD, "0")
	checkDecimal(t, current.positions[1].MarketValueUSD, "0")
	checkDecimal(t, current.positions[0].WeightPct, "42.85714286")
	checkDecimal(t, current.positions[1].WeightPct, "57.14285714")
}

func TestValuationTimelineKeepsCausalMarkPriority(t *testing.T) {
	day := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	rows := []Valuation{
		{ID: "new", ObservedAt: day.Add(time.Hour), AvailableAt: day.Add(3 * time.Hour)},
		{ID: "old-late", ObservedAt: day, AvailableAt: day.Add(4 * time.Hour)},
		{ID: "tie-b", ObservedAt: day, AvailableAt: day.Add(time.Hour)},
		{ID: "tie-a", ObservedAt: day, AvailableAt: day.Add(time.Hour)},
		{ID: "new-correction", ObservedAt: day.Add(time.Hour), AvailableAt: day.Add(5 * time.Hour)},
	}
	timeline := buildValuationTimeline(rows)
	if got := timeline.latestVisibleAt(day); got != nil {
		t.Fatalf("unavailable mark entered valuation: %+v", got)
	}
	for i, want := range []string{"tie-a", "tie-a", "new", "new", "new-correction"} {
		got := timeline.latestVisibleAt(day.Add(time.Duration(i+1) * time.Hour))
		if got == nil || got.ID != want {
			t.Fatalf("cutoff %d: got %+v, want %s", i+1, got, want)
		}
	}
}

func TestCostCoverageUsesTheLatestAvailableOverlappingInterval(t *testing.T) {
	start := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC).Add(-time.Nanosecond)
	end := start.AddDate(0, 0, 3)
	knownAt := end.Add(2 * time.Hour)
	row := func(id, first, last, status string, availableAt time.Time) Valuation {
		return Valuation{Kind: "cost_coverage", ID: id, ObservedAt: availableAt, AvailableAt: availableAt,
			Coverage: &Coverage{FirstDay: first, LastDay: last, Categories: map[string]string{"llm": status}}}
	}
	index := indexValuations([]Valuation{
		row("original", "2026-11-16", "2026-11-18", "complete", end.Add(time.Hour)),
		row("revision", "2026-11-17", "2026-11-18", "zero", knownAt),
		row("future", "2026-11-16", "2026-11-17", "missing", knownAt.Add(time.Hour)),
	}, "")
	coverage := selectCostCoverage(index.coverage, start, end, knownAt)
	if len(coverage) != 3 || coverage["2026-11-16"]["llm"] != "complete" ||
		coverage["2026-11-17"]["llm"] != "zero" || coverage["2026-11-18"]["llm"] != "zero" {
		t.Fatalf("unexpected interval selection: %+v", coverage)
	}
	if got := selectCostCoverage(index.coverage, start, end, knownAt.Add(time.Hour)); got != nil {
		t.Fatalf("newer incomplete coverage was ignored: %+v", got)
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
