package paperquotes

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math/big"
	"slices"
	"time"
)

// Result provides rankings and exclusions for one common share exposure. The
// input digest pins the exact fixture and all of its registry and reference data.
type Result struct {
	Version          string    `json:"schema_version"`
	Source           string    `json:"source_mode"`
	ExecutionEnabled bool      `json:"execution_enabled"`
	InputSHA256      string    `json:"input_sha256"`
	RegistryVersion  string    `json:"registry_snapshot_version"`
	AsOf             time.Time `json:"as_of"`
	Side             string    `json:"side"`
	UnderlyingID     string    `json:"underlying_id"`
	TargetShares     string    `json:"target_shares"`
	ReferenceUSD     string    `json:"reference_usd"`
	Quotes           []Row     `json:"quotes"`
}

// Row carries only normalized values for eligible fixtures. The rank uses the
// exact total and never relies on the rounded per-share or basis-point display.
type Row struct {
	ID                string `json:"id"`
	Wrapper           Token  `json:"wrapper"`
	Settlement        Token  `json:"settlement"`
	RegistryRevision  string `json:"registry_revision"`
	InputRaw          string `json:"input_raw"`
	ExpectedOutputRaw string `json:"expected_output_raw"`
	MinOutputRaw      string `json:"min_output_raw,omitempty"`
	Reason            string `json:"excluded_reason,omitempty"`
	Rank              int    `json:"rank,omitempty"`
	SettlementUSD     string `json:"settlement_usd,omitempty"`
	EmbeddedFeesUSD   string `json:"embedded_fees_usd,omitempty"`
	AdditionalFeesUSD string `json:"additional_fees_usd,omitempty"`
	EffectiveUSD      string `json:"effective_usd,omitempty"`
	USDPerShare       string `json:"usd_per_share,omitempty"`
	CostBPS           string `json:"cost_vs_reference_bps,omitempty"`
}

type evaluator struct {
	document
	maxAge       time.Duration
	shares       *big.Rat
	referenceUSD *big.Rat
	byMint       map[mintKey][]revision
	fx           map[mintKey]settlementFX
}

// Compare validates a bounded fixture and ranks by the worst-case guaranteed
// share or cash leg within the simulation. It has no execution credentials.
func Compare(data []byte) (Result, error) {
	doc, err := parseDocument(data)
	if err != nil {
		return Result{}, err
	}
	e, err := newEvaluator(doc)
	if err != nil {
		return Result{}, err
	}
	digest := sha256.Sum256(data)
	result := Result{
		Version: Version, Source: Source, InputSHA256: hex.EncodeToString(digest[:]), RegistryVersion: doc.Registry.Version,
		AsOf: doc.AsOf.UTC(), Side: doc.Market.Side, UnderlyingID: doc.Market.UnderlyingID,
		TargetShares: exactDecimal(e.shares), ReferenceUSD: exactDecimal(e.referenceUSD), Quotes: make([]Row, len(doc.Quotes)),
	}
	type candidate struct {
		index int
		value *big.Rat
	}
	var eligible []candidate
	seen := make(map[string]bool, len(doc.Quotes))
	for i, q := range doc.Quotes {
		if seen[q.ID] {
			return Result{}, errors.New("duplicate synthetic quote ID")
		}
		seen[q.ID] = true
		row, total := e.compare(q)
		result.Quotes[i] = row
		if total != nil {
			eligible = append(eligible, candidate{index: i, value: total})
		}
	}
	slices.SortFunc(eligible, func(left, right candidate) int {
		order := left.value.Cmp(right.value)
		if doc.Market.Side == "SELL" {
			order = -order
		}
		if order == 0 {
			return cmp.Compare(doc.Quotes[left.index].ID, doc.Quotes[right.index].ID)
		}
		return order
	})
	for i, candidate := range eligible {
		result.Quotes[candidate.index].Rank = i + 1
	}
	return result, nil
}

