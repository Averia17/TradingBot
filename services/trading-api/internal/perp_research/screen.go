// Package perp_research screens first-entry scenarios for a flat virtual account.
package perp_research

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"time"

	"tradingbot/trading-api/internal/contract"
)

const oneBPS = 10_000

type amount struct{ *big.Rat }

func (a *amount) UnmarshalJSON(raw []byte) error {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	rat, err := contract.Amount(value)
	a.Rat = rat
	return err
}

type policy struct {
	AllowedLeverage      []int  `json:"allowed_leverage"`
	MaxNotional          amount `json:"max_notional_usd"`
	MaxStressLoss        amount `json:"max_stress_loss_usd"`
	MaxQuoteAgeSeconds   int    `json:"max_quote_age_seconds"`
	MaxMarkBasisBPS      int    `json:"max_mark_basis_bps"`
	MinMarkStressBPS     int    `json:"min_mark_stress_bps"`
	MaintenanceBufferBPS int    `json:"maintenance_buffer_bps"`
	MinNetEdgeBPS        int    `json:"min_net_edge_bps"`
}

type costs struct {
	RoundTripExecutionBPS int `json:"round_trip_execution_bps"`
	ProjectedFundingBPS   int `json:"projected_funding_cost_bps"`
	FundingStressBPS      int `json:"funding_stress_cost_bps"`
}

type quote struct {
	Notional       amount `json:"notional_usd"`
	MaxLeverage    int    `json:"venue_max_leverage"`
	MaintenanceBPS int    `json:"maintenance_margin_bps"`
	Long           *costs `json:"long"`
	Short          *costs `json:"short"`
}

type market struct {
	MarketID      string    `json:"market_id"`
	SecurityID    string    `json:"security_id"`
	Symbol        string    `json:"symbol"`
	Product       string    `json:"product"`
	ObservationID string    `json:"observation_id"`
	ObservedAt    time.Time `json:"observed_at"`
	AvailableAt   time.Time `json:"available_at"`
	Mark          amount    `json:"mark_usd"`
	Index         amount    `json:"index_usd"`
	MarkStressBPS int       `json:"mark_stress_bps"`
	Quotes        []quote   `json:"quotes"`
}

type study struct {
	Version    string    `json:"schema_version"`
	Source     string    `json:"source_mode"`
	AsOf       time.Time `json:"as_of"`
	Horizon    int       `json:"horizon_hours"`
	Collateral amount    `json:"available_collateral_usd"`
	Policy     policy    `json:"policy"`
	Markets    []market  `json:"markets"`
}

type Option struct {
	ID                  string `json:"option_id"`
	Side                string `json:"side"`
	Leverage            int    `json:"leverage"`
	NotionalUSD         string `json:"notional_usd"`
	InitialMarginUSD    string `json:"initial_margin_usd"`
	StressLossUSD       string `json:"scenario_loss_usd"`
	StressHeadroomUSD   string `json:"scenario_headroom_usd"`
	ProjectedFundingBPS int    `json:"projected_funding_cost_bps"`
	EstimatedCostBPS    int    `json:"estimated_total_cost_bps"`
}

type MarketScreen struct {
	MarketID      string    `json:"market_id"`
	SecurityID    string    `json:"security_id"`
	Symbol        string    `json:"symbol"`
	ObservationID string    `json:"observation_id"`
	ObservedAt    time.Time `json:"observed_at"`
	AvailableAt   time.Time `json:"available_at"`
	MarkUSD       string    `json:"mark_usd"`
	IndexUSD      string    `json:"index_usd"`
	MarkStressBPS int       `json:"mark_stress_bps"`
	Status        string    `json:"status"`
	Options       []Option  `json:"options"`
}

type Screen struct {
	Version     string         `json:"schema_version"`
	Source      string         `json:"source_mode"`
	StudySHA256 string         `json:"study_sha256"`
	AsOf        time.Time      `json:"as_of"`
	Horizon     int            `json:"horizon_hours"`
	Collateral  string         `json:"available_collateral_usd"`
	MinNetEdge  int            `json:"min_net_edge_bps"`
	Markets     []MarketScreen `json:"markets"`
}

