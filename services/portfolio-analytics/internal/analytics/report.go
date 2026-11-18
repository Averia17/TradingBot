package analytics

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"slices"
	"strings"
	"time"
)

type Policy struct {
	MarkMaxAge  time.Duration
	HeadMaxAge  time.Duration
	BenchmarkID string
}

type Report struct {
	SchemaVersion     string           `json:"schema_version"`
	ReportID          string           `json:"report_id"`
	AccountID         string           `json:"account_id"`
	Mode              string           `json:"mode"`
	Currency          string           `json:"currency"`
	Status            string           `json:"status"`
	AccountStatus     string           `json:"account_status"`
	AccountVersion    int64            `json:"account_version"`
	AsOf              time.Time        `json:"as_of"`
	ValuationAt       *time.Time       `json:"valuation_at"`
	LedgerVerifiedAt  *time.Time       `json:"ledger_verified_at"`
	CashUSD           string           `json:"cash_usd"`
	ReservedCashUSD   string           `json:"reserved_cash_usd"`
	FreeCashUSD       string           `json:"free_cash_usd"`
	PositionsValueUSD *string          `json:"positions_value_usd"`
	NAVUSD            *string          `json:"nav_usd"`
	RealizedPnLUSD    *string          `json:"realized_pnl_usd"`
	UnrealizedPnLUSD  *string          `json:"unrealized_pnl_usd"`
	Positions         []PositionReport `json:"positions"`
	Performance       Performance      `json:"performance"`
}

type PositionReport struct {
	Balance
	BasisUSD         string     `json:"basis_usd"`
	PriceUSD         *string    `json:"price_usd"`
	MarkObservedAt   *time.Time `json:"mark_observed_at"`
	MarkAvailableAt  *time.Time `json:"mark_available_at"`
	MarkSourceID     string     `json:"mark_source_id,omitempty"`
	MarkSourceKind   string     `json:"mark_source_kind,omitempty"`
	MarketValueUSD   *string    `json:"market_value_usd"`
	WeightPct        *string    `json:"weight_pct"`
	UnrealizedPnLUSD *string    `json:"unrealized_pnl_usd"`
	Status           string     `json:"status"`
}

type Performance struct {
	Period                   string     `json:"period"`
	Status                   string     `json:"status"`
	StartAt                  *time.Time `json:"start_at"`
	EndAt                    *time.Time `json:"end_at"`
	Observations             int        `json:"observations"`
	PnLUSD                   *string    `json:"pnl_usd"`
	RealizedPnLUSD           *string    `json:"realized_pnl_usd"`
	UnrealizedChangeUSD      *string    `json:"unrealized_change_usd"`
	ReturnPct                *string    `json:"return_pct"`
	BenchmarkID              string     `json:"benchmark_id,omitempty"`
	BenchmarkStatus          string     `json:"benchmark_status"`
	BenchmarkReturnPct       *string    `json:"benchmark_return_pct"`
	ExcessPctPoints          *string    `json:"excess_pct_points"`
	MaxDrawdownPct           *string    `json:"max_drawdown_pct"`
	ExecutionFeesUSD         *string    `json:"execution_fees_usd"`
	OperatingStatus          string     `json:"operating_status"`
	OperatingCostsUSD        *string    `json:"operating_costs_usd"`
	OperatingPnLUSD          *string    `json:"operating_pnl_usd"`
	OperatingReturnPct       *string    `json:"operating_return_pct"`
	OperatingExcessPctPoints *string    `json:"operating_excess_pct_points"`
}

type reportInput struct {
	accountID         string
	state             ledgerState
	head              AccountHead
	seen              int64
	events            []AccountEvent
	valuations        []Valuation
	now               time.Time
	streamUnavailable bool
}

var ErrNoAccount = errors.New("paper account history not available")

func buildReport(in reportInput, policy Policy, period string) (Report, error) {
	if in.state.Version == 0 {
		return Report{}, ErrNoAccount
	}
	verified := in.head.ObservedAt
	accountStatus := in.accountStatus(policy.HeadMaxAge)
	index := indexValuations(in.valuations, policy.BenchmarkID)
	state := in.state
	priced := evaluate(state, index, in.now, policy.MarkMaxAge, true)
	free := usdAmount(state.CashUSD)
	free.Sub(free, usdAmount(state.ReservedCashUSD))
	report := Report{
		SchemaVersion: WireVersion, AccountID: in.accountID, Mode: "paper", Currency: "USD",
		Status: "complete", AccountStatus: accountStatus, AccountVersion: state.Version, AsOf: state.KnownAt,
		CashUSD: state.CashUSD, ReservedCashUSD: state.ReservedCashUSD,
		FreeCashUSD:    amountString(free),
		RealizedPnLUSD: &state.RealizedUSD, Positions: priced.positions, Performance: pendingPerformance(period, policy.BenchmarkID),
	}
	if !verified.IsZero() {
		report.LedgerVerifiedAt = &verified
	}
	switch {
	case accountStatus != "current":
		report.Status = "stale"
		report.Performance.Status = "incomplete"
		for i := range report.Positions {
			position := &report.Positions[i]
			position.MarketValueUSD, position.UnrealizedPnLUSD, position.WeightPct = nil, nil, nil
		}
	case priced.nav == nil:
		report.Status = "incomplete"
	default:
		report.ValuationAt = &in.now
		report.NAVUSD = ratText(priced.nav)
		report.PositionsValueUSD = ratText(priced.positionsValue)
		report.UnrealizedPnLUSD = ratText(priced.unrealized)
	}
	if accountStatus == "current" {
		var err error
		report.Performance, err = calculatePerformance(in, policy, index, period)
		if err != nil {
			return Report{}, err
		}
	}
	payload, err := json.Marshal(report)
	if err != nil {
		return Report{}, err
	}
	digest := sha256.Sum256(payload)
	report.ReportID = hex.EncodeToString(digest[:16])
	return report, nil
}

