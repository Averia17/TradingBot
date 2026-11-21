// Package paper_risk enforces an explicit, local paper portfolio policy.
package paper_risk

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"time"

	"tradingbot/control/internal/contract"
	"tradingbot/control/internal/ledger"
	"tradingbot/control/internal/paper_sim"
)

var (
	ErrPolicy   = errors.New("invalid paper risk policy")
	ErrSnapshot = errors.New("invalid or stale paper risk snapshot")
	ErrLimit    = errors.New("paper risk limit exceeded")

	idPattern       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:._-]{0,127}$`)
	usdPattern      = regexp.MustCompile(`^(?:0|[1-9][0-9]{0,29})(?:\.[0-9]{1,8})?$`)
	quantityPattern = regexp.MustCompile(`^(?:0|[1-9][0-9]{0,37})$`)
)

type limitError struct {
	reason ledger.RiskRefusalReason
	label  string
}

func (e limitError) Error() string { return fmt.Sprintf("%s: %s", ErrLimit, e.label) }
func (e limitError) Unwrap() error { return ErrLimit }

// RefusalReason returns the stable code for an evaluated risk limit violation.
func RefusalReason(err error) (ledger.RiskRefusalReason, bool) {
	var limit limitError
	if !errors.As(err, &limit) {
		return "", false
	}
	return limit.reason, true
}

type Policy struct {
	Version               string           `json:"policy_version"`
	MaxOrderUSD           string           `json:"max_order_usd"`
	MinFreeCashUSD        string           `json:"min_free_cash_usd"`
	MaxPositionBPS        int              `json:"max_position_bps"`
	MaxGroupBPS           int              `json:"max_group_bps"`
	MaxGrossBPS           int              `json:"max_gross_bps"`
	MaxFeeBPS             int              `json:"max_fee_bps"`
	MaxMarkAgeSeconds     int              `json:"max_mark_age_seconds"`
	Instruments           []InstrumentRule `json:"instruments"`
	MaxDailyTurnoverUSD   string           `json:"max_daily_turnover_usd,omitempty"`
	BlockNewBuysOnUnknown bool             `json:"block_new_buys_on_unknown,omitempty"`
	MaxExecutionCostBPS   *int             `json:"max_execution_cost_bps,omitempty"`
}

type InstrumentRule struct {
	Instrument ledger.Instrument `json:"instrument"`
	Symbol     string            `json:"symbol"`
	Group      string            `json:"group"`
}

type Mark struct {
	Instrument  ledger.Instrument `json:"instrument"`
	PriceUSD    string            `json:"price_usd"`
	At          time.Time         `json:"at"`
	AvailableAt time.Time         `json:"available_at"`
}

func (p Policy) rules() (map[ledger.Instrument]string, error) {
	if !idPattern.MatchString(p.Version) || amount(p.MaxOrderUSD) == nil || amount(p.MaxOrderUSD).Sign() <= 0 ||
		amount(p.MinFreeCashUSD) == nil || !validLimit(p.MaxPositionBPS) || !validLimit(p.MaxGroupBPS) ||
		!validLimit(p.MaxGrossBPS) || !validCostBPS(p.MaxFeeBPS) ||
		(p.MaxExecutionCostBPS != nil && !validCostBPS(*p.MaxExecutionCostBPS)) ||
		p.MaxMarkAgeSeconds < 1 || p.MaxMarkAgeSeconds > 3600 || len(p.Instruments) == 0 || len(p.Instruments) > 100 ||
		(p.MaxDailyTurnoverUSD != "" && amount(p.MaxDailyTurnoverUSD) == nil) {
		return nil, ErrPolicy
	}
	groups := make(map[ledger.Instrument]string, len(p.Instruments))
	ids := make(map[string]bool, len(p.Instruments))
	symbols := make([]string, 0, len(p.Instruments))
	for _, rule := range p.Instruments {
		i := rule.Instrument
		if !idPattern.MatchString(i.ID) || !idPattern.MatchString(i.RegistryVersion) || i.Decimals < 0 || i.Decimals > 18 ||
			!idPattern.MatchString(rule.Group) || ids[i.ID] {
			return nil, ErrPolicy
		}
		ids[i.ID] = true
		symbols = append(symbols, rule.Symbol)
		groups[i] = rule.Group
	}
	if contract.CheckSymbols(symbols) != nil {
		return nil, ErrPolicy
	}
	return groups, nil
}

// Validate checks an explicit policy before any snapshot or execution is read.
func (p Policy) Validate() error {
	_, err := p.rules()
	return err
}

func validLimit(bps int) bool   { return bps > 0 && bps <= 10000 }
func validCostBPS(bps int) bool { return bps >= 0 && bps <= 10000 }

// ActivityReader supplies the current ledger activity required by an opted-in policy.
type ActivityReader interface {
	RiskActivity(context.Context, string, int64) (ledger.RiskActivity, error)
}

// RequiresActivity identifies intents that must pass the daily and unresolved order controls.
func (p Policy) RequiresActivity(side string) bool {
	return side == "BUY" && (p.MaxDailyTurnoverUSD != "" || p.BlockNewBuysOnUnknown)
}

// Check validates intents whose policy does not require ledger activity.
func Check(account ledger.Account, intent paper_sim.Intent, policy Policy, marks []Mark, now time.Time) error {
	return CheckWithActivity(account, intent, policy, marks, now, nil)
}

// CheckWithActivity validates against the exact account version. The subsequent ledger
// reservation checks that same version under lock, rejecting races with fills
// and reservations. An unclassified cash reserve counts toward every affected
// BUY cap so missing pending-order details cannot amplify exposure.
func CheckWithActivity(account ledger.Account, intent paper_sim.Intent, policy Policy, marks []Mark, now time.Time, activity *ledger.RiskActivity) error {
	return CheckOrder(account, intent, policy, marks, now, activity, nil)
}

// CheckOrder also checks the deterministic fill preview at its actual size.
func CheckOrder(account ledger.Account, intent paper_sim.Intent, policy Policy, marks []Mark, now time.Time, activity *ledger.RiskActivity, fill *paper_sim.FixtureFill) error {
	groups, err := policy.rules()
	if err != nil {
		return err
	}
	if now.IsZero() || account.ID == "" || account.ID != intent.AccountID || account.Version < 1 || account.Version != intent.SnapshotVersion ||
		intent.DecidedAt.IsZero() || intent.DecidedAt.After(now) || !intent.ExpiresAt.After(now) ||
		(intent.Side != "BUY" && intent.Side != "SELL") || len(account.Positions) > 100 || len(marks) > 100 {
		return ErrSnapshot
	}
	group, enabled := groups[intent.Instrument]
	if !enabled {
		return limitError{ledger.RiskInstrumentNotAllowed, "instrument is outside the policy"}
	}
	prices, err := validatedMarks(marks, groups, now, time.Duration(policy.MaxMarkAgeSeconds)*time.Second)
	if err != nil {
		return err
	}
	cash, reserved := amount(account.CashUSD), amount(account.ReservedCashUSD)
	units := rawUnits(intent.QuantityRaw)
	grossLimit, fee := amount(intent.GrossLimitUSD), amount(intent.MaxFeeUSD)
	price := prices[intent.Instrument]
	if cash == nil || reserved == nil || cash.Cmp(reserved) < 0 || units == nil || units.Sign() <= 0 ||
		grossLimit == nil || grossLimit.Sign() <= 0 || fee == nil || price == nil {
		return ErrSnapshot
	}
	notional := markedValue(units, price, intent.Instrument.Decimals)
	maxNotional := maxAmount(notional, grossLimit)
	if maxNotional.Cmp(amount(policy.MaxOrderUSD)) > 0 {
		return limitError{ledger.RiskOrderNotional, "order notional"}
	}
	if exceeds(fee, minAmount(notional, grossLimit), policy.MaxFeeBPS) {
		return limitError{ledger.RiskMaximumFee, "maximum fee"}
	}
	if policy.MaxExecutionCostBPS != nil {
		if err := checkExecutionCost(intent, *policy.MaxExecutionCostBPS, units, price, notional, grossLimit, fee, fill); err != nil {
			return err
		}
	}
	book, err := valueBook(account.Positions, groups, prices, intent.Instrument, group)
	if err != nil {
		return err
	}
	if intent.Side == "SELL" {
		if units.Cmp(book.freeTarget) > 0 {
			return limitError{ledger.RiskHeldQuantity, "available held quantity"}
		}
		return nil
	}
	if policy.RequiresActivity(intent.Side) {
		if err := checkActivity(account, policy, prices, now, maxNotional, activity); err != nil {
			return err
		}
	}
	freeAfterBuy := new(big.Rat).Sub(cash, reserved)
	freeAfterBuy.Sub(freeAfterBuy, grossLimit).Sub(freeAfterBuy, fee)
	if freeAfterBuy.Cmp(amount(policy.MinFreeCashUSD)) < 0 {
		return limitError{ledger.RiskMinimumFreeCash, "minimum free cash"}
	}
	nav := new(big.Rat).Add(cash, book.gross)
	nav.Sub(nav, fee)
	additional := new(big.Rat).Add(maxNotional, reserved)
	for _, limit := range []struct {
		exposure *big.Rat
		bps      int
		reason   ledger.RiskRefusalReason
		label    string
	}{
		{book.target, policy.MaxPositionBPS, ledger.RiskInstrumentConcentration, "instrument concentration"},
		{book.group, policy.MaxGroupBPS, ledger.RiskGroupConcentration, "correlated group concentration"},
		{book.gross, policy.MaxGrossBPS, ledger.RiskGrossExposure, "portfolio gross exposure"},
	} {
		if nav.Sign() <= 0 || exceeds(new(big.Rat).Add(limit.exposure, additional), nav, limit.bps) {
			return limitError{limit.reason, limit.label}
		}
	}
	return nil
}

func checkExecutionCost(intent paper_sim.Intent, bps int, units *big.Int, price, notional, grossLimit, fee *big.Rat, fill *paper_sim.FixtureFill) error {
	// The reservation's directional limit and fee cap bound the total cost
	// authorized at the requested quantity, including later reconciliation.
	refused := limitError{ledger.RiskMaximumExecutionCost, "maximum execution cost"}
	if exceeds(AdverseExecutionCost(intent.Side, grossLimit, notional, fee), notional, bps) {
		return refused
	}
	if fill == nil {
		return nil
	}
	executed := rawUnits(fill.QuantityRaw)
	gross, actualFee := amount(fill.GrossUSD), amount(fill.FeeUSD)
	if executed == nil || executed.Sign() <= 0 || executed.Cmp(units) > 0 || gross == nil || gross.Sign() <= 0 || actualFee == nil {
		return ErrSnapshot
	}
	reference := markedValue(executed, price, intent.Instrument.Decimals)
	if exceeds(AdverseExecutionCost(intent.Side, gross, reference, actualFee), reference, bps) {
		return refused
	}
	return nil
}

// AdverseExecutionCost counts adverse movement from a prevalidated mark plus
// explicit fees. An improved execution price does not offset the fees.
func AdverseExecutionCost(side string, gross, reference, fee *big.Rat) *big.Rat {
	adverse := new(big.Rat).Sub(gross, reference)
	if side == "SELL" {
		adverse.Neg(adverse)
	}
	if adverse.Sign() > 0 {
		return adverse.Add(adverse, fee)
	}
	return fee
}

func checkActivity(account ledger.Account, policy Policy, prices map[ledger.Instrument]*big.Rat, now time.Time, candidate *big.Rat, activity *ledger.RiskActivity) error {
	if activity == nil || activity.AccountID != account.ID || activity.AccountVersion != account.Version ||
		!activity.DayUTC.Equal(now.UTC().Truncate(24*time.Hour)) {
		return ErrSnapshot
	}
	if policy.BlockNewBuysOnUnknown && activity.HasUnknown {
		return limitError{ledger.RiskUnresolvedOrder, "unresolved paper order"}
	}
	if policy.MaxDailyTurnoverUSD == "" {
		return nil
	}
	turnover := amount(activity.SettledGrossUSD)
	if turnover == nil {
		return ErrSnapshot
	}
	turnover.Add(turnover, candidate)
	for _, open := range activity.Pending {
		units, remaining := rawUnits(open.QuantityRaw), rawUnits(open.RemainingQuantityRaw)
		gross, price := amount(open.GrossUSD), prices[open.Instrument]
		if units == nil || remaining == nil || remaining.Sign() <= 0 || units.Cmp(remaining) < 0 || gross == nil || gross.Sign() <= 0 || price == nil {
			return ErrSnapshot
		}
		limit := new(big.Rat).Mul(gross, new(big.Rat).SetFrac(remaining, units))
		marked := markedValue(remaining, price, open.Instrument.Decimals)
		turnover.Add(turnover, maxAmount(limit, marked))
	}
	if turnover.Cmp(amount(policy.MaxDailyTurnoverUSD)) > 0 {
		return limitError{ledger.RiskDailyTurnover, "daily gross turnover"}
	}
	return nil
}

func validatedMarks(marks []Mark, groups map[ledger.Instrument]string, now time.Time, maxAge time.Duration) (map[ledger.Instrument]*big.Rat, error) {
	prices := make(map[ledger.Instrument]*big.Rat, len(marks))
	for _, mark := range marks {
		_, enabled := groups[mark.Instrument]
		price := amount(mark.PriceUSD)
		if !enabled || prices[mark.Instrument] != nil || price == nil || price.Sign() <= 0 || mark.At.IsZero() ||
			mark.AvailableAt.Before(mark.At) || mark.AvailableAt.After(now) || now.Sub(mark.At) > maxAge {
			return nil, ErrSnapshot
		}
		prices[mark.Instrument] = price
	}
	return prices, nil
}

type exposure struct {
	freeTarget           *big.Int
	target, group, gross *big.Rat
}

func valueBook(positions []ledger.Position, groups map[ledger.Instrument]string, prices map[ledger.Instrument]*big.Rat, target ledger.Instrument, group string) (exposure, error) {
	out := exposure{freeTarget: new(big.Int), target: new(big.Rat), group: new(big.Rat), gross: new(big.Rat)}
	seen := make(map[ledger.Instrument]bool, len(positions))
	for _, p := range positions {
		price := prices[p.Instrument]
		units, reserved := rawUnits(p.QuantityRaw), rawUnits(p.ReservedQuantityRaw)
		if seen[p.Instrument] || price == nil || units == nil || units.Sign() <= 0 || reserved == nil || units.Cmp(reserved) < 0 {
			return exposure{}, ErrSnapshot
		}
		seen[p.Instrument] = true
		value := markedValue(units, price, p.Decimals)
		out.gross.Add(out.gross, value)
		if groups[p.Instrument] == group {
			out.group.Add(out.group, value)
		}
		if p.Instrument == target {
			out.freeTarget.Sub(units, reserved)
			out.target.Set(value)
		}
	}
	return out, nil
}

func amount(value string) *big.Rat {
	if !usdPattern.MatchString(value) {
		return nil
	}
	n, _ := new(big.Rat).SetString(value)
	return n
}

func rawUnits(value string) *big.Int {
	if !quantityPattern.MatchString(value) {
		return nil
	}
	n, _ := new(big.Int).SetString(value, 10)
	return n
}

func markedValue(units *big.Int, price *big.Rat, decimals int) *big.Rat {
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
	return new(big.Rat).Mul(new(big.Rat).SetFrac(units, scale), price)
}

func exceeds(value, base *big.Rat, bps int) bool {
	return value.Cmp(new(big.Rat).Mul(base, big.NewRat(int64(bps), 10000))) > 0
}

func maxAmount(a, b *big.Rat) *big.Rat {
	if a.Cmp(b) >= 0 {
		return a
	}
	return b
}

func minAmount(a, b *big.Rat) *big.Rat {
	if a.Cmp(b) <= 0 {
		return a
	}
	return b
}
