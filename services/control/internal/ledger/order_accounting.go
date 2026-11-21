package ledger

import (
	"errors"
	"math/big"
)

// orderAccounting derives balances from an authorized order. The writer and
// historical replay both apply its signed changes to their account state.
type orderAccounting struct {
	request   ReservationRequest
	status    string
	remaining orderReserve
}

type orderReserve struct {
	quantity *big.Int
	fee      *big.Rat
}

type balanceChange struct {
	cash, reservedCash         *big.Rat
	quantity, reservedQuantity *big.Int
}

func newBalanceChange() balanceChange {
	return balanceChange{
		cash: new(big.Rat), reservedCash: new(big.Rat),
		quantity: new(big.Int), reservedQuantity: new(big.Int),
	}
}

func reserveOrder(request ReservationRequest) (orderAccounting, balanceChange) {
	order := orderAccounting{
		request: request,
		status:  "reserved",
		remaining: orderReserve{
			quantity: rawUnits(request.QuantityRaw),
			fee:      amount(request.MaxFeeUSD),
		},
	}
	change := newBalanceChange()
	if request.Side == "BUY" {
		change.reservedCash.Set(order.remaining.buyCash(request))
	} else {
		change.reservedQuantity.Set(order.remaining.quantity)
	}
	return order, change
}

func restoreOrderAccounting(order Order) orderAccounting {
	fee := amount(order.MaxFeeUSD)
	for _, fill := range order.Fills {
		fee.Sub(fee, amount(fill.FeeUSD))
	}
	return orderAccounting{
		request: order.ReservationRequest,
		status:  order.Status,
		remaining: orderReserve{
			quantity: rawUnits(order.RemainingQuantityRaw),
			fee:      fee,
		},
	}
}

func (o orderAccounting) resolve(outcome string, fill *Fill) (orderAccounting, balanceChange, error) {
	if o.closed() || (o.status == "unknown" && outcome == "unknown") {
		return orderAccounting{}, balanceChange{}, ErrConflict
	}
	if o.remaining.fee.Sign() < 0 {
		return orderAccounting{}, balanceChange{}, errors.New("stored paper fills exceed their fee allocation")
	}
	next := o
	next.status = outcome
	change := newBalanceChange()
	if outcome == "unknown" {
		return next, change, nil
	}

	remaining := orderReserve{quantity: new(big.Int), fee: new(big.Rat)}
	quantity, gross, fee := new(big.Int), new(big.Rat), new(big.Rat)
	switch outcome {
	case "cancelled":
	case "filled", "partially_filled":
		if fill == nil {
			return orderAccounting{}, balanceChange{}, ErrInvalid
		}
		quantity, gross, fee = rawUnits(fill.QuantityRaw), amount(fill.GrossUSD), amount(fill.FeeUSD)
		comparison := quantity.Cmp(o.remaining.quantity)
		if quantity.Sign() <= 0 || (outcome == "filled" && comparison != 0) || (outcome == "partially_filled" && comparison >= 0) ||
			fee.Cmp(o.remaining.fee) > 0 || (o.request.Side == "SELL" && fee.Cmp(gross) > 0) {
			return orderAccounting{}, balanceChange{}, ErrInvalid
		}
		if outcome == "partially_filled" {
			remaining = o.remaining.afterFill(quantity, fee)
		}
	default:
		return orderAccounting{}, balanceChange{}, ErrInvalid
	}
	if o.request.Side == "BUY" {
		change.cash.Neg(change.cash.Add(gross, fee))
		change.reservedCash.Neg(o.remaining.releasedBuyCash(o.request, remaining))
		change.quantity.Set(quantity)
	} else {
		change.cash.Sub(gross, fee)
		change.quantity.Neg(quantity)
		change.reservedQuantity.Neg(o.remaining.releasedQuantity(remaining))
	}
	next.remaining = remaining
	return next, change, nil
}

func (o orderAccounting) closed() bool {
	return o.status == "filled" || o.status == "cancelled"
}

func (r orderReserve) afterFill(quantity *big.Int, fee *big.Rat) orderReserve {
	return orderReserve{
		quantity: new(big.Int).Sub(r.quantity, quantity),
		fee:      new(big.Rat).Sub(r.fee, fee),
	}
}

func proportionalGross(order ReservationRequest, quantity *big.Int) *big.Rat {
	ratio := new(big.Rat).SetFrac(quantity, rawUnits(order.QuantityRaw))
	return ratio.Mul(ratio, amount(order.GrossUSD))
}

func (r orderReserve) buyCash(order ReservationRequest) *big.Rat {
	// Round the unsettled gross limit up to the next scale-8 unit. Preserve
	// enough cash for the entire remaining quantity at the authorized limit.
	scale := big.NewInt(100_000_000)
	gross := proportionalGross(order, r.quantity)
	numerator := new(big.Int).Mul(gross.Num(), scale)
	units, remainder := new(big.Int), new(big.Int)
	units.QuoRem(numerator, gross.Denom(), remainder)
	if remainder.Sign() != 0 {
		units.Add(units, big.NewInt(1))
	}
	rounded := new(big.Rat).SetFrac(units, scale)
	return rounded.Add(rounded, r.fee)
}

func (r orderReserve) releasedBuyCash(order ReservationRequest, after orderReserve) *big.Rat {
	released := r.buyCash(order)
	if after.quantity.Sign() != 0 {
		released.Sub(released, after.buyCash(order))
	}
	return released
}

func (r orderReserve) releasedQuantity(after orderReserve) *big.Int {
	return new(big.Int).Sub(r.quantity, after.quantity)
}
