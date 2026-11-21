package paper_quotes

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
	"time"

	"tradingbot/control/internal/contract"
)

func readExample(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("../../../../contracts/valid-paper-quote-comparison.json")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestCompareEqualShareBuyOnExactFXAndIncrementalFees(t *testing.T) {
	result, err := Compare(readExample(t))
	if err != nil {
		t.Fatal(err)
	}
	if result.ExecutionEnabled || result.Source != "synthetic_fixture" || result.ReferenceUSD != "100" || len(result.Quotes) != 2 {
		t.Fatalf("comparison = %+v", result)
	}
	usdOn, usdC := result.Quotes[0], result.Quotes[1]
	if usdOn.Rank != 2 || usdOn.EffectiveUSD != "100.94" || usdOn.CostBPS != "94.00000000" {
		t.Fatalf("depeg conversion or quantity normalization = %+v", usdOn)
	}
	if usdC.Rank != 1 || usdC.SettlementUSD != "100" || usdC.EffectiveUSD != "100.25" || usdC.EmbeddedFeesUSD != "1" || usdC.AdditionalFeesUSD != "0.25" ||
		usdC.CostBPS != "25.00000000" || usdC.USDPerShare != "80.20000000" {
		t.Fatalf("embedded fees counted twice or precision changed = %+v", usdC)
	}
}

func TestRejectQuoteWithExpiredVersionOrUnequalGuaranteedShares(t *testing.T) {
	for _, example := range []struct {
		name, reason string
		change       func(*document)
	}{
		{"soft", "indicative_quote", func(d *document) {
			d.Quotes[0].Mode, d.Quotes[0].MinOutputRaw = "indicative", ""
		}},
		{"expired", "quote_stale", func(d *document) { d.Quotes[0].ExpiresAt = d.AsOf }},
		{"min output", "share_quantity_mismatch", func(d *document) { d.Quotes[0].MinOutputRaw = "49999999" }},
		{"wrong version", "registry_revision_mismatch", func(d *document) { d.Quotes[0].Revision = "tslaon-r0" }},
		{"unknown FX", "settlement_fx_missing", func(d *document) { d.Reference.Settlements = d.Reference.Settlements[:1] }},
		{"wrong settlement decimals", "settlement_fx_missing", func(d *document) { d.Quotes[0].Settlement.Decimals++ }},
		{"wrong program", "wrapper_identity_mismatch", func(d *document) { d.Quotes[0].Wrapper.Program = "fixture:alternate-program" }},
		{"issuer outside policy", "ineligible_wrapper", func(d *document) { d.Market.AllowedIssuers = d.Market.AllowedIssuers[:1] }},
		{"changed multiplier", "registry_changed_after_quote", func(d *document) {
			changed := d.Registry.Wrappers[1]
			changed.Revision, changed.EffectiveAt = "tslaon-r2", d.AsOf
			changed.SharesPerToken.Numerator = "7"
			d.Registry.Wrappers = append(d.Registry.Wrappers, changed)
		}},
		{"late ratio", "wrapper_unknown_at_observation", func(d *document) { d.Registry.Wrappers[1].AvailableAt = d.AsOf }},
		{"cross chain", "ineligible_chain", func(d *document) { d.Quotes[0].Settlement.Chain = "fixture:chain-b" }},
	} {
		t.Run(example.name, func(t *testing.T) {
			d := parseExample(t)
			example.change(&d)
			result := runExample(t, d)
			if result.Quotes[0].Reason != example.reason || result.Quotes[0].Rank != 0 || result.Quotes[1].Rank != 1 {
				t.Fatalf("expected %s and only one ranked quote: %+v", example.reason, result.Quotes)
			}
		})
	}
}

func TestPreciseRawRankingAndPITRevision(t *testing.T) {
	d := parseExample(t)
	selected := d.Quotes[1]
	selected.Fees = []fee{}
	selected.Settlement.Decimals = 18
	d.Reference.Settlements[0].Token = selected.Settlement
	more, less := selected, selected
	more.ID, less.ID = "a-costs-more", "z-costs-less"
	more.InputRaw, less.InputRaw = "100000000000000000001", "100000000000000000000"
	d.Quotes = []quote{more, less}
	change := d.Registry.Wrappers[0]
	change.Revision, change.EffectiveAt = "tslax-future", d.AsOf.Add(time.Second)
	change.SharesPerToken.Numerator = "2"
	d.Registry.Wrappers = append(d.Registry.Wrappers, change)
	result := runExample(t, d)
	a, b := result.Quotes[0], result.Quotes[1]
	if a.Rank != 2 || b.Rank != 1 || a.EffectiveUSD != "100.000000000000000001" || b.EffectiveUSD != "100" ||
		a.USDPerShare != b.USDPerShare || a.CostBPS != b.CostBPS {
		t.Fatalf("precise rank rounded too early or future ratio used: %+v", result.Quotes)
	}
}

func TestSellUsesHeldMintAndConservativeProceeds(t *testing.T) {
	d := parseExample(t)
	d.Market.Side = "SELL"
	d.Market.Holding = &holding{Token: d.Quotes[1].Wrapper, QuantityRaw: "1250000"}
	wrong, right := &d.Quotes[0], &d.Quotes[1]
	wrong.Side, wrong.InputRaw, wrong.ExpectedOutputRaw, wrong.MinOutputRaw = "SELL", "50000000", "12000000000", "11900000000"
	right.Side, right.InputRaw, right.ExpectedOutputRaw, right.MinOutputRaw = "SELL", "1250000", "110000000", "99000000"
	result := runExample(t, d)
	if result.Quotes[0].Reason != "wrong_held_wrapper" || result.Quotes[0].Rank != 0 || result.Quotes[1].Rank != 1 || result.Quotes[1].SettlementUSD != "99" || result.Quotes[1].EffectiveUSD != "98.75" {
		t.Fatalf("sell accepted a different mint or ranked by indicative proceeds: %+v", result.Quotes)
	}
	d.Market.Holding.QuantityRaw = "1249999"
	result = runExample(t, d)
	if result.Quotes[1].Reason != "insufficient_held_quantity" || result.Quotes[1].Rank != 0 {
		t.Fatalf("sell accepted quantity beyond holdings: %+v", result.Quotes[1])
	}
}

func TestPaperQuoteComparisonRejectsIncompleteOrAmbiguousWireData(t *testing.T) {
	fixture := readExample(t)
	for _, modified := range [][]byte{
		bytes.Replace(fixture, []byte(`"numerator": "5"`), []byte(`"numerator": "0"`), 1),
		bytes.Replace(fixture, []byte(`"input_raw": "9800000000"`), []byte(`"input_raw": 9800000000`), 1),
		bytes.Replace(fixture, []byte(`"source_mode": "synthetic_fixture"`), []byte(`"source_mode": "external"`), 1),
		bytes.Replace(fixture, []byte(`"mode": "synthetic_firm"`), []byte(`"mode": "indicative", "mode": "synthetic_firm"`), 1),
	} {
		if _, err := Compare(modified); err == nil {
			t.Fatal("accepted malformed or untrusted comparison")
		}
	}
	d := parseExample(t)
	d.Quotes[1].Fees[1].ID = d.Quotes[1].Fees[0].ID
	result := runExample(t, d)
	if result.Quotes[1].Reason != "duplicate_fee_id" {
		t.Fatalf("accepted repeated fee identity: %+v", result.Quotes[1])
	}
}

func TestPublishedContractMatchesEmbeddedAndValidatesResult(t *testing.T) {
	public, err := os.ReadFile("../../../../contracts/paper-quote-comparison.schema.json")
	if err != nil || !bytes.Equal(public, schemaDocument) {
		t.Fatalf("published comparison schema differs from embedded schema: %v", err)
	}
	d := parseExample(t)
	d.Quotes[0].Mode, d.Quotes[0].MinOutputRaw = "indicative", ""
	result := runExample(t, d)
	payload, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Result
	if err := contract.Decode(payload, compileSchema("#/$defs/Result"), &decoded); err != nil {
		t.Fatalf("published result violates the quote comparison schema: %v", err)
	}
}

func parseExample(t *testing.T) document {
	t.Helper()
	var data document
	if err := json.Unmarshal(readExample(t), &data); err != nil {
		t.Fatal(err)
	}
	return data
}

func runExample(t *testing.T, data document) Result {
	t.Helper()
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	result, err := Compare(raw)
	if err != nil {
		t.Fatal(err)
	}
	return result
}
