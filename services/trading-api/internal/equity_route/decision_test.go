package equity_route

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile("../../../../contracts/valid-equity-route-" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

type fixtureInputs struct {
	root, theses, account, policy  jsonObject
	security, thesis, small, large jsonObject
}

type jsonObject = map[string]any

func child(object jsonObject, key string) jsonObject { return object[key].(jsonObject) }

func decideFixture(t *testing.T, change func(fixtureInputs)) Decision {
	t.Helper()
	studyRaw, thesisRaw := readFixture(t, "study"), readFixture(t, "theses")
	if change != nil {
		var study, theses jsonObject
		if err := json.Unmarshal(studyRaw, &study); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(thesisRaw, &theses); err != nil {
			t.Fatal(err)
		}
		security := study["securities"].([]any)[0].(jsonObject)
		sizes := security["sizes"].([]any)
		change(fixtureInputs{
			root: study, theses: theses, account: child(study, "account"), policy: child(study, "policy"),
			security: security, thesis: theses["theses"].([]any)[0].(jsonObject),
			small: sizes[0].(jsonObject), large: sizes[1].(jsonObject),
		})
		var err error
		studyRaw, err = json.Marshal(study)
		if err != nil {
			t.Fatal(err)
		}
		theses["study_sha256"] = digest(studyRaw)
		thesisRaw, err = json.Marshal(theses)
		if err != nil {
			t.Fatal(err)
		}
	}
	study, err := LoadStudy(studyRaw)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := study.Decide(thesisRaw)
	if err != nil {
		t.Fatal(err)
	}
	return decision
}

func findCandidate(t *testing.T, d Decision, action, size string, leverage int) Candidate {
	t.Helper()
	for _, c := range d.Candidates {
		if c.Action == action && c.NotionalUSD == size && c.Leverage == leverage {
			return c
		}
	}
	t.Fatalf("missing candidate %s %s %dx", action, size, leverage)
	return Candidate{}
}

func TestLeverageRequiresIncrementalNetDollarsAndRiskAdjustedValue(t *testing.T) {
	d := decideFixture(t, nil)
	if d.Selected == nil || d.Action != "PERP_LONG" || d.Selected.Leverage != 2 ||
		d.Selected.NotionalUSD != "1500" || d.UnleveredBaseline.Action != "SPOT_BUY" ||
		*d.Selected.NetUSD != "28.5" || *d.Selected.StressLossUSD != "117.6" ||
		*d.Selected.ExtraNetUSD != "20.25" || *d.Selected.ExtraValueUSD != "17.847" ||
		*d.Selected.UncertaintyUsedBPS != 40 || d.AlphaMeasured {
		t.Fatalf("unexpected leverage decision: %+v %+v", d, d.Selected)
	}
	one := findCandidate(t, d, "PERP_LONG", "1500", 1)
	if *one.NetUSD != *d.Selected.NetUSD || *one.StressLossUSD != *d.Selected.StressLossUSD ||
		*one.RequiredCapitalUSD != "1514.1" || *d.Selected.RequiredCapitalUSD != "763.35" {
		t.Fatalf("margin changed the P&L at identical exposure: %+v %+v", one, d.Selected)
	}
	for _, test := range []struct {
		name   string
		change func(fixtureInputs)
		want   string
	}{
		{"incremental raw hurdle", func(f fixtureInputs) { f.policy["min_leverage_advantage_usd"] = "20.25" }, "insufficient_incremental_leverage_value"},
		{"incremental risk-adjusted hurdle", func(f fixtureInputs) { f.policy["min_leverage_advantage_usd"] = "18" }, "insufficient_incremental_leverage_value"},
		{"funding eats the advantage", func(f fixtureInputs) {
			funding := child(child(child(f.large, "perp"), "long"), "funding")
			funding["forecast_bps"], funding["stress_bps"] = 150, 150
		}, "insufficient_advantage_over_spot_or_cash"},
		{"stress loss too large", func(f fixtureInputs) { f.account["remaining_stress_budget_usd"] = "117.59999999" }, "position_risk_limit"},
		{"loss of isolated collateral too large", func(f fixtureInputs) { f.policy["max_perp_capital_at_risk_usd"] = "763.34999999" }, "perp_capital_at_risk_limit"},
		{"same exposure is available at 1x", func(f fixtureInputs) {
			f.account["perp_collateral_usd"], f.policy["max_perp_capital_at_risk_usd"] = "2000", "2000"
		}, "leverage_not_justified_at_same_exposure"},
		{"spot beats perp after the switching hurdle", func(f fixtureInputs) {
			f.account["spot_cash_usd"] = "2000"
		}, "insufficient_advantage_over_spot_or_cash"},
		{"stale paired spot", func(f fixtureInputs) {
			o := child(child(f.large, "spot"), "observation")
			o["observed_at"] = "2026-10-05T14:30:00Z"
		}, "paired_spot_observation_unavailable"},
		{"session changes before exit", func(f fixtureInputs) {
			child(f.large, "perp")["external_session_until"] = "2026-10-05T18:34:59Z"
		}, "external_session_does_not_cover_horizon"},
	} {
		t.Run(test.name, func(t *testing.T) {
			d := decideFixture(t, test.change)
			c := findCandidate(t, d, "PERP_LONG", "1500", 2)
			if c.Status != test.want || (d.Selected != nil && d.Selected.Leverage != 1) {
				t.Fatalf("unexpected leveraged result: %s, selected=%+v", c.Status, d.Selected)
			}
		})
	}
}

func TestShortUsesStressOnRisingPriceAndIgnoresFundingCredit(t *testing.T) {
	d := decideFixture(t, func(f fixtureInputs) { f.thesis["expected_price_return_bps"] = -250 })
	if d.Action != "PERP_SHORT" || d.Selected.Leverage != 1 ||
		*d.Selected.CappedFundingCostBPS != 0 || *d.Selected.NetUSD != "9.5" ||
		*d.Selected.StressLossUSD != "49.35" {
		t.Fatalf("unexpected short %+v", d.Selected)
	}
	d = decideFixture(t, func(f fixtureInputs) {
		f.thesis["expected_price_return_bps"] = -1000
		f.security["market_stress_up_bps"] = 5200
		f.account["remaining_stress_budget_usd"] = "2000"
	})
	c := findCandidate(t, d, "PERP_SHORT", "1500", 2)
	if c.Status != "insufficient_stress_margin_headroom" {
		t.Fatalf("short squeeze did not block leverage: %+v", c)
	}
}

func TestConfigurableLeveragePicksLowestFundableLevel(t *testing.T) {
	for _, test := range []struct {
		name, action, collateral string
		expectedReturn, leverage int
	}{
		{"2x is sufficient", "PERP_LONG", "800", 250, 2},
		{"3x long", "PERP_LONG", "525", 250, 3},
		{"3x short", "PERP_SHORT", "525", -250, 3},
		{"4x long", "PERP_LONG", "415", 250, 4},
	} {
		t.Run(test.name, func(t *testing.T) {
			d := decideFixture(t, func(f fixtureInputs) {
				f.policy["max_leverage"] = 5
				f.account["perp_collateral_usd"] = test.collateral
				f.account["remaining_stress_budget_usd"] = "160"
				f.thesis["expected_price_return_bps"] = test.expectedReturn
			})
			if d.Selected == nil || d.Action != test.action ||
				d.Selected.Leverage != test.leverage || d.Selected.NotionalUSD != "1500" {
				t.Fatalf("unexpected route: %+v", d.Selected)
			}
			lower := findCandidate(t, d, test.action, "1500", test.leverage-1)
			higher := findCandidate(t, d, test.action, "1500", test.leverage+1)
			if lower.Status != "insufficient_perp_collateral" ||
				higher.Status != "leverage_not_justified_at_same_exposure" ||
				*lower.NetUSD != *d.Selected.NetUSD || *higher.NetUSD != *d.Selected.NetUSD {
				t.Fatalf("leverage changed economics or ignored a lower viable level: %s %s", lower.Status, higher.Status)
			}
		})
	}
}

func TestHigherLeverageStillRequiresVenuePermissionAndEconomicAndRiskGates(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(fixtureInputs)
		limit  int
		status string
	}{
		{"legacy cap", func(f fixtureInputs) { delete(f.policy, "max_leverage") }, 2, ""},
		{"disabled", func(f fixtureInputs) { f.policy["allow_leverage"] = false }, 1, ""},
		{"explicit owner cap", func(f fixtureInputs) { f.policy["max_leverage"] = 2 }, 2, ""},
		{"venue cap", func(f fixtureInputs) { child(f.large, "perp")["venue_max_leverage"] = 2 }, 2, ""},
		{"stress margin", func(f fixtureInputs) {
			f.security["market_stress_down_bps"] = 2800
			f.account["remaining_stress_budget_usd"] = "600"
		}, 5, "insufficient_stress_margin_headroom"},
		{"incremental advantage", func(f fixtureInputs) {
			f.policy["min_leverage_advantage_usd"] = "18"
		}, 5, "insufficient_incremental_leverage_value"},
	} {
		t.Run(test.name, func(t *testing.T) {
			d := decideFixture(t, func(f fixtureInputs) {
				f.policy["max_leverage"] = 5
				f.account["perp_collateral_usd"] = "525"
				test.change(f)
			})
			if d.Selected == nil || d.Action != "SPOT_BUY" || d.Selected.Leverage != 1 {
				t.Fatalf("expected the spot fallback, got %+v", d.Selected)
			}
			for _, c := range d.Candidates {
				if c.NotionalUSD == "1500" && c.Leverage > test.limit {
					t.Fatalf("tested level beyond the cap: %+v", c)
				}
			}
			if test.status != "" {
				c := findCandidate(t, d, "PERP_LONG", "1500", 3)
				if c.Status != test.status {
					t.Fatalf("expected %s, got %s", test.status, c.Status)
				}
			}
		})
	}
}

