package paper_memory

import (
	"errors"
	"math/big"
)

// OpeningAmounts attributes reconciled amounts only to the selected opening
// fills. Residual cost basis is the basis still held at the snapshot cutoff.
type OpeningAmounts struct {
	ExitFillIDs          []string `json:"exit_fill_event_ids"`
	RealizedQuantityRaw  string   `json:"realized_quantity_raw"`
	RealizedNetCashUSD   string   `json:"realized_net_cash_usd"`
	RealizedCostBasisUSD string   `json:"realized_cost_basis_usd"`
	RealizedPnLUSD       string   `json:"realized_net_pnl_usd"`
	OpenQuantityRaw      string   `json:"open_quantity_raw"`
	OpenCostBasisUSD     string   `json:"open_cost_basis_usd"`
}

// AttributeOpening uses the projection's FIFO match order. A sale that spans
// multiple openings allocates its net cash by cumulative raw quantity, flooring
// to scale 8 at allocation boundaries. The final allocation absorbs the
// rounding remainder, so attribution across all openings conserves cash.
func AttributeOpening(book Snapshot, fills map[string]struct{}) (OpeningAmounts, error) {
	out := OpeningAmounts{ExitFillIDs: []string{}}
	var realized, netCash, costBasis, open, openBasis big.Int
	for _, sale := range book.Realized {
		total, ok := parseUnits(sale.QuantityRaw)
		gross, grossErr := usdUnits(sale.GrossUSD)
		fee, feeErr := usdUnits(sale.FeeUSD)
		if !ok || grossErr != nil || feeErr != nil {
			return OpeningAmounts{}, errors.New("invalid projected sale amounts")
		}
		// Allocate net cash directly, so a matched lot never receives negative
		// cash solely from rounding gross and fee in opposite directions.
		proceeds := new(big.Int).Sub(gross, fee)
		if proceeds.Sign() < 0 {
			return OpeningAmounts{}, errors.New("negative projected sale proceeds")
		}
		include := false
		var matched big.Int
		for _, lot := range sale.Lots {
			units, ok := parseUnits(lot.QuantityRaw)
			if !ok {
				return OpeningAmounts{}, errors.New("invalid projected sale match quantity")
			}
			if _, selected := fills[lot.BuyFillEventID]; selected {
				basis, err := usdUnits(lot.CostBasisUSD)
				if err != nil {
					return OpeningAmounts{}, err
				}
				before := proratedUnits(proceeds, &matched, total)
				after := proratedUnits(proceeds, new(big.Int).Add(&matched, units), total)
				netCash.Add(&netCash, after.Sub(after, before))
				realized.Add(&realized, units)
				costBasis.Add(&costBasis, basis)
				include = true
			}
			matched.Add(&matched, units)
		}
		if matched.Cmp(total) != 0 {
			return OpeningAmounts{}, errors.New("projected sale matches do not reconcile to filled units")
		}
		if include {
			out.ExitFillIDs = append(out.ExitFillIDs, sale.FillEventID)
		}
	}
	for _, lot := range book.OpenLots {
		if _, selected := fills[lot.BuyFillEventID]; !selected {
			continue
		}
		units, ok := parseUnits(lot.QuantityRaw)
		basis, err := usdUnits(lot.CostBasisUSD)
		if !ok || err != nil {
			return OpeningAmounts{}, errors.New("invalid projected open lot")
		}
		open.Add(&open, units)
		openBasis.Add(&openBasis, basis)
	}
	out.RealizedQuantityRaw = realized.String()
	out.RealizedNetCashUSD = formatUSD(&netCash)
	out.RealizedCostBasisUSD = formatUSD(&costBasis)
	out.RealizedPnLUSD = formatUSD(new(big.Int).Sub(&netCash, &costBasis))
	out.OpenQuantityRaw = open.String()
	out.OpenCostBasisUSD = formatUSD(&openBasis)
	return out, nil
}

func parseUnits(value string) (*big.Int, bool) {
	if !quantityPattern.MatchString(value) {
		return nil, false
	}
	return new(big.Int).SetString(value, 10)
}

// proratedUnits floors the allocation at the cumulative quantity boundary.
func proratedUnits(amount, quantity, total *big.Int) *big.Int {
	scaled := new(big.Int).Mul(amount, quantity)
	return scaled.Quo(scaled, total)
}