func newEvaluator(doc document) (evaluator, error) {
	e := evaluator{
		document: doc, maxAge: time.Duration(doc.Market.MaxAgeSeconds) * time.Second,
		shares: decimal(doc.Market.TargetShares), byMint: make(map[mintKey][]revision), fx: make(map[mintKey]settlementFX),
	}
	price := decimal(doc.Reference.USDPerShare)
	if price.Sign() <= 0 || e.shares.Sign() <= 0 || !fresh(doc.Reference.ObservedAt, doc.Reference.ExpiresAt, doc.AsOf, e.maxAge) {
		return evaluator{}, errors.New("invalid or stale synthetic market reference")
	}
	e.referenceUSD = new(big.Rat).Mul(e.shares, price)
	if doc.Market.Holding != nil && doc.Market.Holding.Token.Chain != doc.Market.Chain {
		return evaluator{}, errors.New("held instrument does not match the comparison chain")
	}
	for _, entry := range doc.Registry.Wrappers {
		if entry.AvailableAt.IsZero() || entry.EffectiveAt.IsZero() {
			return evaluator{}, errors.New("invalid wrapper registry timestamps")
		}
		key := entry.Token.key()
		for _, previous := range e.byMint[key] {
			if previous.Revision == entry.Revision || previous.EffectiveAt.Equal(entry.EffectiveAt) ||
				previous.UnderlyingID != entry.UnderlyingID || previous.IssuerID != entry.IssuerID {
				return evaluator{}, errors.New("ambiguous wrapper registry revision")
			}
		}
		e.byMint[key] = append(e.byMint[key], entry)
	}
	for _, rate := range doc.Reference.Settlements {
		if _, exists := e.fx[rate.Token.key()]; exists || decimal(rate.USDPerToken).Sign() <= 0 {
			return evaluator{}, errors.New("invalid or duplicate synthetic settlement FX")
		}
		e.fx[rate.Token.key()] = rate
	}
	return e, nil
}

func (e evaluator) compare(q quote) (Row, *big.Rat) {
	row := Row{
		ID: q.ID, Wrapper: q.Wrapper, Settlement: q.Settlement, RegistryRevision: q.Revision,
		InputRaw: q.InputRaw, ExpectedOutputRaw: q.ExpectedOutputRaw, MinOutputRaw: q.MinOutputRaw,
	}
	observed, fx, reason := e.bind(q)
	if reason != "" {
		row.Reason = reason
		return row, nil
	}
	wrapperRaw, settlementRaw := q.MinOutputRaw, q.InputRaw
	if q.Side == "SELL" {
		wrapperRaw, settlementRaw = q.InputRaw, q.MinOutputRaw
		if rawNumber(wrapperRaw).Cmp(rawNumber(e.Market.Holding.QuantityRaw)) > 0 {
			row.Reason = "insufficient_held_quantity"
			return row, nil
		}
	}
	quotedShares := tokenUnits(wrapperRaw, q.Wrapper.Decimals)
	quotedShares.Mul(quotedShares, new(big.Rat).SetFrac(rawNumber(observed.SharesPerToken.Numerator), rawNumber(observed.SharesPerToken.Denominator)))
	if quotedShares.Cmp(e.shares) != 0 {
		row.Reason = "share_quantity_mismatch"
		return row, nil
	}
	embedded, extra := new(big.Rat), new(big.Rat)
	feeIDs := make(map[string]bool, len(q.Fees))
	for _, fee := range q.Fees {
		if feeIDs[fee.ID] {
			row.Reason = "duplicate_fee_id"
			return row, nil
		}
		feeIDs[fee.ID] = true
		if fee.Treatment == "embedded" {
			embedded.Add(embedded, decimal(fee.AmountUSD))
		} else {
			extra.Add(extra, decimal(fee.AmountUSD))
		}
	}
	settlementUSD := tokenUnits(settlementRaw, q.Settlement.Decimals)
	settlementUSD.Mul(settlementUSD, decimal(fx.USDPerToken))
	effective := new(big.Rat).Add(settlementUSD, extra)
	cost := new(big.Rat)
	if q.Side == "SELL" {
		effective.Sub(settlementUSD, extra)
		if effective.Sign() <= 0 {
			row.Reason = "nonpositive_net_proceeds"
			return row, nil
		}
		cost.Sub(e.referenceUSD, effective)
	} else {
		cost.Sub(effective, e.referenceUSD)
	}
	row.SettlementUSD, row.EffectiveUSD = exactDecimal(settlementUSD), exactDecimal(effective)
	row.EmbeddedFeesUSD, row.AdditionalFeesUSD = exactDecimal(embedded), exactDecimal(extra)
	row.USDPerShare = new(big.Rat).Quo(effective, e.shares).FloatString(8)
	row.CostBPS = cost.Quo(cost, e.referenceUSD).Mul(cost, big.NewRat(10000, 1)).FloatString(8)
	return row, effective
}

