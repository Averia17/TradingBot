package perp_research

import (
	"crypto/sha256"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
)

func studyFixture(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("../../../../contracts/valid-perp-research-study.json")
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestScreenConsidersBothSidesAndBoundsLeverage(t *testing.T) {
	raw := []byte(studyFixture(t))
	screen, err := Evaluate(raw)
	if err != nil {
		t.Fatal(err)
	}
	if screen.StudySHA256 != fmt.Sprintf("%x", sha256.Sum256(raw)) {
		t.Fatal("screen lost the exact input hash")
	}
	tsla, nvda := screen.Markets[0], screen.Markets[1]
	if tsla.Status != "eligible" || nvda.Status != "eligible" ||
		len(tsla.Options) != 4 || len(nvda.Options) != 8 {
		t.Fatalf("unexpected candidates: %+v %+v", tsla, nvda)
	}
	want := Option{
		ID: "FIXTURE.TSLA:SHORT:1200:2", Side: "SHORT", Leverage: 2,
		NotionalUSD: "1200", InitialMarginUSD: "600",
		StressLossUSD: "91.08", StressHeadroomUSD: "431.88",
		ProjectedFundingBPS: -8, EstimatedCostBPS: 16,
	}
	if !slices.Contains(tsla.Options, want) {
		t.Fatalf("short costs or margin changed: %+v", tsla.Options)
	}
	for _, option := range tsla.Options {
		if option.Leverage < 2 || option.Leverage > 3 {
			t.Fatalf("option exceeds collateral or venue limits: %+v", option)
		}
	}
}

func TestScreenRejectsIneligibleObservationsAndSizing(t *testing.T) {
	raw := studyFixture(t)
	for _, tt := range []struct {
		name, orig, edit, status string
	}{
		{"stale", "14:34:50Z", "14:32:50Z", "stale_observation"},
		{"unavailable", "14:34:51Z", "14:35:01Z", "not_yet_available"},
		{"basis", `"300.9"`, `"302"`, "mark_basis_above_limit"},
		{"collateral", `"1000"`, `"400"`, "no_eligible_options"},
		{"size", `"1500"`, `"1000"`, "no_eligible_options"},
		{"loss", `"110"`, `"91"`, "no_eligible_options"},
		{"maintenance", `"maintenance_buffer_bps": 100`, `"maintenance_buffer_bps": 5000`, "no_eligible_options"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			screen, err := Evaluate([]byte(strings.Replace(raw, tt.orig, tt.edit, 1)))
			if err != nil {
				t.Fatal(err)
			}
			if market := screen.Markets[0]; market.Status != tt.status || len(market.Options) != 0 {
				t.Fatalf("ineligible scenario escaped screening: %+v", market)
			}
		})
	}
}

func TestScreenRejectsMalformedOrAmbiguousStudy(t *testing.T) {
	raw := studyFixture(t)
	for _, tt := range []struct{ name, orig, edit string }{
		{"duplicate key", `"schema_version": "0.1",`, `"schema_version": "0.1", "schema_version": "0.1",`},
		{"missing costs", `"funding_stress_cost_bps": 40`, `"funding_unknown": true`},
		{"understated funding stress", `"funding_stress_cost_bps": 40`, `"funding_stress_cost_bps": 7`},
		{"numeric money", `"1000"`, "1000"},
		{"duplicate market", "FIXTURE.NVDA", "FIXTURE.TSLA"},
		{"ambiguous security", `"fixture-nvda"`, `"fixture-tsla"`},
		{"wrong product", "linear_usd_isolated_perpetual", "spot"},
		{"zero mark", `"300.9"`, `"0"`},
		{"reverse chronology", "14:34:51Z", "14:34:49Z"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if screen, err := Evaluate([]byte(strings.Replace(raw, tt.orig, tt.edit, 1))); err == nil {
				t.Fatalf("invalid study became a screen: %+v", screen)
			}
		})
	}
}
