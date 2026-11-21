package paper_risk

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"tradingbot/control/internal/ledger"
	"tradingbot/control/internal/paper_sim"
)

func TestOptionalActivityLimits(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	instrument := ledger.Instrument{ID: "fixture:NVDAx", RegistryVersion: "fixture-v1", Decimals: 2}
	account := ledger.Account{
		ID: "paper-account", Version: 7, CashUSD: "800", ReservedCashUSD: "30",
		Positions: []ledger.Position{{Instrument: instrument, QuantityRaw: "400", ReservedQuantityRaw: "0"}},
	}
	policy := Policy{
		Version: "daily-v1", MaxOrderUSD: "250", MaxPositionBPS: 10000, MaxGroupBPS: 10000,
		MaxGrossBPS: 10000, MaxFeeBPS: 0, MinFreeCashUSD: "0", MaxMarkAgeSeconds: 60,
		Instruments: []InstrumentRule{{Instrument: instrument, Symbol: "NVDA", Group: "tech"}},
	}
	intent := paper_sim.Intent{
		AccountID: account.ID, SnapshotVersion: account.Version, Instrument: instrument,
		Side: "BUY", QuantityRaw: "200", GrossLimitUSD: "100", MaxFeeUSD: "0",
		DecidedAt: now, ExpiresAt: now.Add(time.Minute),
	}
	marks := []Mark{{Instrument: instrument, PriceUSD: "55", At: now, AvailableAt: now}}
	activity := ledger.RiskActivity{
		AccountID: account.ID, AccountVersion: account.Version, DayUTC: now.Truncate(24 * time.Hour),
		SettledGrossUSD: "100.00000001", HasUnknown: true,
		Pending: []ledger.RiskReservation{{Instrument: instrument, QuantityRaw: "300", RemainingQuantityRaw: "100", GrossUSD: "90"}},
	}
	raw, err := json.Marshal(policy)
	if err != nil || strings.Contains(string(raw), "max_daily_turnover_usd") || strings.Contains(string(raw), "block_new_buys_on_unknown") {
		t.Fatalf("legacy policy serialization = %s, %v", raw, err)
	}
	if err := CheckWithActivity(account, intent, policy, marks, now, &activity); err != nil {
		t.Fatalf("legacy policy should disregard activity: %v", err)
	}
	policy.MaxDailyTurnoverUSD = "265"
	if err := CheckWithActivity(account, intent, policy, marks, now, &activity); !errors.Is(err, ErrLimit) {
		t.Fatalf("settled and remaining marked gross plus candidate mark exceeds limit: %v", err)
	}
	policy.MaxDailyTurnoverUSD = "265.00000001"
	if err := CheckWithActivity(account, intent, policy, marks, now, &activity); err != nil {
		t.Fatalf("exact limit should pass: %v", err)
	}
	activity.Pending[0].GrossUSD = "180"
	if err := CheckWithActivity(account, intent, policy, marks, now, &activity); !errors.Is(err, ErrLimit) {
		t.Fatalf("remaining pro-rata gross higher than mark must count: %v", err)
	}
	if err := Check(account, intent, policy, marks, now); !errors.Is(err, ErrSnapshot) {
		t.Fatalf("configured policy must not pass without activity: %v", err)
	}
	activity.DayUTC = activity.DayUTC.AddDate(0, 0, -1)
	if err := CheckWithActivity(account, intent, policy, marks, now, &activity); !errors.Is(err, ErrSnapshot) {
		t.Fatalf("previous day's activity must not pass: %v", err)
	}
	activity.DayUTC = now.Truncate(24 * time.Hour)
	activity.AccountVersion--
	if err := CheckWithActivity(account, intent, policy, marks, now, &activity); !errors.Is(err, ErrSnapshot) {
		t.Fatalf("previous account version's activity must not pass: %v", err)
	}
	activity.AccountVersion = account.Version
	policy.MaxDailyTurnoverUSD = ""
	policy.BlockNewBuysOnUnknown = true
	if err := CheckWithActivity(account, intent, policy, marks, now, &activity); !errors.Is(err, ErrLimit) {
		t.Fatalf("unknown order must block new discretionary buys when enabled: %v", err)
	}
	activity.HasUnknown = false
	if err := CheckWithActivity(account, intent, policy, marks, now, &activity); err != nil {
		t.Fatalf("no unresolved order should pass unknown-only policy: %v", err)
	}
	policy.MaxDailyTurnoverUSD = "0"
	intent.Side = "SELL"
	if err := Check(account, intent, policy, marks, now); err != nil {
		t.Fatalf("a sell within held units should pass the original limits: %v", err)
	}
	policy.MaxDailyTurnoverUSD = "1.000000001"
	if !errors.Is(policy.Validate(), ErrPolicy) {
		t.Fatal("sub-precision USD must be rejected")
	}
}