func Evaluate(raw []byte) (Screen, error) {
	var input study
	if err := contract.Decode(raw, contract.PerpStudy, &input); err != nil {
		return Screen{}, fmt.Errorf("invalid perpetual study: %w", err)
	}
	if input.AsOf.IsZero() || input.Policy.MaxNotional.Sign() <= 0 || input.Policy.MaxStressLoss.Sign() <= 0 {
		return Screen{}, fmt.Errorf("study cutoff and positive risk caps are required")
	}
	slices.Sort(input.Policy.AllowedLeverage)

	result := Screen{
		Version: "0.1", Source: input.Source, StudySHA256: fmt.Sprintf("%x", sha256.Sum256(raw)),
		AsOf: input.AsOf, Horizon: input.Horizon, Collateral: decimal(input.Collateral.Rat),
		MinNetEdge: input.Policy.MinNetEdgeBPS, Markets: make([]MarketScreen, 0, len(input.Markets)),
	}
	marketIDs := map[string]bool{}
	symbolBySecurity := map[string]string{}
	securityBySymbol := map[string]string{}
	for _, market := range input.Markets {
		if marketIDs[market.MarketID] {
			return Screen{}, fmt.Errorf("duplicate market identity %q", market.MarketID)
		}
		if id, ok := securityBySymbol[market.Symbol]; ok && id != market.SecurityID {
			return Screen{}, fmt.Errorf("ambiguous security for symbol %q", market.Symbol)
		}
		if symbol, ok := symbolBySecurity[market.SecurityID]; ok && symbol != market.Symbol {
			return Screen{}, fmt.Errorf("ambiguous symbol for security %q", market.SecurityID)
		}
		marketIDs[market.MarketID] = true
		symbolBySecurity[market.SecurityID] = market.Symbol
		securityBySymbol[market.Symbol] = market.SecurityID
		row, err := screenMarket(input, market)
		if err != nil {
			return Screen{}, fmt.Errorf("market %q: %w", market.MarketID, err)
		}
		result.Markets = append(result.Markets, row)
	}
	return result, nil
}

func screenMarket(input study, market market) (MarketScreen, error) {
	if market.ObservedAt.IsZero() || market.AvailableAt.Before(market.ObservedAt) {
		return MarketScreen{}, fmt.Errorf("invalid observation chronology")
	}
	if market.Mark.Sign() <= 0 || market.Index.Sign() <= 0 {
		return MarketScreen{}, fmt.Errorf("mark and index must be positive")
	}
	status := "no_eligible_options"
	basis := new(big.Rat).Abs(new(big.Rat).Sub(market.Mark.Rat, market.Index.Rat))
	switch {
	case market.AvailableAt.After(input.AsOf):
		status = "not_yet_available"
	case input.AsOf.Sub(market.ObservedAt) > time.Duration(input.Policy.MaxQuoteAgeSeconds)*time.Second:
		status = "stale_observation"
	case basis.Cmp(atBPS(market.Index.Rat, input.Policy.MaxMarkBasisBPS)) > 0:
		status = "mark_basis_above_limit"
	}
	row := MarketScreen{
		MarketID: market.MarketID, SecurityID: market.SecurityID, Symbol: market.Symbol,
		ObservationID: market.ObservationID, ObservedAt: market.ObservedAt, AvailableAt: market.AvailableAt,
		MarkUSD: decimal(market.Mark.Rat), IndexUSD: decimal(market.Index.Rat),
		MarkStressBPS: max(market.MarkStressBPS, input.Policy.MinMarkStressBPS),
		Status:        status, Options: []Option{},
	}
	notionals := map[string]bool{}
	for _, quote := range market.Quotes {
		notional := decimal(quote.Notional.Rat)
		if quote.Notional.Sign() <= 0 || notionals[notional] || (quote.Long == nil && quote.Short == nil) {
			return MarketScreen{}, fmt.Errorf("quotes require unique positive sizes and a supported side")
		}
		notionals[notional] = true
		for _, side := range []struct {
			Name  string
			Costs *costs
		}{{"LONG", quote.Long}, {"SHORT", quote.Short}} {
			if side.Costs == nil {
				continue
			}
			if side.Costs.FundingStressBPS < side.Costs.ProjectedFundingBPS {
				return MarketScreen{}, fmt.Errorf("funding stress understates projected funding expense")
			}
			if status == "no_eligible_options" && quote.Notional.Cmp(input.Policy.MaxNotional.Rat) <= 0 {
				row.Options = append(row.Options, screenSide(input, row, quote, side.Name, *side.Costs)...)
			}
		}
	}
	if len(row.Options) > 0 {
		row.Status = "eligible"
	}
	return row, nil
}

