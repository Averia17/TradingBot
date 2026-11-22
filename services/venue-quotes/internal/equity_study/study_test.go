package equity_study

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"tradingbot/venue-quotes/internal/hyperliquid"
)

func fixture(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile("../../../../contracts/valid-equity-study-input.json")
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func withInput(t *testing.T, change func(*Request)) []byte {
	t.Helper()
	var in Request
	if err := json.Unmarshal(fixture(t), &in); err != nil {
		t.Fatal(err)
	}
	change(&in)
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func editCapture(t *testing.T, in *Request, change func(*hyperliquid.Capture)) {
	t.Helper()
	var capture hyperliquid.Capture
	if err := json.Unmarshal(in.Securities[0].Perp.Capture, &capture); err != nil {
		t.Fatal(err)
	}
	change(&capture)
	raw, err := json.Marshal(capture)
	if err != nil {
		t.Fatal(err)
	}
	in.Securities[0].Perp.Capture = raw
}

func TestBuildNormalizesUnitsAndChargesFundingOnOracle(t *testing.T) {
	raw := fixture(t)
	study, err := Build(raw)
	if err != nil {
		t.Fatal(err)
	}
	if study.InputSHA256 != sha(raw) || study.Source != "synthetic_fixture" {
		t.Fatal("study lost input provenance")
	}
	sizes := study.Securities[0].Sizes
	if len(sizes) != 2 || sizes[0].Notional != "500.00000000" || sizes[1].Notional != "1500.00000000" {
		t.Fatalf("unexpected common lot sizes %+v", sizes)
	}
	small, large := sizes[0], sizes[1]
	if small.Spot.Costs != (costBand{Base: 37, Stress: 72}) ||
		large.Spot.Costs != (costBand{Base: 36, Stress: 71}) || small.Spot.Exit != 21 {
		t.Fatalf("incorrect spread, fees or fixed cost rounding: %+v %+v", small.Spot, large.Spot)
	}
	perp := small.Perp
	if perp.MarkBasis != 10 || perp.MaxLeverage != 10 || perp.Maintenance != 500 ||
		perp.Long.Execution != (costBand{Base: 8, Stress: 18}) ||
		perp.Long.Funding != (fundingBand{Forecast: 2, Stress: 27}) ||
		perp.Short.Funding != (fundingBand{Forecast: -1, Stress: 27}) {
		t.Fatalf("incorrect derivative economics: %+v", perp)
	}
	var in Request
	if err := json.Unmarshal(raw, &in); err != nil {
		t.Fatal(err)
	}
	if small.Spot.Observation.SHA256 != sha(in.Securities[0].Spot.Book) ||
		perp.Observation.SHA256 != sha(in.Securities[0].Perp.Capture) {
		t.Fatal("observation digests do not bind the archived inputs")
	}
}

func TestBuildUsesStressedMarginTierForSelectedSize(t *testing.T) {
	raw := withInput(t, func(in *Request) {
		editCapture(t, in, func(c *hyperliquid.Capture) {
			meta := strings.Replace(string(c.MetaResponse.Bytes), `"marginTableId":10`, `"marginTableId":51`, 1)
			meta = strings.Replace(meta, `"collateralToken":0`, `"collateralToken":0,"marginTables":[[51,{"marginTiers":[{"lowerBound":"0","maxLeverage":10},{"lowerBound":"1600","maxLeverage":3}]}]]`, 1)
			c.MetaResponse.Bytes = []byte(meta)
			c.MetaResponse.SHA256 = sha(c.MetaResponse.Bytes)
		})
	})
	study, err := Build(raw)
	if err != nil {
		t.Fatal(err)
	}
	sizes := study.Securities[0].Sizes
	if sizes[0].Perp.MaxLeverage != 10 || sizes[1].Perp.MaxLeverage != 3 ||
		sizes[1].Perp.Maintenance != 1667 {
		t.Fatalf("adverse move did not carry the larger position into the stricter tier: %+v", sizes[1].Perp)
	}
}

func TestBuildDropsRouteWhenDepthCannotFillBothLegs(t *testing.T) {
	raw := withInput(t, func(in *Request) {
		editCapture(t, in, func(c *hyperliquid.Capture) {
			book := strings.Replace(string(c.BookResponse.Bytes), `"sz":"20"`, `"sz":"8"`, 1)
			c.BookResponse.Bytes = []byte(book)
			c.BookResponse.SHA256 = sha(c.BookResponse.Bytes)
		})
	})
	study, err := Build(raw)
	if err != nil {
		t.Fatal(err)
	}
	sizes := study.Securities[0].Sizes
	if sizes[0].Perp == nil || sizes[1].Perp != nil || sizes[1].Spot == nil {
		t.Fatal("partial depth was treated as a complete route")
	}
}

func TestBuildRejectsInvalidObservationInputs(t *testing.T) {
	for _, test := range []struct {
		name, want string
		edit       func(*Request)
	}{
		{"future as of", "future perpetual", func(in *Request) { in.AsOf = in.AsOf.Add(-8 * time.Second) }},
		{"excessive skew", "maximum skew", func(in *Request) { in.MaxSkew = 2 }},
		{"bad archive hash", "invalid archived provider response", func(in *Request) {
			editCapture(t, in, func(c *hyperliquid.Capture) { c.BookResponse.SHA256 = strings.Repeat("0", 64) })
		}},
		{"provider book from the future", "timestamp follows its receipt", func(in *Request) {
			editCapture(t, in, func(c *hyperliquid.Capture) {
				c.BookResponse.ReceivedAt = c.BookResponse.RequestedAt
				c.AvailableAt = c.BookResponse.ReceivedAt
			})
		}},
		{"wrong book identity", "does not match", func(in *Request) {
			in.Securities[0].Spot.MarketSymbol = "TSLABUSDT"
		}},
		{"missing fees", "invalid market units or cost assumptions", func(in *Request) {
			in.Securities[0].Spot.TakerFee = nil
		}},
		{"below lot", "no requested size", func(in *Request) {
			in.Securities[0].Budgets = []string{"19.99999999"}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Build(withInput(t, test.edit)); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("expected %q, got %v", test.want, err)
			}
		})
	}
}
