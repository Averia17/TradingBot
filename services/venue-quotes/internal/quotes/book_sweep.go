package quotes

import (
	"fmt"
	"math/big"
	"strings"
)

// BookSweep estimates both sides from a single indicative snapshot in quote
// currency. Actual exit spreads, venue fees and funding are separate inputs.
type BookSweep struct {
	Quantity            string  `json:"quantity"`
	Status              string  `json:"status"`
	AskCost             *string `json:"ask_cost_quote"`
	BidProceeds         *string `json:"bid_proceeds_quote"`
	SpreadAndImpactCost *string `json:"spread_and_impact_cost_quote"`
}

func EstimateRoundTrip(book OrderBook, rawQuantity string, sizeDecimals int) (BookSweep, error) {
	quantity, size := positiveDecimal(rawQuantity)
	if quantity == nil || sizeDecimals < 0 || sizeDecimals > 24 {
		return BookSweep{}, fmt.Errorf("invalid quantity or lot precision")
	}
	_, fraction, _ := strings.Cut(size, ".")
	if len(fraction) > sizeDecimals {
		return BookSweep{}, fmt.Errorf("quantity does not align to the venue lot precision")
	}
	result := BookSweep{Quantity: size, Status: "insufficient_depth"}
	buy, err := sweepSide(book.Asks, quantity)
	if err != nil {
		return BookSweep{}, err
	}
	sell, err := sweepSide(book.Bids, quantity)
	if err != nil {
		return BookSweep{}, err
	}
	if buy == nil || sell == nil {
		return result, nil
	}
	cost := new(big.Rat).Sub(buy, sell)
	if cost.Sign() < 0 {
		return BookSweep{}, ErrUnavailable
	}
	result.Status = "indicative"
	result.AskCost, result.BidProceeds, result.SpreadAndImpactCost =
		sweepDecimal(buy), sweepDecimal(sell), sweepDecimal(cost)
	return result, nil
}

func sweepSide(levels []BookLevel, quantity *big.Rat) (*big.Rat, error) {
	remaining, total := new(big.Rat).Set(quantity), new(big.Rat)
	for _, level := range levels {
		price, _ := positiveDecimal(level.Price)
		size, _ := positiveDecimal(level.Quantity)
		if price == nil || size == nil {
			return nil, ErrUnavailable
		}
		if remaining.Cmp(size) < 0 {
			size = new(big.Rat).Set(remaining)
		}
		total.Add(total, new(big.Rat).Mul(price, size))
		remaining.Sub(remaining, size)
		if remaining.Sign() == 0 {
			return total, nil
		}
	}
	return nil, nil
}

func sweepDecimal(value *big.Rat) *string {
	// Both price and size accept at most 24 fractional places.
	text := strings.TrimRight(strings.TrimRight(value.FloatString(48), "0"), ".")
	return &text
}