func (in reportInput) accountStatus(maxAge time.Duration) string {
	switch {
	case in.seen > in.state.Version || in.head.AccountVersion > in.state.Version:
		return "gap"
	case in.head.ObservedAt.IsZero():
		return "missing_head"
	case in.streamUnavailable || in.head.AccountVersion != in.state.Version || in.head.ObservedAt.Before(in.state.KnownAt) ||
		in.head.ObservedAt.After(in.now) || in.now.Sub(in.head.ObservedAt) > maxAge:
		return "stale"
	default:
		return "current"
	}
}

type valuedState struct {
	nav            *big.Rat
	unrealized     *big.Rat
	positionsValue *big.Rat
	positions      []PositionReport
}

func evaluate(state ledgerState, index valuationIndex, cutoff time.Time, maxAge time.Duration, includePositions bool) valuedState {
	result := valuedState{positions: []PositionReport{}}
	positionsValue, openBasis := new(big.Rat), new(big.Int)
	var marketValues map[string]*big.Rat
	if includePositions {
		marketValues = make(map[string]*big.Rat, len(state.Positions))
	}
	complete := true
	for _, position := range state.Positions {
		mark := latestAt(index.marks[position.Instrument], cutoff)
		basis := position.basis()
		row := PositionReport{Balance: position.Balance, BasisUSD: amountString(basis), Status: "mark_missing"}
		var value *big.Rat
		if mark != nil {
			row.Status, row.PriceUSD = "mark_stale", &mark.Mark.PriceUSD
			row.MarkObservedAt, row.MarkAvailableAt = &mark.ObservedAt, &mark.AvailableAt
			row.MarkSourceID, row.MarkSourceKind = mark.SourceID, mark.SourceKind
			if cutoff.Sub(mark.ObservedAt) <= maxAge {
				denom := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(position.Decimals)), nil)
				quantity := new(big.Rat).SetFrac(rawAmount(position.QuantityRaw), denom)
				value = quantity.Mul(quantity, dollars(mark.Mark.PriceUSD))
				positionsValue.Add(positionsValue, value)
				openBasis.Add(openBasis, basis)
				row.Status, row.MarketValueUSD = "priced", ratText(value)
				row.UnrealizedPnLUSD = ratText(new(big.Rat).Sub(value, new(big.Rat).SetFrac(basis, usdScale)))
			}
		}
		complete = complete && value != nil
		if includePositions {
			marketValues[position.InstrumentID] = value
			result.positions = append(result.positions, row)
		}
	}
	if complete {
		result.positionsValue = positionsValue
		result.nav = new(big.Rat).Add(positionsValue, dollars(state.CashUSD))
		result.unrealized = new(big.Rat).Sub(positionsValue, new(big.Rat).SetFrac(openBasis, usdScale))
		if result.nav.Sign() > 0 {
			for i := range result.positions {
				row := &result.positions[i]
				row.WeightPct = ratText(percent(marketValues[row.InstrumentID], result.nav))
			}
		}
	}
	slices.SortFunc(result.positions, func(a, b PositionReport) int {
		if a.MarketValueUSD != nil && b.MarketValueUSD != nil {
			if cmp := dollars(*b.MarketValueUSD).Cmp(dollars(*a.MarketValueUSD)); cmp != 0 {
				return cmp
			}
		} else if a.MarketValueUSD != nil {
			return -1
		} else if b.MarketValueUSD != nil {
			return 1
		}
		return strings.Compare(a.InstrumentID, b.InstrumentID)
	})
	return result
}

func dollars(value string) *big.Rat { return new(big.Rat).SetFrac(usdAmount(value), usdScale) }

func ratText(value *big.Rat) *string {
	text := decimalString(value)
	return &text
}

func percent(numerator, denominator *big.Rat) *big.Rat {
	return new(big.Rat).Mul(new(big.Rat).Quo(numerator, denominator), big.NewRat(100, 1))
}
