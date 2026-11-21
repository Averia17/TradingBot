// Package equity_route compares hypothetical equity routes on immutable snapshots.
package equity_route

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"time"

	"tradingbot/control/internal/contract"
)

const bpsScale = 10_000

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

type account struct {
	SnapshotID            string `json:"snapshot_id"`
	SpotCash              amount `json:"spot_cash_usd"`
	PerpCollateral        amount `json:"perp_collateral_usd"`
	RemainingStressBudget amount `json:"remaining_stress_budget_usd"`
	HasOpenPerps          bool   `json:"has_open_perps"`
	HasPendingOrders      bool   `json:"has_pending_orders"`
}

type policy struct {
	Version           string `json:"version"`
	AllowLeverage     bool   `json:"allow_leverage"`
	MaxLeverage       int    `json:"max_leverage,omitempty"`
	MaxNotional       amount `json:"max_notional_usd"`
	MaxPerpCapital    amount `json:"max_perp_capital_at_risk_usd"`
	MinStressMove     int    `json:"min_stress_move_bps"`
	MaxObservationAge int    `json:"max_observation_age_seconds"`
	MaxBasis          int    `json:"max_basis_bps"`
	MaintenanceBuffer int    `json:"maintenance_buffer_bps"`
	MinUncertainty    int    `json:"min_forecast_uncertainty_bps"`
	MinNetEdge        int    `json:"min_net_edge_bps"`
	MinPerpAdvantage  int    `json:"min_perp_advantage_bps"`
	MinLeverageGain   amount `json:"min_leverage_advantage_usd"`
	StressLossPenalty int    `json:"stress_loss_penalty_bps"`
}

func (p policy) leverageLimit() int {
	if !p.AllowLeverage {
		return 1
	}
	if p.MaxLeverage == 0 {
		return 2
	}
	return p.MaxLeverage
}

type SecurityKey struct {
	ID     string `json:"security_id"`
	Symbol string `json:"symbol"`
}

type heldSpot struct {
	MarketID string `json:"market_id"`
	Notional amount `json:"notional_usd"`
}

type security struct {
	SecurityKey
	HeldSpot             *heldSpot `json:"held_spot"`
	ClearCorporateWindow bool      `json:"corporate_action_window_clear"`
	StressDown           int       `json:"market_stress_down_bps"`
	StressUp             int       `json:"market_stress_up_bps"`
	Sizes                []size    `json:"sizes"`
}

type size struct {
	Notional amount     `json:"notional_usd"`
	Spot     *spotQuote `json:"spot"`
	Perp     *perpQuote `json:"perp"`
}

type observation struct {
	SHA256      string    `json:"observation_sha256"`
	MarketID    string    `json:"market_id"`
	ObservedAt  time.Time `json:"observed_at"`
	AvailableAt time.Time `json:"available_at"`
}

type costs struct {
	Base   int `json:"base_bps"`
	Stress int `json:"stress_bps"`
}

type spotQuote struct {
	Observation observation `json:"observation"`
	Costs       costs       `json:"costs"`
	Exit        int         `json:"exit_bps"`
}

type funding struct {
	Forecast int `json:"forecast_bps"`
	Stress   int `json:"stress_bps"`
}

type perpCosts struct {
	Execution costs   `json:"execution"`
	Basis     costs   `json:"basis"`
	Funding   funding `json:"funding"`
}

type perpQuote struct {
	Observation          observation `json:"observation"`
	ExternalSessionUntil *time.Time  `json:"external_session_until"`
	MarkBasis            int         `json:"mark_basis_bps"`
	MarginMode           string      `json:"margin_mode"`
	MaxLeverage          int         `json:"venue_max_leverage"`
	Maintenance          int         `json:"maintenance_margin_bps"`
	Long                 *perpCosts  `json:"long"`
	Short                *perpCosts  `json:"short"`
}

type Study struct {
	Version    string     `json:"schema_version"`
	Source     string     `json:"source_mode"`
	AsOf       time.Time  `json:"as_of"`
	Horizon    int        `json:"horizon_hours"`
	Account    account    `json:"account"`
	Policy     policy     `json:"policy"`
	Securities []security `json:"securities"`
	digest     string
}

type StudyInfo struct {
	Source                 string        `json:"source_mode"`
	StudyHash              string        `json:"study_sha256"`
	AsOf                   time.Time     `json:"as_of"`
	Horizon                int           `json:"horizon_hours"`
	Securities             []SecurityKey `json:"securities"`
	RequiresReconciliation bool          `json:"requires_reconciliation"`
}

func (s Study) Info() StudyInfo {
	info := StudyInfo{
		Source: s.Source, StudyHash: s.digest, AsOf: s.AsOf, Horizon: s.Horizon,
		RequiresReconciliation: s.Account.HasOpenPerps || s.Account.HasPendingOrders,
	}
	for _, sec := range s.Securities {
		info.Securities = append(info.Securities, sec.SecurityKey)
	}
	return info
}

