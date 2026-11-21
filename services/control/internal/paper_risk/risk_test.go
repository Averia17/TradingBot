package paper_risk

import (
	"errors"
	"testing"
	"time"

	"tradingbot/control/internal/ledger"
	"tradingbot/control/internal/paper_sim"
)

func TestPaperRiskAppliesExactPortfolioLimits(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	ts := ledger.Instrument{ID: "fixture:TSLAx", RegistryVersion: "fixture-v1", Decimals: 2}
	nv := ledger.Instrument{ID: "fixture:NVDAx", RegistryVersion: "fixture-v1", Decimals: 2}
	policy := Policy{
		Version: "policy-v1", MaxOrderUSD: "250", MaxPositionBPS: 2000, MaxGroupBPS: 3000,
		MaxGrossBPS: 6000, MaxFeeBPS: 100, MinFreeCashUSD: "25", MaxMarkAgeSeconds: 60,
		Instruments: []InstrumentRule{{Instrument: ts, Symbol: "TSLA", Group: "tech"}, {Instrument: nv, Symbol: "NVDA", Group: "tech"}},
	}
	account := ledger.Account{
		ID: "paper-account", Version: 3, CashUSD: "800", ReservedCashUSD: "0",
		Positions: []ledger.Position{{Instrument: nv, QuantityRaw: "200", ReservedQuantityRaw: "0"}},
	}
	intent := paper_sim.Intent{
		AccountID: account.ID, SnapshotVersion: 3, Instrument: ts, Side: "BUY", QuantityRaw: "125",
		GrossLimitUSD: "100", MaxFeeUSD: "0.1", DecidedAt: now.Add(-10 * time.Second), ExpiresAt: now.Add(time.Minute),
	}
	marks := []Mark{
		{Instrument: ts, PriceUSD: "79.90", At: now.Add(-5 * time.Second), AvailableAt: now.Add(-4 * time.Second)},
		{Instrument: nv, PriceUSD: "100", At: now.Add(-5 * time.Second), AvailableAt: now.Add(-4 * time.Second)},
	}
	// The pending fee makes exactly $300 of exposure exceed 30% of $999.90 NAV.
	if err := Check(account, intent, policy, marks, now); !errors.Is(err, ErrLimit) {
		t.Fatalf("group cap: %v", err)
	}
	intent.GrossLimitUSD = "99.90"
	if err := Check(account, intent, policy, marks, now); err != nil {
		t.Fatalf("buy just inside group cap: %v", err)
	}
	account.ReservedCashUSD = "1"
	if err := Check(account, intent, policy, marks, now); !errors.Is(err, ErrLimit) {
		t.Fatalf("existing reserve must consume risk capacity: %v", err)
	}
	account.ReservedCashUSD = "0"
	marks[1].AvailableAt = now.Add(time.Nanosecond)
	if err := Check(account, intent, policy, marks, now); !errors.Is(err, ErrSnapshot) {
		t.Fatalf("future mark: %v", err)
	}
	marks[1].AvailableAt = now
	intent.SnapshotVersion++
	if err := Check(account, intent, policy, marks, now); !errors.Is(err, ErrSnapshot) {
		t.Fatalf("stale account version: %v", err)
	}
}

func TestPaperRiskAllowsReductionButStillChecksHeldUnitsAndFees(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	instrument := ledger.Instrument{ID: "fixture:NVDAx", RegistryVersion: "fixture-v1", Decimals: 8}
	account := ledger.Account{
		ID: "paper-account", Version: 2, CashUSD: "0", ReservedCashUSD: "0",
		Positions: []ledger.Position{{Instrument: instrument, QuantityRaw: "300000000", ReservedQuantityRaw: "100000000"}},
	}
	policy := Policy{
		Version: "small-caps", MaxOrderUSD: "300", MaxPositionBPS: 1000, MaxGroupBPS: 1000,
		MaxGrossBPS: 1000, MaxFeeBPS: 20, MinFreeCashUSD: "10", MaxMarkAgeSeconds: 30,
		Instruments: []InstrumentRule{{Instrument: instrument, Symbol: "NVDA", Group: "tech"}},
	}
	intent := paper_sim.Intent{
		AccountID: account.ID, SnapshotVersion: 2, Instrument: instrument, Side: "SELL",
		QuantityRaw: "200000000", GrossLimitUSD: "200", MaxFeeUSD: "0.4",
		DecidedAt: now.Add(-10 * time.Second), ExpiresAt: now.Add(time.Minute),
	}
	marks := []Mark{{Instrument: instrument, PriceUSD: "100", At: now, AvailableAt: now}}
	if err := Check(account, intent, policy, marks, now); err != nil {
		t.Fatalf("reduce an over-limit holding: %v", err)
	}
	intent.QuantityRaw = "200000001"
	if err := Check(account, intent, policy, marks, now); !errors.Is(err, ErrLimit) {
		t.Fatalf("sell reserved units: %v", err)
	}
	intent.QuantityRaw = "200000000"
	intent.MaxFeeUSD = "0.40000001"
	if err := Check(account, intent, policy, marks, now); !errors.Is(err, ErrLimit) {
		t.Fatalf("fee cap rounded upward: %v", err)
	}
}

