package equity_route

import (
	"math/big"
	"slices"
	"strings"
	"time"
)

type Candidate struct {
	SecurityKey
	Action               string  `json:"action"`
	MarketID             string  `json:"market_id,omitempty"`
	ObservationSHA256    string  `json:"observation_sha256,omitempty"`
	Leverage             int     `json:"leverage"`
	NotionalUSD          string  `json:"notional_usd"`
	Status               string  `json:"status"`
	NetReturnBPS         *int    `json:"conservative_net_return_bps,omitempty"`
	NetUSD               *string `json:"conservative_net_usd,omitempty"`
	StressLossUSD        *string `json:"stress_loss_usd,omitempty"`
	RiskAdjustedUSD      *string `json:"risk_adjusted_value_usd,omitempty"`
	RequiredCapitalUSD   *string `json:"required_capital_usd,omitempty"`
	MarginHeadroomUSD    *string `json:"stress_margin_headroom_usd,omitempty"`
	CappedFundingCostBPS *int    `json:"funding_cost_used_bps,omitempty"`
	ExecutionCostBPS     *int    `json:"execution_cost_bps,omitempty"`
	BasisCostBPS         *int    `json:"basis_cost_bps,omitempty"`
	UncertaintyUsedBPS   *int    `json:"uncertainty_used_bps,omitempty"`
	ExtraNetUSD          *string `json:"incremental_net_usd_vs_unlevered,omitempty"`
	ExtraValueUSD        *string `json:"incremental_value_usd_vs_unlevered,omitempty"`

	notional, net, value *big.Rat
}

type Decision struct {
	Version           string      `json:"schema_version"`
	Mode              string      `json:"execution_mode"`
	Source            string      `json:"source_mode"`
	StudyHash         string      `json:"study_sha256"`
	ThesesHash        string      `json:"theses_sha256"`
	AccountSnapshotID string      `json:"account_snapshot_id"`
	PolicyVersion     string      `json:"policy_version"`
	AsOf              time.Time   `json:"as_of"`
	Horizon           int         `json:"horizon_hours"`
	AlphaMeasured     bool        `json:"alpha_measured"`
	Action            string      `json:"action"`
	Reason            string      `json:"reason"`
	Selected          *Candidate  `json:"selected"`
	SelectedThesis    *Thesis     `json:"selected_thesis"`
	UnleveredBaseline *Candidate  `json:"best_unlevered_candidate"`
	Candidates        []Candidate `json:"candidates"`
}

func (s Study) Decide(rawTheses []byte) (Decision, error) {
	theses, err := s.loadTheses(rawTheses)
	if err != nil {
		return Decision{}, err
	}
	result := Decision{
		Version: "0.1", Mode: "research", Source: s.Source,
		StudyHash: s.digest, ThesesHash: digest(rawTheses), AccountSnapshotID: s.Account.SnapshotID,
		PolicyVersion: s.Policy.Version, AsOf: s.AsOf, Horizon: s.Horizon,
		Action: "NO_TRADE", Reason: "no_eligible_candidate", Candidates: []Candidate{},
	}
	if s.Account.HasPendingOrders || s.Account.HasOpenPerps {
		result.Reason = "account_requires_lifecycle_reconciliation"
		return result, nil
	}
	for _, sec := range s.Securities {
		thesis := theses[sec.ID]
		for _, size := range sec.Sizes {
			result.Candidates = append(result.Candidates, s.evaluateSize(sec, size, thesis)...)
		}
	}
	slices.SortFunc(result.Candidates, compareCandidates)
	bestUnlevered := bestCandidate(result.Candidates, 1)
	result.UnleveredBaseline = bestUnlevered
	s.compareLeverage(result.Candidates, bestUnlevered)
	best := bestCandidate(result.Candidates, s.Policy.leverageLimit())
	if best != nil {
		thesis := theses[best.ID]
		result.Action, result.Reason = best.Action, "selected_after_risk_and_costs"
		result.Selected, result.SelectedThesis = best, &thesis
	}
	return result, nil
}

