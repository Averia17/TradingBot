package quotes

import (
	"context"
	"math/big"
)

type RoundTripObservation struct {
	Buy          Observation `json:"buy"`
	Sell         Observation `json:"sell"`
	QuoteCostRaw string      `json:"quote_cost_raw"`
	QuoteCostBPS string      `json:"quote_cost_bps"`
	BuyVenues    []string    `json:"buy_venues,omitempty"`
	SellVenues   []string    `json:"sell_venues,omitempty"`
}

// RoundTrip prices two exact-input legs. It requests quotes only and does not
// estimate costs that are absent from the provider's quoted token amounts.
func RoundTrip(ctx context.Context, request Request, quote func(context.Context, Request) (Observation, error)) (RoundTripObservation, error) {
	if err := request.Validate(); err != nil {
		return RoundTripObservation{}, err
	}
	buy, err := quote(ctx, request)
	if err != nil {
		return RoundTripObservation{}, err
	}
	reverse := Request{
		Network: request.Network, InputMint: request.OutputMint,
		OutputMint: request.InputMint, InAmount: buy.OutAmount,
	}
	if buy.Request != request || reverse.Validate() != nil {
		return RoundTripObservation{}, ErrUnavailable
	}
	sell, err := quote(ctx, reverse)
	if err != nil {
		return RoundTripObservation{}, err
	}
	input, _ := PositiveAmount(request.InAmount)
	output, validOutput := PositiveAmount(sell.OutAmount)
	if sell.Request != reverse || !validOutput || sell.RequestedAt.Before(buy.ObservedAt) ||
		(buy.ProviderExpiresAt != nil && !buy.ProviderExpiresAt.After(sell.ObservedAt)) {
		return RoundTripObservation{}, ErrUnavailable
	}

	inputUnits := new(big.Int).SetUint64(input)
	cost := new(big.Int).Sub(inputUnits, new(big.Int).SetUint64(output))
	costBPS := new(big.Rat).SetFrac(new(big.Int).Mul(cost, big.NewInt(10_000)), inputUnits).FloatString(4)
	if costBPS == "-0.0000" {
		costBPS = "0.0000"
	}
	return RoundTripObservation{
		Buy: buy, Sell: sell, QuoteCostRaw: cost.String(), QuoteCostBPS: costBPS,
		BuyVenues: buy.RouteVenues, SellVenues: sell.RouteVenues,
	}, nil
}
