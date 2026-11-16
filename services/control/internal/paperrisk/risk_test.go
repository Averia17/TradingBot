package paperrisk

import (
	"errors"
	"testing"
	"time"

	"tradingbot/control/internal/ledger"
	"tradingbot/control/internal/papersim"
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
	intent := papersim.Intent{
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
	intent := papersim.Intent{
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