func (s Study) evaluateSize(sec security, size size, thesis Thesis) []Candidate {
	if !sec.ClearCorporateWindow {
		return []Candidate{newCandidate(sec, size, "NO_TRADE", 1).rejected("corporate_action_window_unverified")}
	}
	if sec.HeldSpot != nil {
		return []Candidate{s.reduceSpot(sec, size, thesis)}
	}
	if thesis.ExpectedReturn == 0 {
		return nil
	}
	side := "PERP_LONG"
	stress := max(s.Policy.MinStressMove, sec.StressDown)
	if thesis.ExpectedReturn < 0 {
		side = "PERP_SHORT"
		stress = max(s.Policy.MinStressMove, sec.StressUp)
	}
	var candidates []Candidate
	var spot Candidate
	if side == "PERP_LONG" {
		spot = s.buySpot(sec, size, thesis, stress)
		candidates = append(candidates, spot)
	}
	one := s.openPerp(sec, size, thesis, side, stress, 1)
	candidates = append(candidates, s.comparePerpWithSpot(one, spot))
	if size.Perp == nil {
		return candidates
	}
	// At the same exposure, more leverage only frees collateral and
	// reduces margin headroom. Advance only past lower levels that lack collateral.
	lowerLevelsNeedCollateral := one.Status == "insufficient_perp_collateral"
	limit := min(s.Policy.leverageLimit(), size.Perp.MaxLeverage)
	for leverage := 2; leverage <= limit; leverage++ {
		raw := s.openPerp(sec, size, thesis, side, stress, leverage)
		candidate := s.comparePerpWithSpot(raw, spot)
		if candidate.Status == "eligible" {
			switch {
			case !lowerLevelsNeedCollateral:
				candidate.Status = "leverage_not_justified_at_same_exposure"
			case side == "PERP_LONG" && spot.Status != "eligible" &&
				spot.Status != "insufficient_spot_cash" && spot.Status != "insufficient_net_edge":
				candidate.Status = "paired_spot_observation_unavailable"
			}
		}
		candidates = append(candidates, candidate)
		lowerLevelsNeedCollateral = lowerLevelsNeedCollateral && raw.Status == "insufficient_perp_collateral"
	}
	return candidates
}

func (s Study) buySpot(sec security, size size, thesis Thesis, stress int) Candidate {
	c := newCandidate(sec, size, "SPOT_BUY", 1)
	if size.Spot == nil {
		return c.rejected("missing_quote")
	}
	quote := size.Spot
	if status := s.observe(&c, quote.Observation); status != "" {
		return c.rejected(status)
	}
	loss := atBPS(c.notional, stress+quote.Costs.Stress)
	required := new(big.Rat).Add(c.notional, atBPS(c.notional, quote.Costs.Stress))
	c.ExecutionCostBPS = &quote.Costs.Base
	s.value(&c, thesis, thesis.ExpectedReturn-quote.Costs.Base, loss)
	c.RequiredCapitalUSD = usd(required, true)
	if c.notional.Cmp(s.Policy.MaxNotional.Rat) > 0 || loss.Cmp(s.Account.RemainingStressBudget.Rat) > 0 {
		return c.rejected("position_risk_limit")
	}
	if c.Status == "eligible" && required.Cmp(s.Account.SpotCash.Rat) > 0 {
		return c.rejected("insufficient_spot_cash")
	}
	return c
}

func (s Study) reduceSpot(sec security, size size, thesis Thesis) Candidate {
	if thesis.ExpectedReturn >= 0 {
		return newCandidate(sec, size, "HOLD_SPOT", 1).rejected("no_bearish_thesis")
	}
	action := "SPOT_REDUCE"
	if cmp := size.Notional.Cmp(sec.HeldSpot.Notional.Rat); cmp == 0 {
		action = "SPOT_CLOSE"
	} else if cmp > 0 {
		return newCandidate(sec, size, action, 1).rejected("exceeds_held_spot")
	}
	c := newCandidate(sec, size, action, 1)
	if size.Spot == nil || size.Spot.Observation.MarketID != sec.HeldSpot.MarketID {
		return c.rejected("held_spot_quote_unavailable")
	}
	if status := s.observe(&c, size.Spot.Observation); status != "" {
		return c.rejected(status)
	}
	exit := size.Spot.Exit
	c.ExecutionCostBPS = &exit
	stress := max(s.Policy.MinStressMove, sec.StressUp)
	s.value(&c, thesis, -thesis.ExpectedReturn-exit, atBPS(c.notional, stress+exit))
	return c
}