func TestInvalidLeverageCapsFailClosed(t *testing.T) {
	raw := string(readFixture(t, "study"))
	for _, invalid := range []string{"0", "101", "2.5", `"5"`, "null"} {
		t.Run(invalid, func(t *testing.T) {
			replacement := `"allow_leverage": true, "max_leverage": ` + invalid + ","
			input := strings.Replace(raw, `"allow_leverage": true,`, replacement, 1)
			if _, err := LoadStudy([]byte(input)); err == nil {
				t.Fatalf("accepted invalid owner leverage cap %s", invalid)
			}
		})
	}
}

func TestLeverageMustBeatUnleveredOpportunityInAnotherStock(t *testing.T) {
	d := decideFixture(t, func(f fixtureInputs) {
		obs := jsonObject{
			"market_id": "fixture-spot-tsla", "observation_sha256": digest([]byte("tsla quote")),
			"observed_at": "2026-10-05T14:34:50Z", "available_at": "2026-10-05T14:34:51Z",
		}
		other := jsonObject{
			"security_id": "fixture-tsla", "symbol": "TSLA", "held_spot": nil,
			"corporate_action_window_clear": true, "market_stress_down_bps": 700, "market_stress_up_bps": 900,
			"sizes": []any{jsonObject{
				"notional_usd": "500", "perp": nil, "spot": jsonObject{
					"observation": obs, "costs": jsonObject{"base_bps": 45, "stress_bps": 50}, "exit_bps": 20,
				},
			}},
		}
		f.root["securities"] = append(f.root["securities"].([]any), other)
		forecast := jsonObject{}
		for key, value := range f.thesis {
			forecast[key] = value
		}
		forecast["security_id"], forecast["symbol"], forecast["expected_price_return_bps"] = "fixture-tsla", "TSLA", 600
		f.theses["theses"] = append(f.theses["theses"].([]any), forecast)
	})
	c := findCandidate(t, d, "PERP_LONG", "1500", 2)
	if d.Action != "SPOT_BUY" || d.Selected.Symbol != "TSLA" ||
		c.Status != "insufficient_incremental_leverage_value" || *c.ExtraNetUSD != "2.75" {
		t.Fatalf("leverage ignored the portfolio alternative: %+v, selected=%+v", c, d.Selected)
	}
}

