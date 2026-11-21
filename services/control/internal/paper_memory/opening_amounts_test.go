package paper_memory

import "testing"

func TestAttributeOpeningConservesReconciledCashAcrossMixedFIFOFill(t *testing.T) {
	book := Snapshot{Realized: []Outcome{{
		FillEventID: eventID(22), QuantityRaw: "3", GrossUSD: "10.00000002", FeeUSD: "0.00000001",
		Lots: []LotAllocation{
			{BuyFillEventID: eventID(7), QuantityRaw: "1", CostBasisUSD: "4"},
			{BuyFillEventID: eventID(8), QuantityRaw: "2", CostBasisUSD: "5"},
		},
	}}}
	for _, c := range []struct {
		id                       int
		netCash, basis, realized string
	}{
		{7, "3.33333333", "4", "-0.66666667"},
		{8, "6.66666668", "5", "1.66666668"},
	} {
		attributed, err := AttributeOpening(book, map[string]struct{}{eventID(c.id): {}})
		if err != nil || attributed.RealizedNetCashUSD != c.netCash || attributed.RealizedCostBasisUSD != c.basis ||
			attributed.RealizedPnLUSD != c.realized || len(attributed.ExitFillIDs) != 1 || attributed.ExitFillIDs[0] != eventID(22) {
			t.Fatalf("mixed sale attribution for fill %d: %+v / %v", c.id, attributed, err)
		}
	}
}