func (s Study) openPerp(sec security, size size, thesis Thesis, side string, stress, leverage int) Candidate {
	c := newCandidate(sec, size, side, leverage)
	quote := size.Perp
	if quote == nil {
		return c.rejected("missing_quote")
	}
	if status := s.observe(&c, quote.Observation); status != "" {
		return c.rejected(status)
	}
	if (quote.MarginMode != "isolated" && quote.MarginMode != "strict_isolated") || leverage > quote.MaxLeverage {
		return c.rejected("unsupported_isolated_margin")
	}
	if quote.ExternalSessionUntil == nil ||
		quote.ExternalSessionUntil.Before(s.AsOf.Add(time.Duration(s.Horizon)*time.Hour)) {
		return c.rejected("external_session_does_not_cover_horizon")
	}
	if quote.MarkBasis > s.Policy.MaxBasis {
		return c.rejected("mark_basis_above_limit")
	}
	costs := quote.Long
	if side == "PERP_SHORT" {
		costs = quote.Short
	}
	if costs == nil {
		return c.rejected("side_unavailable")
	}
	// Expected receipts are upside only. Stress includes a possible sign reversal.
	fundingCost := max(0, costs.Funding.Forecast)
	c.CappedFundingCostBPS = &fundingCost
	c.ExecutionCostBPS, c.BasisCostBPS = &costs.Execution.Base, &costs.Basis.Base
	cost := costs.Execution.Base + costs.Basis.Base + fundingCost
	stressCost := costs.Execution.Stress + costs.Basis.Stress + costs.Funding.Stress
	loss := atBPS(c.notional, stress+stressCost)
	s.value(&c, thesis, abs(thesis.ExpectedReturn)-cost, loss)

	// Common exposure is based on the underlying. Bound initial margin from the
	// absolute mark basis in the snapshot, and maintenance at the adverse mark.
	marginFloor := new(big.Rat).Quo(c.notional, big.NewRat(int64(leverage), 1))
	marginUpper := atBPS(marginFloor, bpsScale+quote.MarkBasis)
	stressedNotional := atBPS(c.notional, bpsScale+quote.MarkBasis+stress+costs.Basis.Stress)
	maintenance := atBPS(stressedNotional, quote.Maintenance+s.Policy.MaintenanceBuffer)
	// Use a lower initial margin bound when checking liquidation headroom.
	marginLower := atBPS(marginFloor, bpsScale-quote.MarkBasis)
	headroom := new(big.Rat).Sub(marginLower, new(big.Rat).Add(loss, maintenance))
	expense := atBPS(c.notional, stressCost)
	required := new(big.Rat).Add(marginUpper, expense)
	c.RequiredCapitalUSD = usd(required, true)
	c.MarginHeadroomUSD = usd(headroom, false)
	switch {
	case c.notional.Cmp(s.Policy.MaxNotional.Rat) > 0 || loss.Cmp(s.Account.RemainingStressBudget.Rat) > 0:
		return c.rejected("position_risk_limit")
	case headroom.Cmp(big.NewRat(1, 100_000_000)) < 0:
		return c.rejected("insufficient_stress_margin_headroom")
	case c.Status != "eligible":
		return c
	case required.Cmp(s.Account.PerpCollateral.Rat) > 0:
		return c.rejected("insufficient_perp_collateral")
	case required.Cmp(s.Policy.MaxPerpCapital.Rat) > 0:
		return c.rejected("perp_capital_at_risk_limit")
	default:
		return c
	}
}

func (s Study) observe(c *Candidate, o observation) string {
	c.MarketID, c.ObservationSHA256 = o.MarketID, o.SHA256
	if o.AvailableAt.After(s.AsOf) {
		return "not_yet_available"
	}
	if s.AsOf.Sub(o.ObservedAt) > time.Duration(s.Policy.MaxObservationAge)*time.Second {
		return "stale_observation"
	}
	return ""
}

func (s Study) value(c *Candidate, thesis Thesis, pnlBeforeUncertaintyBPS int, stress *big.Rat) {
	uncertainty := max(thesis.Uncertainty, s.Policy.MinUncertainty)
	c.UncertaintyUsedBPS = &uncertainty
	netBPS := pnlBeforeUncertaintyBPS - uncertainty
	net := atBPS(c.notional, netBPS)
	value := new(big.Rat).Sub(net, atBPS(stress, s.Policy.StressLossPenalty))
	c.net, c.value = net, value
	c.NetReturnBPS, c.NetUSD = &netBPS, usd(net, false)
	c.StressLossUSD, c.RiskAdjustedUSD = usd(stress, true), usd(value, false)
	c.Status = "eligible"
	if netBPS < s.Policy.MinNetEdge || net.Sign() <= 0 || value.Sign() <= 0 {
		c.Status = "insufficient_net_edge"
	}
}

