package analytics

import (
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
)

const scaleDigits = 8

var usdScale = big.NewInt(100_000_000)

type ledgerState struct {
	Version         int64              `json:"version"`
	KnownAt         time.Time          `json:"known_at"`
	FundedAt        time.Time          `json:"funded_at"`
	InitialCashUSD  string             `json:"initial_cash_usd"`
	CashUSD         string             `json:"cash_usd"`
	ReservedCashUSD string             `json:"reserved_cash_usd"`
	RealizedUSD     string             `json:"realized_usd"`
	FeesUSD         string             `json:"fees_usd"`
	Positions       map[string]holding `json:"positions"`
}

type holding struct {
	Balance
	Lots []lot `json:"lots"`
}

type lot struct {
	Quantity string `json:"quantity_raw"`
	Basis    string `json:"basis_usd"`
	Consumed string `json:"consumed_raw"`
}

func newState() ledgerState {
	return ledgerState{
		CashUSD: "0", InitialCashUSD: "0", ReservedCashUSD: "0", RealizedUSD: "0", FeesUSD: "0",
		Positions: make(map[string]holding),
	}
}

// apply consumes a validated, contiguous ledger event. Cash and quantities in
// the event are authoritative. Independent fill arithmetic catches divergence.
func (s *ledgerState) apply(e AccountEvent) error {
	if e.AccountVersion != s.Version+1 {
		return errors.New("account event version is not contiguous")
	}
	if s.Version == 0 {
		if e.Kind != "paper_account.created" {
			return errors.New("account history must begin with funding")
		}
		s.FundedAt, s.InitialCashUSD = e.RecordedAt, e.CashUSD
	} else if err := s.applyOrder(e); err != nil {
		return err
	}
	s.Version, s.CashUSD, s.ReservedCashUSD = e.AccountVersion, e.CashUSD, e.ReservedCashUSD
	if e.RecordedAt.After(s.KnownAt) {
		s.KnownAt = e.RecordedAt
	}
	return nil
}

func (s *ledgerState) applyOrder(e AccountEvent) error {
	if e.Position == nil {
		return errors.New("order event is missing its position balance")
	}
	balance := *e.Position
	position := s.Positions[balance.InstrumentID]
	if len(position.Lots) != 0 && position.Instrument != balance.Instrument {
		return errors.New("held instrument identity changed")
	}
	expectedCash := usdAmount(s.CashUSD)
	expectedQuantity := rawAmount(position.QuantityRaw)
	if f := e.Fill; f != nil {
		quantity := rawAmount(f.QuantityRaw)
		gross, fee := usdAmount(f.GrossUSD), usdAmount(f.FeeUSD)
		if f.Side == "BUY" {
			basis := gross.Add(gross, fee)
			expectedCash.Sub(expectedCash, basis)
			expectedQuantity.Add(expectedQuantity, quantity)
			position.Lots = append(position.Lots, lot{Quantity: f.QuantityRaw, Basis: amountString(basis), Consumed: "0"})
		} else {
			proceeds := gross.Sub(gross, fee)
			expectedCash.Add(expectedCash, proceeds)
			expectedQuantity.Sub(expectedQuantity, quantity)
			basis, err := position.sell(quantity)
			if err != nil {
				return err
			}
			realized := usdAmount(s.RealizedUSD)
			s.RealizedUSD = amountString(realized.Add(realized, proceeds.Sub(proceeds, basis)))
		}
		s.FeesUSD = amountString(fee.Add(fee, usdAmount(s.FeesUSD)))
	}
	if expectedCash.Cmp(usdAmount(e.CashUSD)) != 0 || expectedQuantity.Cmp(rawAmount(balance.QuantityRaw)) != 0 {
		return fmt.Errorf("fill arithmetic disagrees with ledger at version %d", e.AccountVersion)
	}
	if e.Kind == "paper_order.unknown" && (usdAmount(s.ReservedCashUSD).Cmp(usdAmount(e.ReservedCashUSD)) != 0 ||
		rawAmount(position.ReservedQuantityRaw).Cmp(rawAmount(balance.ReservedQuantityRaw)) != 0) {
		return errors.New("unknown resolution changed a reservation")
	}
	position.Balance = balance
	if balance.QuantityRaw == "0" {
		delete(s.Positions, balance.InstrumentID)
	} else {
		s.Positions[balance.InstrumentID] = position
	}
	return nil
}

func (p *holding) sell(quantity *big.Int) (*big.Int, error) {
	remaining := new(big.Int).Set(quantity)
	soldBasis := new(big.Int)
	for remaining.Sign() > 0 && len(p.Lots) != 0 {
		current := &p.Lots[0]
		size, consumed := rawAmount(current.Quantity), rawAmount(current.Consumed)
		take := new(big.Int).Sub(size, consumed)
		if take.Cmp(remaining) > 0 {
			take.Set(remaining)
		}
		priorBasis := allocatedBasis(current, consumed)
		consumed.Add(consumed, take)
		allocated := allocatedBasis(current, consumed)
		soldBasis.Add(soldBasis, allocated.Sub(allocated, priorBasis))
		remaining.Sub(remaining, take)
		if size.Cmp(consumed) == 0 {
			p.Lots[0] = lot{}
			p.Lots = p.Lots[1:]
		} else {
			current.Consumed = consumed.String()
		}
	}
	if remaining.Sign() != 0 {
		return nil, errors.New("sell exceeds open FIFO lots")
	}
	return soldBasis, nil
}

func (p holding) basis() *big.Int {
	total := new(big.Int)
	for _, current := range p.Lots {
		open := usdAmount(current.Basis)
		total.Add(total, open.Sub(open, allocatedBasis(&current, rawAmount(current.Consumed))))
	}
	return total
}

// Match Control's fifo_recording_order_usd_scale8_v1. Each sale takes the
// increment of the cumulatively rounded share, including the final remainder.
func allocatedBasis(l *lot, consumed *big.Int) *big.Int {
	units := usdAmount(l.Basis)
	return units.Quo(units.Mul(units, consumed), rawAmount(l.Quantity))
}

func rawAmount(value string) *big.Int {
	if value == "" {
		return new(big.Int)
	}
	amount, ok := new(big.Int).SetString(value, 10)
	if !ok {
		panic("validated integer could not be decoded")
	}
	return amount
}

func usdAmount(value string) *big.Int {
	whole, fraction, _ := strings.Cut(value, ".")
	return rawAmount(whole + fraction + strings.Repeat("0", scaleDigits-len(fraction)))
}

func amountString(units *big.Int) string {
	return decimalString(new(big.Rat).SetFrac(units, usdScale))
}

func decimalString(value *big.Rat) string {
	text := strings.TrimRight(strings.TrimRight(value.FloatString(scaleDigits), "0"), ".")
	if text == "-0" {
		return "0"
	}
	return text
}