func TestOwnerExecutionCostCapUsesWorstPriceAndExecutedPartialQuantity(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	instrument := ledger.Instrument{ID: "fixture:TSLA", RegistryVersion: "fixture-v1", Decimals: 2}
	account := ledger.Account{ID: "paper-account", Version: 1, CashUSD: "1000", ReservedCashUSD: "0",
		Positions: []ledger.Position{{Instrument: instrument, QuantityRaw: "500", ReservedQuantityRaw: "0"}}}
	policy := Policy{Version: "execution-budget", MaxOrderUSD: "150", MinFreeCashUSD: "0", MaxPositionBPS: 10000,
		MaxGroupBPS: 10000, MaxGrossBPS: 10000, MaxFeeBPS: 100, MaxMarkAgeSeconds: 60, MaxExecutionCostBPS: new(25),
		Instruments: []InstrumentRule{{Instrument: instrument, Symbol: "TSLA", Group: "tech"}}}
	marks := []Mark{{Instrument: instrument, PriceUSD: "20", At: now, AvailableAt: now}}
	intent := paper_sim.Intent{AccountID: account.ID, SnapshotVersion: account.Version, Instrument: instrument,
		Side: "BUY", QuantityRaw: "500", GrossLimitUSD: "100.15", MaxFeeUSD: "0.1",
		DecidedAt: now.Add(-time.Second), ExpiresAt: now.Add(time.Minute)}
	assertRefusal := func(err error) {
		t.Helper()
		reason, ok := RefusalReason(err)
		if !ok || reason != ledger.RiskMaximumExecutionCost {
			t.Fatalf("expected execution cost refusal, got %v", err)
		}
	}

	if err := Check(account, intent, policy, marks, now); err != nil {
		t.Fatalf("exact BUY cost boundary: %v", err)
	}
	intent.GrossLimitUSD = "100.15000001"
	assertRefusal(Check(account, intent, policy, marks, now))
	intent.GrossLimitUSD = "100.15"
	fill := &paper_sim.FixtureFill{QuantityRaw: "100", GrossUSD: "20.03", FeeUSD: "0.02"}
	if err := CheckOrder(account, intent, policy, marks, now, nil, fill); err != nil {
		t.Fatalf("exact executed cost boundary: %v", err)
	}
	fill.FeeUSD = "0.02000001"
	assertRefusal(CheckOrder(account, intent, policy, marks, now, nil, fill))

	intent.Side, intent.GrossLimitUSD = "SELL", "99.85"
	if err := Check(account, intent, policy, marks, now); err != nil {
		t.Fatalf("exact SELL cost boundary: %v", err)
	}
	intent.GrossLimitUSD = "99.84999999"
	assertRefusal(Check(account, intent, policy, marks, now))
	intent.GrossLimitUSD, intent.MaxFeeUSD = "100.1", "0.25000001"
	assertRefusal(Check(account, intent, policy, marks, now))

	policy.MaxExecutionCostBPS = new(0)
	intent.MaxFeeUSD = "0"
	if err := Check(account, intent, policy, marks, now); err != nil {
		t.Fatalf("zero-cost price improvement: %v", err)
	}
	intent.MaxFeeUSD = "0.00000001"
	assertRefusal(Check(account, intent, policy, marks, now))
	policy.MaxExecutionCostBPS = new(-1)
	if err := policy.Validate(); !errors.Is(err, ErrPolicy) {
		t.Fatalf("negative owner cost cap accepted: %v", err)
	}
}
