package equity_study

import (
	"encoding/json"
	"fmt"
	"math/big"
	"time"

	"tradingbot/venue-quotes/internal/hyperliquid"
	"tradingbot/venue-quotes/internal/quotes"
)

type costBand struct {
	Base   int `json:"base_bps"`
	Stress int `json:"stress_bps"`
}

type spotQuote struct {
	Observation observation `json:"observation"`
	Costs       costBand    `json:"costs"`
	Exit        int         `json:"exit_bps"`
}

type fundingBand struct {
	Forecast int `json:"forecast_bps"`
	Stress   int `json:"stress_bps"`
}

type sideCosts struct {
	Execution costBand    `json:"execution"`
	Basis     costBand    `json:"basis"`
	Funding   fundingBand `json:"funding"`
}

type perpQuote struct {
	Observation  observation `json:"observation"`
	SessionUntil *time.Time  `json:"external_session_until"`
	MarkBasis    int         `json:"mark_basis_bps"`
	MarginMode   string      `json:"margin_mode"`
	MaxLeverage  int         `json:"venue_max_leverage"`
	Maintenance  int         `json:"maintenance_margin_bps"`
	Long         sideCosts   `json:"long"`
	Short        sideCosts   `json:"short"`
}

func readSpot(input spotInput) (*market, error) {
	var book quotes.OrderBook
	if err := decode(input.Book, &book); err != nil {
		return nil, fmt.Errorf("invalid spot book: %w", err)
	}
	if input.Provider != "dzengi" && input.Provider != "binance" ||
		book.Symbol != input.MarketSymbol || book.Provider != input.Provider ||
		book.Firmness != "indicative" || book.RequestedAt.IsZero() || book.ObservedAt.Before(book.RequestedAt) ||
		book.BookRequest.Validate() != nil || !inRange(input.SizeDecimals, 24) ||
		len(book.Bids) > book.Depth || len(book.Asks) > book.Depth {
		return nil, fmt.Errorf("spot book does not match its configured market")
	}
	obsTime := book.RequestedAt
	switch input.Provider {
	case "dzengi":
		providerTime := time.UnixMilli(book.ProviderTimestamp)
		if book.ProviderTimestamp <= 0 || providerTime.After(book.ObservedAt) {
			return nil, fmt.Errorf("invalid spot provider timestamp")
		}
		obsTime = earlier(obsTime, providerTime)
	case "binance":
		if book.ProviderSequence <= 0 || book.ProviderTimestamp != 0 {
			return nil, fmt.Errorf("invalid spot provider sequence")
		}
	}
	book, err := quotes.NormalizeBook(book, levelPairs(book.Bids), levelPairs(book.Asks))
	if err != nil {
		return nil, err
	}
	return newMarket(input.marketInput, input.Book, book, *input.SizeDecimals, obsTime, book.ObservedAt)
}

func readPerp(input perpInput, symbol string) (*perpetual, error) {
	capture, tiers, err := hyperliquid.Replay(input.Capture, symbol)
	if err != nil {
		return nil, fmt.Errorf("perpetual capture: %w", err)
	}
	stress := positive(input.FundingStressRate)
	if stress == nil || stress.Cmp(big.NewRat(1, 1)) > 0 ||
		input.CollateralIndex == nil || *input.CollateralIndex != capture.CollateralToken {
		return nil, fmt.Errorf("invalid funding stress assumption or collateral identity")
	}
	mkt, err := newMarket(input.marketInput, input.Capture, capture.OrderBook,
		*capture.Market.SizeDecimals, capture.OldestMarketTime(), capture.AvailableAt)
	if err != nil {
		return nil, err
	}
	return &perpetual{market: *mkt, capture: capture, tiers: tiers, sessionUntil: input.SessionUntil, fundingStress: stress}, nil
}