func LoadStudy(raw []byte) (Study, error) {
	var s Study
	if err := contract.Decode(raw, contract.EquityRouteStudy, &s); err != nil {
		return Study{}, fmt.Errorf("invalid equity route study: %w", err)
	}
	if s.AsOf.IsZero() || s.Policy.MaxNotional.Sign() <= 0 || s.Policy.MinLeverageGain.Sign() <= 0 {
		return Study{}, fmt.Errorf("study requires a cutoff and positive notional and leverage advantage limits")
	}
	ids, symbols := map[string]bool{}, map[string]bool{}
	marketOwners := map[string]string{}
	for _, sec := range s.Securities {
		if ids[sec.ID] || symbols[sec.Symbol] {
			return Study{}, fmt.Errorf("duplicate security or symbol")
		}
		ids[sec.ID], symbols[sec.Symbol] = true, true
		if err := validateSecurity(sec, marketOwners); err != nil {
			return Study{}, fmt.Errorf("security %s: %w", sec.ID, err)
		}
	}
	s.digest = digest(raw)
	return s, nil
}

func validateSecurity(sec security, marketOwners map[string]string) error {
	sizes := map[string]bool{}
	if sec.HeldSpot != nil {
		if sec.HeldSpot.Notional.Sign() <= 0 {
			return fmt.Errorf("held position must have positive exposure")
		}
		if err := bindMarket(marketOwners, sec.HeldSpot.MarketID, sec.ID+":spot"); err != nil {
			return err
		}
	}
	for _, size := range sec.Sizes {
		key := size.Notional.RatString()
		if size.Notional.Sign() <= 0 || sizes[key] || (size.Spot == nil && size.Perp == nil) {
			return fmt.Errorf("size requires unique positive exposure and a route")
		}
		sizes[key] = true
		if quote := size.Spot; quote != nil {
			if !quote.Observation.valid() || quote.Costs.Stress < quote.Costs.Base || quote.Exit > quote.Costs.Base {
				return fmt.Errorf("invalid spot observation or incomplete cost bands")
			}
			if err := bindMarket(marketOwners, quote.Observation.MarketID, sec.ID+":spot"); err != nil {
				return err
			}
		}
		if quote := size.Perp; quote != nil {
			if !quote.Observation.valid() || (quote.Long == nil && quote.Short == nil) {
				return fmt.Errorf("invalid perpetual observation or missing sides")
			}
			if err := bindMarket(marketOwners, quote.Observation.MarketID, sec.ID+":perp"); err != nil {
				return err
			}
			for _, costs := range []*perpCosts{quote.Long, quote.Short} {
				if costs != nil && (costs.Execution.Stress < costs.Execution.Base ||
					costs.Basis.Stress < costs.Basis.Base || costs.Funding.Stress < abs(costs.Funding.Forecast)) {
					return fmt.Errorf("perpetual stress bands must cover base costs and a funding reversal")
				}
			}
		}
	}
	return nil
}

func bindMarket(owners map[string]string, market, scope string) error {
	if owner, known := owners[market]; known && owner != scope {
		return fmt.Errorf("ambiguous market identity %q", market)
	}
	owners[market] = scope
	return nil
}

func (o observation) valid() bool {
	return !o.ObservedAt.IsZero() && !o.AvailableAt.Before(o.ObservedAt)
}

type Thesis struct {
	SecurityKey
	ExpectedReturn int      `json:"expected_price_return_bps"`
	Uncertainty    int      `json:"uncertainty_bps"`
	ReviewHours    int      `json:"review_in_hours"`
	Summary        string   `json:"thesis"`
	Invalidation   []string `json:"invalidation_conditions"`
	Evidence       []string `json:"evidence_sha256"`
}

type thesisBatch struct {
	Version   string   `json:"schema_version"`
	Source    string   `json:"source_mode"`
	StudyHash string   `json:"study_sha256"`
	Theses    []Thesis `json:"theses"`
}

func (s Study) loadTheses(raw []byte) (map[string]Thesis, error) {
	var result thesisBatch
	if err := contract.Decode(raw, contract.EquityRouteTheses, &result); err != nil {
		return nil, fmt.Errorf("invalid equity theses: %w", err)
	}
	if result.StudyHash != s.digest || result.Source != s.Source || len(result.Theses) != len(s.Securities) {
		return nil, fmt.Errorf("equity theses do not match the study")
	}
	theses := make(map[string]Thesis, len(result.Theses))
	for _, thesis := range result.Theses {
		if _, duplicate := theses[thesis.ID]; duplicate ||
			thesis.ReviewHours > s.Horizon || strings.TrimSpace(thesis.Summary) == "" {
			return nil, fmt.Errorf("invalid or duplicate thesis for %q", thesis.ID)
		}
		for _, condition := range thesis.Invalidation {
			if strings.TrimSpace(condition) == "" {
				return nil, fmt.Errorf("blank invalidation condition")
			}
		}
		theses[thesis.ID] = thesis
	}
	for _, sec := range s.Securities {
		if thesis, ok := theses[sec.ID]; !ok || thesis.Symbol != sec.Symbol {
			return nil, fmt.Errorf("missing or mismatched thesis for %q", sec.ID)
		}
	}
	return theses, nil
}

func abs(value int) int {
	return max(value, -value)
}

func digest(raw []byte) string {
	return fmt.Sprintf("%x", sha256.Sum256(raw))
}