func (s Study) comparePerpWithSpot(perp, spot Candidate) Candidate {
	if perp.Status != "eligible" || perp.Action != "PERP_LONG" {
		return perp
	}
	hurdle := atBPS(perp.notional, s.Policy.MinPerpAdvantage)
	baseNet, baseValue := new(big.Rat), new(big.Rat)
	if spot.Status == "eligible" {
		baseNet, baseValue = spot.net, spot.value
	}
	if perp.net.Cmp(new(big.Rat).Add(baseNet, hurdle)) <= 0 ||
		perp.value.Cmp(new(big.Rat).Add(baseValue, hurdle)) <= 0 {
		return perp.rejected("insufficient_advantage_over_spot_or_cash")
	}
	return perp
}

func (s Study) compareLeverage(candidates []Candidate, best *Candidate) {
	baseNet, baseValue := new(big.Rat), new(big.Rat)
	if best != nil {
		baseNet, baseValue = best.net, best.value
	}
	for i := range candidates {
		c := &candidates[i]
		if c.Leverage <= 1 || c.Status != "eligible" {
			continue
		}
		extraNet := new(big.Rat).Sub(c.net, baseNet)
		extraValue := new(big.Rat).Sub(c.value, baseValue)
		c.ExtraNetUSD, c.ExtraValueUSD = usd(extraNet, false), usd(extraValue, false)
		if extraNet.Cmp(s.Policy.MinLeverageGain.Rat) <= 0 || extraValue.Cmp(s.Policy.MinLeverageGain.Rat) <= 0 {
			c.Status = "insufficient_incremental_leverage_value"
		}
	}
}

func newCandidate(sec security, size size, action string, leverage int) Candidate {
	return Candidate{
		SecurityKey: sec.SecurityKey, Action: action, Leverage: leverage,
		NotionalUSD: *usd(size.Notional.Rat, false), notional: size.Notional.Rat,
	}
}

func (c Candidate) rejected(reason string) Candidate {
	c.Status = reason
	return c
}

func compareCandidates(a, b Candidate) int {
	if cmp := strings.Compare(a.ID, b.ID); cmp != 0 {
		return cmp
	}
	if cmp := a.notional.Cmp(b.notional); cmp != 0 {
		return cmp
	}
	if cmp := strings.Compare(a.Action, b.Action); cmp != 0 {
		return cmp
	}
	return a.Leverage - b.Leverage
}

func bestCandidate(candidates []Candidate, maxLeverage int) *Candidate {
	var best *Candidate
	for i := range candidates {
		c := &candidates[i]
		if c.Status != "eligible" || c.Leverage > maxLeverage {
			continue
		}
		if best == nil || c.value.Cmp(best.value) > 0 ||
			(c.value.Cmp(best.value) == 0 && preferredOnTie(c, best)) {
			best = c
		}
	}
	return best
}

func preferredOnTie(a, b *Candidate) bool {
	if a.Leverage != b.Leverage {
		return a.Leverage < b.Leverage
	}
	if aPerp, bPerp := strings.HasPrefix(a.Action, "PERP_"), strings.HasPrefix(b.Action, "PERP_"); aPerp != bPerp {
		return !aPerp
	}
	return a.notional.Cmp(b.notional) < 0
}

func atBPS(value *big.Rat, bps int) *big.Rat {
	return new(big.Rat).Mul(value, big.NewRat(int64(bps), bpsScale))
}

func usd(value *big.Rat, roundUp bool) *string {
	scale := big.NewInt(100_000_000)
	scaled := new(big.Int).Mul(value.Num(), scale)
	if roundUp {
		scaled.Neg(scaled)
	}
	units := new(big.Int).Div(scaled, value.Denom())
	if roundUp {
		units.Neg(units)
	}
	rounded := new(big.Rat).SetFrac(units, scale).FloatString(8)
	formatted := strings.TrimRight(strings.TrimRight(rounded, "0"), ".")
	return &formatted
}