func TestSpotReductionCannotReverseAnExistingPosition(t *testing.T) {
	d := decideFixture(t, func(f fixtureInputs) {
		f.thesis["expected_price_return_bps"] = -250
		f.security["held_spot"] = jsonObject{"market_id": "fixture-spot-nvda", "notional_usd": "1500"}
	})
	if d.Action != "SPOT_CLOSE" || len(d.Candidates) != 2 ||
		findCandidate(t, d, "SPOT_REDUCE", "500", 1).Status != "eligible" {
		t.Fatalf("unexpected existing position decision: %+v", d)
	}
}

func TestWeakThesisOrUnreconciledAccountAbstains(t *testing.T) {
	d := decideFixture(t, func(f fixtureInputs) { f.thesis["expected_price_return_bps"] = 35 })
	if d.Action != "NO_TRADE" || d.Selected != nil {
		t.Fatalf("weak forecast got a position: %+v", d.Selected)
	}
	d = decideFixture(t, func(f fixtureInputs) { f.security["corporate_action_window_clear"] = false })
	if d.Action != "NO_TRADE" ||
		findCandidate(t, d, "NO_TRADE", "1500", 1).Status != "corporate_action_window_unverified" {
		t.Fatal("unverified corporate action window got a position")
	}
	for _, key := range []string{"has_pending_orders", "has_open_perps"} {
		d := decideFixture(t, func(f fixtureInputs) { f.account[key] = true })
		if d.Action != "NO_TRADE" || d.Reason != "account_requires_lifecycle_reconciliation" {
			t.Fatalf("account with %s got an entry: %+v", key, d)
		}
	}
	d = decideFixture(t, func(f fixtureInputs) { f.policy["allow_leverage"] = false })
	for _, c := range d.Candidates {
		if c.Leverage > 1 {
			t.Fatal("leverage escaped the owner policy")
		}
	}
}

func TestStudyAndForecastMustBeBound(t *testing.T) {
	study, err := LoadStudy(readFixture(t, "study"))
	if err != nil {
		t.Fatal(err)
	}
	raw := readFixture(t, "theses")
	var theses jsonObject
	if err := json.Unmarshal(raw, &theses); err != nil {
		t.Fatal(err)
	}
	theses["study_sha256"] = digest([]byte("another study"))
	raw, err = json.Marshal(theses)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := study.Decide(raw); err == nil {
		t.Fatal("accepted unbound thesis")
	}
}