func screenSide(input study, market MarketScreen, quote quote, side string, costs costs) []Option {
	notional := quote.Notional.Rat
	expense := atBPS(notional, costs.RoundTripExecutionBPS+costs.FundingStressBPS)
	loss := new(big.Rat).Add(atBPS(notional, market.MarkStressBPS), expense)
	if loss.Cmp(input.Policy.MaxStressLoss.Rat) > 0 {
		return nil
	}
	// Use the larger adverse marked notional for maintenance on either side.
	// This is a conservative scenario, independent of any venue's liquidation formula.
	stressedValue := atBPS(notional, oneBPS+market.MarkStressBPS)
	maintenance := atBPS(stressedValue, quote.MaintenanceBPS+input.Policy.MaintenanceBufferBPS)
	var options []Option
	for _, leverage := range input.Policy.AllowedLeverage {
		if leverage > quote.MaxLeverage {
			break
		}
		margin := new(big.Rat).Quo(notional, big.NewRat(int64(leverage), 1))
		headroom := floorUSD(new(big.Rat).Sub(margin, new(big.Rat).Add(loss, maintenance)))
		// Keep expense outside the initial margin when comparing with free collateral.
		initialMargin := ceilUSD(margin)
		required := new(big.Rat).Add(initialMargin, ceilUSD(expense))
		if headroom.Sign() <= 0 || required.Cmp(input.Collateral.Rat) > 0 {
			continue
		}
		size := decimal(notional)
		options = append(options, Option{
			ID:   fmt.Sprintf("%s:%s:%s:%d", market.MarketID, side, size, leverage),
			Side: side, Leverage: leverage, NotionalUSD: size,
			InitialMarginUSD: decimal(initialMargin), StressLossUSD: decimal(ceilUSD(loss)),
			StressHeadroomUSD:   decimal(headroom),
			ProjectedFundingBPS: costs.ProjectedFundingBPS,
			EstimatedCostBPS:    costs.RoundTripExecutionBPS + costs.ProjectedFundingBPS,
		})
	}
	return options
}

func atBPS(value *big.Rat, bps int) *big.Rat {
	return new(big.Rat).Mul(value, big.NewRat(int64(bps), oneBPS))
}

func decimal(value *big.Rat) string {
	return strings.TrimRight(strings.TrimRight(value.FloatString(8), "0"), ".")
}

// Round USD requirements up before comparing them with available collateral.
func ceilUSD(value *big.Rat) *big.Rat {
	result := floorUSD(value)
	if result.Cmp(value) < 0 {
		result.Add(result, big.NewRat(1, 100_000_000))
	}
	return result
}

func floorUSD(value *big.Rat) *big.Rat {
	// Div by the positive denominator also rounds negative headroom down.
	scale := big.NewInt(100_000_000)
	units := new(big.Int).Mul(value.Num(), scale)
	units.Div(units, value.Denom())
	return new(big.Rat).SetFrac(units, scale)
}
