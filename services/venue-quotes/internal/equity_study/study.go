// Package equity_study converts archived books into comparable equity exposures.
package equity_study

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"regexp"
	"time"

	"tradingbot/venue-quotes/internal/hyperliquid"
	"tradingbot/venue-quotes/internal/quotes"
)

const bps = 10_000

var identifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,95}$`)
var digestText = regexp.MustCompile(`^[a-f0-9]{64}$`)
var decimalText = regexp.MustCompile(`^-?(0|[1-9][0-9]{0,11})(\.[0-9]{1,20})?$`)

type Request struct {
	Version    string          `json:"schema_version"`
	Source     string          `json:"source_mode"`
	AsOf       time.Time       `json:"as_of"`
	Horizon    int             `json:"horizon_hours"`
	MaxSkew    int             `json:"max_skew_seconds"`
	Account    json.RawMessage `json:"account"`
	Policy     json.RawMessage `json:"policy"`
	Securities []securityInput `json:"securities"`
}

type reference struct {
	PriceUSD    string    `json:"price_usd"`
	ObservedAt  time.Time `json:"observed_at"`
	AvailableAt time.Time `json:"available_at"`
	Evidence    string    `json:"evidence_sha256"`
}

type securityInput struct {
	SecurityID           string          `json:"security_id"`
	Symbol               string          `json:"symbol"`
	HeldSpot             json.RawMessage `json:"held_spot"`
	ClearCorporateWindow *bool           `json:"corporate_action_window_clear"`
	StressDown           *int            `json:"market_stress_down_bps"`
	StressUp             *int            `json:"market_stress_up_bps"`
	Reference            reference       `json:"reference"`
	Budgets              []string        `json:"notionals_usd"`
	Spot                 *spotInput      `json:"spot"`
	Perp                 *perpInput      `json:"perp"`
}

type marketInput struct {
	MarketID    string `json:"market_id"`
	Shares      string `json:"shares_per_unit"`
	USDPerQuote string `json:"usd_per_quote_unit"`
	TakerFee    *int   `json:"taker_fee_bps"`
	FixedFee    string `json:"fixed_fee_usd_per_leg"`
	StressAdd   *int   `json:"execution_stress_add_bps"`
	BasisCost   *int   `json:"basis_cost_bps"`
	BasisStress *int   `json:"basis_stress_bps"`
}

type spotInput struct {
	marketInput
	Provider     string          `json:"provider"`
	MarketSymbol string          `json:"market_symbol"`
	SizeDecimals *int            `json:"size_decimals"`
	Book         json.RawMessage `json:"order_book"`
}

type perpInput struct {
	marketInput
	Capture           json.RawMessage `json:"capture"`
	SessionUntil      *time.Time      `json:"external_session_until"`
	FundingStressRate string          `json:"funding_stress_rate_hourly"`
	CollateralIndex   *int            `json:"collateral_token_index"`
}

type Study struct {
	Version     string          `json:"schema_version"`
	Source      string          `json:"source_mode"`
	InputSHA256 string          `json:"normalization_input_sha256"`
	AsOf        time.Time       `json:"as_of"`
	Horizon     int             `json:"horizon_hours"`
	Account     json.RawMessage `json:"account"`
	Policy      json.RawMessage `json:"policy"`
	Securities  []security      `json:"securities"`
}

type security struct {
	ID                   string          `json:"security_id"`
	Symbol               string          `json:"symbol"`
	HeldSpot             json.RawMessage `json:"held_spot"`
	ClearCorporateWindow bool            `json:"corporate_action_window_clear"`
	StressDown           int             `json:"market_stress_down_bps"`
	StressUp             int             `json:"market_stress_up_bps"`
	Sizes                []size          `json:"sizes"`
}

type size struct {
	Notional string     `json:"notional_usd"`
	Spot     *spotQuote `json:"spot"`
	Perp     *perpQuote `json:"perp"`
}

type observation struct {
	SHA256      string    `json:"observation_sha256"`
	MarketID    string    `json:"market_id"`
	ObservedAt  time.Time `json:"observed_at"`
	AvailableAt time.Time `json:"available_at"`
}

type market struct {
	input        marketInput
	observation  observation
	book         quotes.OrderBook
	sizeDecimals int
	shares       *big.Rat
	fx           *big.Rat
	fixedFee     *big.Rat
}

type perpetual struct {
	market
	capture       hyperliquid.Capture
	tiers         []hyperliquid.MarginTier
	sessionUntil  *time.Time
	fundingStress *big.Rat
}

func Build(raw []byte) (Study, error) {
	var request Request
	if err := decode(raw, &request); err != nil {
		return Study{}, fmt.Errorf("equity study input: %w", err)
	}
	var limits struct {
		MaxAge    int `json:"max_observation_age_seconds"`
		MinStress int `json:"min_stress_move_bps"`
	}
	if request.Version != "0.1" || (request.Source != "synthetic_fixture" && request.Source != "observed_market") ||
		request.AsOf.IsZero() || request.Horizon < 1 || request.Horizon > 720 ||
		request.MaxSkew < 1 || request.MaxSkew > 300 || len(request.Securities) < 1 || len(request.Securities) > 8 ||
		!object(request.Account) || !object(request.Policy) || json.Unmarshal(request.Policy, &limits) != nil ||
		limits.MaxAge < 1 || limits.MaxAge > 86400 || limits.MinStress < 1 || limits.MinStress > bps {
		return Study{}, fmt.Errorf("invalid study scope, account or observation limits")
	}
	result := Study{
		Version: "0.1", Source: request.Source, InputSHA256: sha(raw), AsOf: request.AsOf.UTC(),
		Horizon: request.Horizon, Account: request.Account, Policy: request.Policy,
	}
	ids, symbols, markets := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, item := range request.Securities {
		if ids[item.SecurityID] || symbols[item.Symbol] {
			return Study{}, fmt.Errorf("duplicate security identity")
		}
		ids[item.SecurityID], symbols[item.Symbol] = true, true
		for _, input := range []*marketInput{spotMarket(item.Spot), perpMarket(item.Perp)} {
			if input != nil {
				if markets[input.MarketID] {
					return Study{}, fmt.Errorf("duplicate market identity %q", input.MarketID)
				}
				markets[input.MarketID] = true
			}
		}
		sec, err := request.normalize(item, limits.MaxAge, limits.MinStress)
		if err != nil {
			return Study{}, fmt.Errorf("%s: %w", item.Symbol, err)
		}
		result.Securities = append(result.Securities, sec)
	}
	return result, nil
}

func spotMarket(input *spotInput) *marketInput {
	if input == nil {
		return nil
	}
	return &input.marketInput
}

func perpMarket(input *perpInput) *marketInput {
	if input == nil {
		return nil
	}
	return &input.marketInput
}

func (r Request) normalize(in securityInput, maxAge, minimumStress int) (security, error) {
	refPrice := positive(in.Reference.PriceUSD)
	if !identifier.MatchString(in.SecurityID) || (in.HeldSpot == nil || !object(in.HeldSpot) && string(in.HeldSpot) != "null") ||
		in.ClearCorporateWindow == nil || !inRange(in.StressDown, bps) || !inRange(in.StressUp, 30000) ||
		refPrice == nil || !digestText.MatchString(in.Reference.Evidence) || len(in.Budgets) < 1 || len(in.Budgets) > 8 ||
		(in.Spot == nil && in.Perp == nil) {
		return security{}, fmt.Errorf("missing identity, risk context, reference or markets")
	}
	if err := (quotes.BookRequest{Symbol: in.Symbol, Depth: 20}).Validate(); err != nil {
		return security{}, fmt.Errorf("invalid underlying symbol")
	}
	ref := in.Reference.ObservedAt
	if !r.fresh(ref, in.Reference.AvailableAt, maxAge) {
		return security{}, fmt.Errorf("stale or future equity reference")
	}
	earliest, latest := ref, ref
	var spot *market
	var perp *perpetual
	var grid *big.Rat
	if in.Spot != nil {
		var err error
		spot, err = readSpot(*in.Spot)
		if err != nil {
			return security{}, err
		}
		if !r.fresh(spot.observation.ObservedAt, spot.observation.AvailableAt, maxAge) {
			return security{}, fmt.Errorf("stale or future spot book")
		}
		grid = spot.shareLot()
		earliest = earlier(earliest, spot.observation.ObservedAt)
		latest = later(latest, spot.observation.AvailableAt)
	}
	if in.Perp != nil {
		var err error
		perp, err = readPerp(*in.Perp, in.Symbol)
		if err != nil {
			return security{}, err
		}
		if !r.fresh(perp.observation.ObservedAt, perp.observation.AvailableAt, maxAge) {
			return security{}, fmt.Errorf("stale or future perpetual capture")
		}
		grid = commonLot(grid, perp.shareLot())
		earliest = earlier(earliest, perp.observation.ObservedAt)
		latest = later(latest, perp.observation.AvailableAt)
	}
	if latest.Sub(earliest) > time.Duration(r.MaxSkew)*time.Second {
		return security{}, fmt.Errorf("equity reference and market observations exceed maximum skew")
	}
	out := security{
		ID: in.SecurityID, Symbol: in.Symbol, HeldSpot: in.HeldSpot,
		ClearCorporateWindow: *in.ClearCorporateWindow, StressDown: *in.StressDown, StressUp: *in.StressUp,
	}
	seen := map[string]bool{}
	lotUSD := new(big.Rat).Mul(grid, refPrice)
	for _, budget := range in.Budgets {
		limit := positive(budget)
		if limit == nil || !exactAtScale(limit, 8) {
			return security{}, fmt.Errorf("invalid USD notional target")
		}
		lots := new(big.Rat).Quo(limit, lotUSD)
		count := new(big.Int).Quo(lots.Num(), lots.Denom())
		if count.Sign() == 0 {
			continue
		}
		shares := new(big.Rat).Mul(grid, new(big.Rat).SetInt(count))
		notional := new(big.Rat).Mul(shares, refPrice)
		wireNotional := upward(notional, 100_000_000).FloatString(8)
		if seen[wireNotional] {
			continue
		}
		seen[wireNotional] = true
		row := size{Notional: wireNotional}
		var err error
		if spot != nil {
			row.Spot, err = spot.spot(shares, notional)
			if err != nil {
				return security{}, err
			}
		}
		if perp != nil {
			row.Perp, err = perp.perp(shares, notional, r.Horizon, max(minimumStress, *in.StressUp, *in.StressDown))
			if err != nil {
				return security{}, err
			}
		}
		if row.Spot != nil || row.Perp != nil {
			out.Sizes = append(out.Sizes, row)
		}
	}
	if len(out.Sizes) == 0 {
		return security{}, fmt.Errorf("no requested size fits the lot grid and two-sided depth")
	}
	return out, nil
}

func (r Request) fresh(observed, available time.Time, maxAge int) bool {
	return !observed.IsZero() && !available.Before(observed) && !available.After(r.AsOf) &&
		r.AsOf.Sub(observed) <= time.Duration(maxAge)*time.Second
}

func decode(raw []byte, target any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return fmt.Errorf("expected a single JSON document")
	}
	return nil
}

func object(raw []byte) bool {
	return len(raw) > 0 && raw[0] == '{'
}

func inRange(value *int, limit int) bool {
	return value != nil && *value >= 0 && *value <= limit
}

func amount(raw string) *big.Rat {
	if !decimalText.MatchString(raw) {
		return nil
	}
	value, _ := new(big.Rat).SetString(raw)
	return value
}

func positive(raw string) *big.Rat {
	value := amount(raw)
	if value == nil || value.Sign() <= 0 {
		return nil
	}
	return value
}

func sha(raw []byte) string {
	return fmt.Sprintf("%x", sha256.Sum256(raw))
}

func earlier(left, right time.Time) time.Time {
	if left.Before(right) {
		return left
	}
	return right
}

func later(left, right time.Time) time.Time {
	if left.After(right) {
		return left
	}
	return right
}
