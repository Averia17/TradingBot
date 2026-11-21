package quotes

import "testing"

func TestSweepRequiresFullExecutableDepthAndUsesExactAmounts(t *testing.T) {
	book := OrderBook{
		Bids: []BookLevel{{Price: "99.98", Quantity: "0.3"}, {Price: "99.90", Quantity: "1.2"}},
		Asks: []BookLevel{{Price: "100.01", Quantity: "0.2"}, {Price: "100.02", Quantity: "0.7"}},
	}
	got, err := EstimateRoundTrip(book, "0.501", 3)
	if err != nil || got.Status != "indicative" || *got.AskCost != "50.10802" ||
		*got.BidProceeds != "50.0739" || *got.SpreadAndImpactCost != "0.03412" {
		t.Fatalf("incorrect exact sweep %+v, error %v", got, err)
	}
	got, err = EstimateRoundTrip(book, "1", 3)
	if err != nil || got.Status != "insufficient_depth" || got.AskCost != nil || got.BidProceeds != nil {
		t.Fatalf("partial depth was represented as an executable size %+v, error %v", got, err)
	}
	if _, err = EstimateRoundTrip(book, "0.0001", 3); err == nil {
		t.Fatal("invalid quantity increment accepted")
	}
}