func newMarket(input marketInput, raw []byte, book quotes.OrderBook, decimals int, observed, available time.Time) (*market, error) {
	shares, fx, fee := positive(input.Shares), positive(input.USDPerQuote), amount(input.FixedFee)
	if !identifier.MatchString(input.MarketID) || shares == nil || fx == nil || fee == nil || fee.Sign() < 0 ||
		!inRange(input.TakerFee, bps) || !inRange(input.StressAdd, bps) ||
		!inRange(input.BasisCost, bps) || !inRange(input.BasisStress, bps) || *input.BasisStress < *input.BasisCost {
		return nil, fmt.Errorf("invalid market units or cost assumptions")
	}
	obs := observation{SHA256: sha(raw), MarketID: input.MarketID, ObservedAt: observed.UTC(), AvailableAt: available.UTC()}
	return &market{input: input, observation: obs, book: book, sizeDecimals: decimals, shares: shares, fx: fx, fixedFee: fee}, nil
}

func (m market) shareLot() *big.Rat {
	return new(big.Rat).Quo(m.shares, new(big.Rat).SetInt(pow10(m.sizeDecimals)))
}

func (m market) quantity(shares *big.Rat) *big.Rat {
	return new(big.Rat).Quo(shares, m.shares)
}

func (m market) sweep(shares, notional *big.Rat) (base, exit int, complete bool, err error) {
	qty := m.quantity(shares)
	swept, err := quotes.EstimateRoundTrip(m.book, qty.FloatString(m.sizeDecimals), m.sizeDecimals)
	if err != nil || swept.Status != "indicative" {
		return 0, 0, false, err
	}
	ask, _ := new(big.Rat).SetString(*swept.AskCost)
	bid, _ := new(big.Rat).SetString(*swept.BidProceeds)
	impact := new(big.Rat).Sub(ask, bid)
	feeRatio := big.NewRat(int64(*m.input.TakerFee), bps)
	roundTrip := new(big.Rat).Add(impact, new(big.Rat).Mul(new(big.Rat).Add(ask, bid), feeRatio))
	roundTrip.Mul(roundTrip, m.fx).Add(roundTrip, new(big.Rat).Mul(m.fixedFee, big.NewRat(2, 1)))

	bestBid, _ := new(big.Rat).SetString(m.book.Bids[0].Price)
	bestAsk, _ := new(big.Rat).SetString(m.book.Asks[0].Price)
	midValue := new(big.Rat).Mul(new(big.Rat).Add(bestBid, bestAsk), qty)
	midValue.Quo(midValue, big.NewRat(2, 1))
	exitCost := new(big.Rat).Sub(midValue, bid)
	exitCost.Add(exitCost, new(big.Rat).Mul(bid, feeRatio)).Mul(exitCost, m.fx).Add(exitCost, m.fixedFee)
	base, err = ceilBPS(roundTrip, notional)
	if err != nil {
		return 0, 0, false, err
	}
	exit, err = ceilBPS(exitCost, notional)
	return base, exit, true, err
}

func (m market) spot(shares, notional *big.Rat) (*spotQuote, error) {
	base, exit, complete, err := m.sweep(shares, notional)
	if err != nil || !complete {
		return nil, err
	}
	stress := base + *m.input.StressAdd + *m.input.BasisStress
	if stress > bps {
		return nil, fmt.Errorf("spot cost exceeds supported bps range")
	}
	return &spotQuote{
		Observation: m.observation, Costs: costBand{Base: base + *m.input.BasisCost, Stress: stress},
		Exit: exit + *m.input.BasisCost,
	}, nil
}

