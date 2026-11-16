// Package papersim applies a pinned, synthetic quote to a virtual paper ledger.
package papersim

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"time"
	"unicode/utf8"

	"tradingbot/control/internal/contract"
	"tradingbot/control/internal/ledger"
)

const (
	Version       = "0.1"
	Source        = "synthetic_fixture"
	MaxInputBytes = 256 * 1024
)

var (
	idPattern         = regexp.MustCompile(`^[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$`)
	instrumentPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:._-]{0,127}$`)
	quantityPattern   = regexp.MustCompile(`^[1-9][0-9]{0,37}$`)
	usdPattern        = regexp.MustCompile(`^(?:0|[1-9][0-9]{0,29})(?:\.[0-9]{1,8})?$`)
)

type Intent struct {
	Version         string            `json:"paper_intent_schema_version"`
	AccountID       string            `json:"account_id"`
	DecisionID      string            `json:"decision_id"`
	SnapshotVersion int64             `json:"snapshot_version"`
	Instrument      ledger.Instrument `json:"instrument"`
	Side            string            `json:"side"`
	QuantityRaw     string            `json:"quantity_raw"`
	GrossLimitUSD   string            `json:"gross_limit_usd"`
	MaxFeeUSD       string            `json:"max_fee_usd"`
	DecidedAt       time.Time         `json:"decided_at"`
	ExpiresAt       time.Time         `json:"expires_at"`
	QuoteSHA256     string            `json:"quote_sha256"`
}

type Quote struct {
	Version     string            `json:"paper_quote_schema_version"`
	Source      string            `json:"source_mode"`
	Instrument  ledger.Instrument `json:"instrument"`
	Side        string            `json:"side"`
	QuantityRaw string            `json:"quantity_raw"`
	GrossUSD    string            `json:"gross_usd"`
	FeeBPS      *int              `json:"fee_bps"`
	FixedFeeUSD string            `json:"fixed_fee_usd"`
	ObservedAt  time.Time         `json:"observed_at"`
	ExpiresAt   time.Time         `json:"expires_at"`
	Execution   string            `json:"execution"`
}

type Store interface {
	Reserve(context.Context, ledger.ReservationRequest) (ledger.Order, bool, error)
	Resolve(context.Context, ledger.ResolutionRequest) (ledger.Order, bool, error)
}

type Result struct {
	Version             string       `json:"paper_replay_schema_version"`
	Source              string       `json:"source_mode"`
	QuoteSHA256         string       `json:"quote_sha256"`
	SimulatedExecution  string       `json:"simulated_execution"`
	ReservationRecorded bool         `json:"reservation_recorded"`
	OutcomeRecorded     bool         `json:"outcome_recorded"`
	Order               ledger.Order `json:"order"`
}

// FixturePreview describes the exact quote and deterministic order identity versioned by Run.
type FixturePreview struct {
	OrderID string
	GrossUSD string
	FeeUSD string
	quote Quote
	digest [32]byte
}

func ParseIntent(data []byte) (Intent, error) {
	var in Intent
	if !validDocument(data) || decodeFixture(data, &in) != nil {
		return Intent{}, errors.New("invalid paper fixture intent JSON")
	}
	return in, nil
}

func validDocument(data []byte) bool {
	return len(data) <= MaxInputBytes && utf8.Valid(data)
}

func decodeFixture(data []byte, target any) error {
	if err := contract.Decode(data, nil, target); err != nil {
		return err
	}
	// A missing integer would otherwise be indistinguishable from decimals=0.
	var fields struct {
		Instrument struct {
			Decimals *int `json:"decimals"`
		} `json:"instrument"`
	}
	if json.Unmarshal(data, &fields) != nil || fields.Instrument.Decimals == nil {
		return errors.New("missing paper fixture instrument decimals")
	}
	return nil
}

// Run applies the fixture to the current paper account. Retrying the same inputs
// after an uncertain commit reuses both request identities. The database checks
// quote expiry for a new reservation, so committed reservations remain replayable.
func Preview(in Intent, rawQuote []byte, now time.Time) (FixturePreview, error) {
	if !validDocument(rawQuote) {
		return FixturePreview{}, errors.New("invalid paper quote size or encoding")
	}
	digest := sha256.Sum256(rawQuote)
	if hex.EncodeToString(digest[:]) != in.QuoteSHA256 {
		return FixturePreview{}, errors.New("paper quote SHA-256 does not match intent")
	}
	var quote Quote
	if decodeFixture(rawQuote, &quote) != nil {
		return FixturePreview{}, errors.New("invalid paper quote JSON")
	}
	fee, err := validate(in, quote, now)
	if err != nil {
		return FixturePreview{}, err
	}
	in.DecidedAt, in.ExpiresAt = in.DecidedAt.UTC(), in.ExpiresAt.UTC()
	intentPayload, err := json.Marshal(in)
	if err != nil {
		return FixturePreview{}, fmt.Errorf("encode paper intent identity: %w", err)
	}
	return FixturePreview{OrderID:stableID("order", intentPayload), GrossUSD:quote.GrossUSD, FeeUSD:fee, quote:quote, digest:digest}, nil
}

func Run(ctx context.Context, store Store, in Intent, rawQuote []byte, now time.Time) (Result, error) {
	preview, err := Preview(in, rawQuote, now)
	if err != nil { return Result{}, err }
	orderID, quote := preview.OrderID, preview.quote
	order, reserved, err := store.Reserve(ctx, ledger.ReservationRequest{
		AccountID: in.AccountID, OrderID: orderID, DecisionID: in.DecisionID, QuoteID: stableID("quote", preview.digest[:]),
		SnapshotVersion: in.SnapshotVersion, Instrument: in.Instrument, Side: in.Side, QuantityRaw: in.QuantityRaw,
		GrossUSD: in.GrossLimitUSD, MaxFeeUSD: in.MaxFeeUSD, ExpiresAt: minTime(in.ExpiresAt, quote.ExpiresAt).UTC(),
	})
	if err != nil {
		return Result{}, fmt.Errorf("reserve paper fixture order: %w", err)
	}
	outcome := ledger.ResolutionRequest{AccountID: in.AccountID, OrderID: orderID, EventID: stableID("outcome", []byte(orderID))}
	switch quote.Execution {
	case "fill":
		// A simulated observation cannot precede the quote or the durable
		// reservation. Preserve the same timestamp for reconciliation after TTL.
		observed := quote.ObservedAt.Truncate(time.Microsecond)
		if observed.Before(quote.ObservedAt) {
			observed = observed.Add(time.Microsecond)
		}
		if order.CreatedAt.After(observed) {
			observed = order.CreatedAt
		}
		outcome.Outcome, outcome.GrossUSD, outcome.FeeUSD, outcome.ObservedAt = "filled", quote.GrossUSD, preview.FeeUSD, observed
	case "reject":
		outcome.Outcome = "cancelled"
	case "timeout":
		outcome.Outcome = "unknown"
	}
	order, resolved, err := store.Resolve(ctx, outcome)
	if err != nil {
		return Result{}, fmt.Errorf("resolve paper fixture order: %w", err)
	}
	return Result{
		Version: Version, Source: Source, QuoteSHA256: in.QuoteSHA256, SimulatedExecution: quote.Execution,
		ReservationRecorded: reserved, OutcomeRecorded: resolved, Order: order,
	}, nil
}

func validate(in Intent, q Quote, now time.Time) (string, error) {
	if in.Version != Version || q.Version != Version || q.Source != Source {
		return "", errors.New("unsupported paper fixture version or source")
	}
	if !idPattern.MatchString(in.AccountID) || !idPattern.MatchString(in.DecisionID) || in.SnapshotVersion < 1 ||
		!instrumentPattern.MatchString(in.Instrument.ID) || !instrumentPattern.MatchString(in.Instrument.RegistryVersion) ||
		in.Instrument.Decimals < 0 || in.Instrument.Decimals > 18 || (in.Side != "BUY" && in.Side != "SELL") ||
		!quantityPattern.MatchString(in.QuantityRaw) || in.Instrument != q.Instrument || in.Side != q.Side || in.QuantityRaw != q.QuantityRaw {
		return "", errors.New("paper quote does not match intent identity or quantity")
	}
	if in.DecidedAt.IsZero() || !q.ObservedAt.After(in.DecidedAt) || q.ObservedAt.After(now) ||
		!q.ObservedAt.Before(in.ExpiresAt) || !q.ObservedAt.Before(q.ExpiresAt) {
		return "", errors.New("paper quote lies outside the decision window")
	}
	if q.Execution != "fill" && q.Execution != "reject" && q.Execution != "timeout" {
		return "", errors.New("unknown synthetic paper execution outcome")
	}
	if !usdPattern.MatchString(in.GrossLimitUSD) || !usdPattern.MatchString(in.MaxFeeUSD) ||
		!usdPattern.MatchString(q.GrossUSD) || !usdPattern.MatchString(q.FixedFeeUSD) ||
		q.FeeBPS == nil || *q.FeeBPS < 0 || *q.FeeBPS > 10000 {
		return "", errors.New("invalid paper quote money or fee model")
	}
	gross, limit, cap := usd(q.GrossUSD), usd(in.GrossLimitUSD), usd(in.MaxFeeUSD)
	if gross.Sign() <= 0 || limit.Sign() <= 0 || (in.Side == "BUY" && gross.Cmp(limit) > 0) ||
		(in.Side == "SELL" && (gross.Cmp(limit) < 0 || cap.Cmp(limit) > 0)) {
		return "", errors.New("paper quote gross violates intent limit")
	}
	fee := new(big.Rat).Mul(gross, big.NewRat(int64(*q.FeeBPS), 10000))
	fee.Add(fee, usd(q.FixedFeeUSD))
	// Upward rounding conservatively reserves every fractional USD quantum.
	fee.Mul(fee, big.NewRat(100000000, 1))
	units, remainder := new(big.Int), new(big.Int)
	units.QuoRem(fee.Num(), fee.Denom(), remainder)
	if remainder.Sign() > 0 {
		units.Add(units, big.NewInt(1))
	}
	fee.SetFrac(units, big.NewInt(100000000))
	if fee.Cmp(cap) > 0 {
		return "", errors.New("paper quote fee violates intent limit")
	}
	return fee.FloatString(8), nil
}

func usd(value string) *big.Rat {
	number, _ := new(big.Rat).SetString(value) // Caller validated the decimal grammar.
	return number
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func stableID(domain string, payload []byte) string {
	h := sha256.New()
	h.Write([]byte("tradingbot.papersim.v1/" + domain + "/"))
	h.Write(payload)
	b := h.Sum(nil)
	b[6] = b[6]&0x0f | 0x80
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