func (e evaluator) bind(q quote) (revision, settlementFX, string) {
	if q.Mode != "synthetic_firm" {
		return revision{}, settlementFX{}, "indicative_quote"
	}
	if q.Side != e.Market.Side {
		return revision{}, settlementFX{}, "quote_side_mismatch"
	}
	if !fresh(q.ObservedAt, q.ExpiresAt, e.AsOf, e.maxAge) || q.ObservedAt.Before(e.Reference.ObservedAt) {
		return revision{}, settlementFX{}, "quote_stale"
	}
	if q.Wrapper.Chain != e.Market.Chain || q.Settlement.Chain != e.Market.Chain {
		return revision{}, settlementFX{}, "ineligible_chain"
	}
	if e.Market.Holding != nil && e.Market.Holding.Token != q.Wrapper {
		return revision{}, settlementFX{}, "wrong_held_wrapper"
	}
	if q.Wrapper.key() == q.Settlement.key() || activeRevision(e.byMint[q.Settlement.key()], e.AsOf).Revision != "" {
		return revision{}, settlementFX{}, "ineligible_settlement"
	}
	if rawNumber(q.MinOutputRaw).Cmp(rawNumber(q.ExpectedOutputRaw)) > 0 {
		return revision{}, settlementFX{}, "quote_output_below_minimum"
	}
	entries := e.byMint[q.Wrapper.key()]
	observed := activeRevision(entries, q.ObservedAt)
	if observed.Revision == "" {
		return revision{}, settlementFX{}, "wrapper_unknown_at_observation"
	}
	if observed.Revision != activeRevision(entries, e.AsOf).Revision {
		return revision{}, settlementFX{}, "registry_changed_after_quote"
	}
	if observed.Revision != q.Revision {
		return revision{}, settlementFX{}, "registry_revision_mismatch"
	}
	if observed.Token != q.Wrapper {
		return revision{}, settlementFX{}, "wrapper_identity_mismatch"
	}
	if observed.UnderlyingID != e.Market.UnderlyingID || !slices.Contains(e.Market.AllowedIssuers, observed.IssuerID) {
		return revision{}, settlementFX{}, "ineligible_wrapper"
	}
	fx, available := e.fx[q.Settlement.key()]
	if !available || fx.Token != q.Settlement {
		return revision{}, settlementFX{}, "settlement_fx_missing"
	}
	return observed, fx, ""
}

func activeRevision(entries []revision, asOf time.Time) revision {
	var active revision
	for _, entry := range entries {
		if !entry.AvailableAt.After(asOf) && !entry.EffectiveAt.After(asOf) && entry.EffectiveAt.After(active.EffectiveAt) {
			active = entry
		}
	}
	return active
}

func fresh(observed, expires, asOf time.Time, maxAge time.Duration) bool {
	return !observed.IsZero() && !asOf.IsZero() && !observed.After(asOf) && asOf.Before(expires) && asOf.Sub(observed) <= maxAge
}

func rawNumber(value string) *big.Int {
	integer, _ := new(big.Int).SetString(value, 10)
	return integer
}

func decimal(value string) *big.Rat {
	parsed, _ := new(big.Rat).SetString(value)
	return parsed
}

func tokenUnits(raw string, decimals int) *big.Rat {
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
	return new(big.Rat).SetFrac(rawNumber(raw), scale)
}

// Exact totals arise only from raw units and finite decimal prices and fees.
func exactDecimal(value *big.Rat) string {
	digits, _ := value.FloatPrec()
	return value.FloatString(digits)
}