func (p perpetual) perp(shares, notional *big.Rat, hours, shock int) (*perpQuote, error) {
	base, _, complete, err := p.sweep(shares, notional)
	if err != nil || !complete {
		return nil, err
	}
	if base+*p.input.StressAdd > bps {
		return nil, fmt.Errorf("perpetual execution cost exceeds supported bps range")
	}
	quantity := p.quantity(shares)
	mark, _ := new(big.Rat).SetString(p.capture.Context.MarkPrice)
	oracle, _ := new(big.Rat).SetString(p.capture.Context.OraclePrice)
	rate, _ := new(big.Rat).SetString(p.capture.Context.Funding)
	marked := new(big.Rat).Mul(quantity, mark)
	markedUSD := new(big.Rat).Mul(marked, p.fx)
	gap := new(big.Rat).Abs(new(big.Rat).Sub(markedUSD, notional))
	markBasis, err := ceilBPS(gap, notional)
	if err != nil {
		return nil, err
	}
	maxLeverage := p.capture.Market.MaxLeverage
	// Use the tier at the adverse marked notional and charge its marginal
	// maintenance rate on the full exposure, without any tier deduction.
	stressMultiplier := big.NewRat(int64(bps+shock+*p.input.BasisStress), bps)
	stressedMark := new(big.Rat).Quo(notional, p.fx)
	if marked.Cmp(stressedMark) > 0 {
		stressedMark.Set(marked)
	}
	stressedMark.Mul(stressedMark, stressMultiplier)
	for _, tier := range p.tiers {
		if stressedMark.Cmp(tier.LowerBound) < 0 {
			break
		}
		maxLeverage = tier.MaxLeverage
	}
	fundingNotional := new(big.Rat).Mul(quantity, oracle)
	fundingNotional.Mul(fundingNotional, p.fx).Mul(fundingNotional, big.NewRat(int64(hours), 1))
	projected := new(big.Rat).Mul(fundingNotional, rate)
	long, err := ceilBPS(projected, notional)
	if err != nil {
		return nil, err
	}
	short, err := ceilBPS(new(big.Rat).Neg(projected), notional)
	if err != nil {
		return nil, err
	}
	stressRate := new(big.Rat).Abs(rate)
	if stressRate.Cmp(p.fundingStress) < 0 {
		stressRate = p.fundingStress
	}
	stressCash := new(big.Rat).Mul(fundingNotional, stressRate)
	stressCash.Mul(stressCash, stressMultiplier)
	fundingStress, err := ceilBPS(stressCash, notional)
	if err != nil {
		return nil, err
	}
	mode := "unknown"
	if p.capture.Market.MarginMode != nil {
		switch *p.capture.Market.MarginMode {
		case "strictIsolated":
			mode = "strict_isolated"
		case "noCross":
			mode = "isolated"
		}
	}
	costs := sideCosts{
		Execution: costBand{Base: base, Stress: base + *p.input.StressAdd},
		Basis:     costBand{Base: *p.input.BasisCost, Stress: *p.input.BasisStress},
		Funding:   fundingBand{Forecast: long, Stress: fundingStress},
	}
	shortCosts := costs
	shortCosts.Funding.Forecast = short
	return &perpQuote{
		Observation: p.observation, SessionUntil: p.sessionUntil, MarkBasis: markBasis, MarginMode: mode,
		MaxLeverage: maxLeverage, Maintenance: (5000 + maxLeverage - 1) / maxLeverage,
		Long: costs, Short: shortCosts,
	}, nil
}

func levelPairs(rows []quotes.BookLevel) [][]json.Number {
	if rows == nil {
		return nil
	}
	pairs := make([][]json.Number, 0, len(rows))
	for _, row := range rows {
		pairs = append(pairs, []json.Number{json.Number(row.Price), json.Number(row.Quantity)})
	}
	return pairs
}

func commonLot(a, b *big.Rat) *big.Rat {
	if a == nil {
		return b
	}
	numGCD := new(big.Int).GCD(nil, nil, a.Num(), b.Num())
	numLCM := new(big.Int).Quo(new(big.Int).Mul(a.Num(), b.Num()), numGCD)
	denGCD := new(big.Int).GCD(nil, nil, a.Denom(), b.Denom())
	return new(big.Rat).SetFrac(numLCM, denGCD)
}

func pow10(scale int) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(scale)), nil)
}

func exactAtScale(value *big.Rat, scale int) bool {
	return new(big.Int).Mod(pow10(scale), value.Denom()).Sign() == 0
}

func upward(value *big.Rat, scale int64) *big.Rat {
	factor := big.NewInt(scale)
	numerator := new(big.Int).Mul(value.Num(), factor)
	numerator.Neg(numerator).Div(numerator, value.Denom()).Neg(numerator)
	return new(big.Rat).SetFrac(numerator, factor)
}

func ceilBPS(cost, notional *big.Rat) (int, error) {
	ratio := new(big.Rat).Quo(cost, notional)
	rounded := upward(ratio, bps)
	units := new(big.Rat).Mul(rounded, big.NewRat(bps, 1)).Num()
	if !units.IsInt64() || units.Int64() < -bps || units.Int64() > bps {
		return 0, fmt.Errorf("normalized value exceeds supported bps range")
	}
	return int(units.Int64()), nil
}
