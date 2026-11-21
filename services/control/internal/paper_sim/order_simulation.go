// Package paper_sim applies a pinned, synthetic quote to a virtual paper ledger.
package paper_sim

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
	Version               = "0.1"
	PartialVersion        = "0.2"
	IdentityIntentVersion = "0.2"
	IdentityQuoteVersion  = "0.3"
	Source                = "synthetic_fixture"
	MaxInputBytes         = 256 * 1024
)

var (
	idPattern         = regexp.MustCompile(`^[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$`)
	instrumentPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:._-]{0,127}$`)
	quantityPattern   = regexp.MustCompile(`^[1-9][0-9]{0,37}$`)
	usdPattern        = regexp.MustCompile(`^(?:0|[1-9][0-9]{0,29})(?:\.[0-9]{1,8})?$`)
)

type Intent struct {
	Version                string            `json:"paper_intent_schema_version"`
	AccountID              string            `json:"account_id"`
	DecisionID             string            `json:"decision_id"`
	SnapshotVersion        int64             `json:"snapshot_version"`
	Instrument             ledger.Instrument `json:"instrument"`
	Side                   string            `json:"side"`
	QuantityRaw            string            `json:"quantity_raw"`
	GrossLimitUSD          string            `json:"gross_limit_usd"`
	MaxFeeUSD              string            `json:"max_fee_usd"`
	DecidedAt              time.Time         `json:"decided_at"`
	ExpiresAt              time.Time         `json:"expires_at"`
	QuoteSHA256            string            `json:"quote_sha256"`
	SecurityID             string            `json:"security_id,omitempty"`
	IdentityRegistrySHA256 string            `json:"identity_registry_sha256,omitempty"`
}

type Quote struct {
	Version                string            `json:"paper_quote_schema_version"`
	Source                 string            `json:"source_mode"`
	Instrument             ledger.Instrument `json:"instrument"`
	Side                   string            `json:"side"`
	QuantityRaw            string            `json:"quantity_raw"`
	ExecutedQuantityRaw    string            `json:"executed_quantity_raw,omitempty"`
	GrossUSD               string            `json:"gross_usd"`
	FeeBPS                 *int              `json:"fee_bps"`
	FixedFeeUSD            string            `json:"fixed_fee_usd"`
	ObservedAt             time.Time         `json:"observed_at"`
	ExpiresAt              time.Time         `json:"expires_at"`
	Execution              string            `json:"execution"`
	SecurityID             string            `json:"security_id,omitempty"`
	IdentityRegistrySHA256 string            `json:"identity_registry_sha256,omitempty"`
}

type Store interface {
	Reserve(context.Context, ledger.ReservationRequest) (ledger.Order, bool, error)
	Resolve(context.Context, ledger.ResolutionRequest) (ledger.Order, bool, error)
}

type Result struct {
	Version                string       `json:"paper_replay_schema_version"`
	Source                 string       `json:"source_mode"`
	QuoteSHA256            string       `json:"quote_sha256"`
	SimulatedExecution     string       `json:"simulated_execution"`
	ReservationRecorded    bool         `json:"reservation_recorded"`
	OutcomeRecorded        bool         `json:"outcome_recorded"`
	Order                  ledger.Order `json:"order"`
	SecurityID             string       `json:"security_id,omitempty"`
	IdentityRegistrySHA256 string       `json:"identity_registry_sha256,omitempty"`
}

// FixturePreview shows the amounts and order identity of a validated fixture.
type FixturePreview struct {
	OrderID   string
	GrossUSD  string
	FeeUSD    string
	ExpiresAt time.Time
	Fill      *FixtureFill
}

type FixtureFill struct {
	QuantityRaw string
	GrossUSD    string
	FeeUSD      string
}

// PreparedFixture retains the validated intent and quote used by the ledger.
type PreparedFixture struct {
	intent  Intent
	quote   Quote
	digest  [32]byte
	preview FixturePreview
}

// FixtureQuote keeps the schema-checked document with the hash of its source bytes.
type FixtureQuote struct {
	document Quote
	digest   [32]byte
}

func DecodeFixtureQuote(raw []byte) (FixtureQuote, error) {
	quote, err := ParseQuote(raw)
	if err != nil {
		return FixtureQuote{}, err
	}
	return FixtureQuote{document: quote, digest: sha256.Sum256(raw)}, nil
}

func (q FixtureQuote) Fields() Quote {
	fields := q.document
	if fields.FeeBPS != nil {
		fields.FeeBPS = new(*fields.FeeBPS)
	}
	return fields
}

func (q FixtureQuote) SHA256() string { return hex.EncodeToString(q.digest[:]) }

func ParseIntent(data []byte) (Intent, error) {
	var in Intent
	if !validDocument(data) || contract.Decode(data, contract.PaperFixtureIntent, &in) != nil {
		return Intent{}, errors.New("invalid paper fixture intent JSON")
	}
	return in, nil
}

func ParseQuote(data []byte) (Quote, error) {
	var quote Quote
	if !validDocument(data) || contract.Decode(data, contract.PaperFixtureQuote, &quote) != nil {
		return Quote{}, errors.New("invalid paper quote JSON")
	}
	return quote, nil
}

func validDocument(data []byte) bool {
	return len(data) <= MaxInputBytes && utf8.Valid(data)
}

// Preview checks the pinned quote and derives the stable reservation identity.
func Preview(in Intent, rawQuote []byte, now time.Time) (FixturePreview, error) {
	fixture, err := Prepare(in, rawQuote, now)
	if err != nil {
		return FixturePreview{}, err
	}
	return fixture.preview, nil
}

// Prepare validates the pinned quote once so it can be checked against policy
// and then applied without reinterpreting the quote or changing its identity.
func Prepare(in Intent, rawQuote []byte, now time.Time) (*PreparedFixture, error) {
	if !validDocument(rawQuote) {
		return nil, errors.New("invalid paper quote size or encoding")
	}
	digest := sha256.Sum256(rawQuote)
	if hex.EncodeToString(digest[:]) != in.QuoteSHA256 {
		return nil, errors.New("paper quote SHA-256 does not match intent")
	}
	quote, err := ParseQuote(rawQuote)
	if err != nil {
		return nil, err
	}
	return prepareFixture(in, quote, digest, now)
}

// Prepare binds a derived intent to the same quote that was used to construct it.
func (q FixtureQuote) Prepare(in Intent, now time.Time) (*PreparedFixture, error) {
	if q.document.Version == "" || q.SHA256() != in.QuoteSHA256 {
		return nil, errors.New("paper quote SHA-256 does not match intent")
	}
	return prepareFixture(in, q.document, q.digest, now)
}

func prepareFixture(in Intent, quote Quote, digest [32]byte, now time.Time) (*PreparedFixture, error) {
	fee, err := validate(in, quote, now)
	if err != nil {
		return nil, err
	}
	fill, err := simulatedFill(quote, fee)
	if err != nil {
		return nil, err
	}
	in.DecidedAt, in.ExpiresAt = in.DecidedAt.UTC(), in.ExpiresAt.UTC()
	intentPayload, err := json.Marshal(in)
	if err != nil {
		return nil, fmt.Errorf("encode paper intent identity: %w", err)
	}
	return &PreparedFixture{
		intent: in, quote: quote, digest: digest,
		preview: FixturePreview{
			OrderID: stableID("order", intentPayload), GrossUSD: quote.GrossUSD, FeeUSD: fee,
			ExpiresAt: minTime(in.ExpiresAt, quote.ExpiresAt).UTC(), Fill: fill,
		},
	}, nil
}

// Preview returns a copy for risk and authorization checks.
func (fixture *PreparedFixture) Preview() FixturePreview {
	preview := fixture.preview
	if preview.Fill != nil {
		preview.Fill = new(*preview.Fill)
	}
	return preview
}

// Run applies the fixture to the current paper account. Retrying the same inputs
// after an uncertain commit reuses both request identities. The database checks
// quote expiry for a new reservation, so committed reservations remain replayable.
func Run(ctx context.Context, store Store, in Intent, rawQuote []byte, now time.Time) (Result, error) {
	fixture, err := Prepare(in, rawQuote, now)
	if err != nil {
		return Result{}, err
	}
	return fixture.Run(ctx, store)
}

// Run reserves and resolves the order using the validated fixture.
func (fixture *PreparedFixture) Run(ctx context.Context, store Store) (Result, error) {
	if fixture == nil || fixture.preview.OrderID == "" {
		return Result{}, errors.New("paper fixture must be prepared before execution")
	}
	in, quote, preview := fixture.intent, fixture.quote, fixture.preview
	orderID := preview.OrderID
	order, reserved, err := store.Reserve(ctx, ledger.ReservationRequest{
		AccountID: in.AccountID, OrderID: orderID, DecisionID: in.DecisionID, QuoteID: stableID("quote", fixture.digest[:]),
		SnapshotVersion: in.SnapshotVersion, Instrument: in.Instrument, Side: in.Side, QuantityRaw: in.QuantityRaw,
		GrossUSD: in.GrossLimitUSD, MaxFeeUSD: in.MaxFeeUSD, ExpiresAt: preview.ExpiresAt,
	})
	if err != nil {
		return Result{}, fmt.Errorf("reserve paper fixture order: %w", err)
	}
	outcome := ledger.ResolutionRequest{AccountID: in.AccountID, OrderID: orderID, EventID: OutcomeEventID(orderID)}
	switch quote.Execution {
	case "fill", "partial":
		// A simulated observation cannot precede the quote or the durable
		// reservation. Preserve the same timestamp for reconciliation after TTL.
		observed := quote.ObservedAt.Truncate(time.Microsecond)
		if observed.Before(quote.ObservedAt) {
			observed = observed.Add(time.Microsecond)
		}
		if order.CreatedAt.After(observed) {
			observed = order.CreatedAt
		}
		outcome.Outcome, outcome.GrossUSD, outcome.FeeUSD, outcome.ObservedAt = "filled", preview.Fill.GrossUSD, preview.Fill.FeeUSD, observed
		if quote.Execution == "partial" {
			outcome.Outcome, outcome.QuantityRaw = "partially_filled", preview.Fill.QuantityRaw
		}
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
		Version: quote.Version, Source: Source, QuoteSHA256: in.QuoteSHA256, SimulatedExecution: quote.Execution,
		ReservationRecorded: reserved, OutcomeRecorded: resolved, Order: order,
		SecurityID: quote.SecurityID, IdentityRegistrySHA256: quote.IdentityRegistrySHA256,
	}, nil
}

// OutcomeEventID identifies the original fixture resolution across retries.
func OutcomeEventID(orderID string) string {
	return stableID("outcome", []byte(orderID))
}

func validate(in Intent, q Quote, now time.Time) (string, error) {
	switch in.Version {
	case Version:
		if q.Version == IdentityQuoteVersion || in.SecurityID != "" || in.IdentityRegistrySHA256 != "" {
			return "", errors.New("legacy paper intent cannot authorize a security-bound quote")
		}
	case IdentityIntentVersion:
		if q.Version != IdentityQuoteVersion || in.SecurityID != q.SecurityID || in.IdentityRegistrySHA256 != q.IdentityRegistrySHA256 {
			return "", errors.New("paper quote does not match the authorized security and registry")
		}
	default:
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
	if !usdPattern.MatchString(in.GrossLimitUSD) || !usdPattern.MatchString(in.MaxFeeUSD) {
		return "", errors.New("invalid paper intent money")
	}
	fee, err := q.FeeUSD()
	if err != nil {
		return "", err
	}
	gross, limit, cap := usd(q.GrossUSD), usd(in.GrossLimitUSD), usd(in.MaxFeeUSD)
	if gross.Sign() <= 0 || limit.Sign() <= 0 || (in.Side == "BUY" && gross.Cmp(limit) > 0) ||
		(in.Side == "SELL" && (gross.Cmp(limit) < 0 || cap.Cmp(limit) > 0)) {
		return "", errors.New("paper quote gross violates intent limit")
	}
	if usd(fee).Cmp(cap) > 0 {
		return "", errors.New("paper quote fee violates intent limit")
	}
	return fee, nil
}

func simulatedFill(q Quote, fee string) (*FixtureFill, error) {
	switch q.Execution {
	case "fill":
		return &FixtureFill{QuantityRaw: q.QuantityRaw, GrossUSD: q.GrossUSD, FeeUSD: fee}, nil
	case "partial":
		executed, _ := new(big.Int).SetString(q.ExecutedQuantityRaw, 10)
		requested, _ := new(big.Int).SetString(q.QuantityRaw, 10)
		if executed.Cmp(requested) >= 0 {
			return nil, errors.New("partial quote execution must leave an unfilled quantity")
		}
		// Round in the price-limit direction so decimal granularity cannot
		// breach the quoted unit price at the executed quantity.
		gross := new(big.Rat).Mul(usd(q.GrossUSD), new(big.Rat).SetFrac(executed, requested))
		gross = quantizeUSD(gross, q.Side == "SELL")
		if gross.Sign() == 0 {
			return nil, errors.New("partial quote gross is below the paper USD precision")
		}
		q.GrossUSD = gross.FloatString(8)
		fee, err := q.FeeUSD()
		if err != nil {
			return nil, err
		}
		if q.Side == "SELL" && usd(fee).Cmp(gross) > 0 {
			return nil, errors.New("partial paper sale fee exceeds its proceeds")
		}
		return &FixtureFill{QuantityRaw: q.ExecutedQuantityRaw, GrossUSD: q.GrossUSD, FeeUSD: fee}, nil
	default:
		return nil, nil
	}
}

// FeeUSD returns the explicit fee model rounded up to the ledger's USD scale.
func (q Quote) FeeUSD() (string, error) {
	if !usdPattern.MatchString(q.GrossUSD) || !usdPattern.MatchString(q.FixedFeeUSD) ||
		q.FeeBPS == nil || *q.FeeBPS < 0 || *q.FeeBPS > 10000 {
		return "", errors.New("invalid paper quote money or fee model")
	}
	fee := new(big.Rat).Mul(usd(q.GrossUSD), big.NewRat(int64(*q.FeeBPS), 10000))
	fee.Add(fee, usd(q.FixedFeeUSD))
	return quantizeUSD(fee, true).FloatString(8), nil
}

func quantizeUSD(amount *big.Rat, roundUp bool) *big.Rat {
	const scale = 100_000_000
	amount.Mul(amount, big.NewRat(scale, 1))
	units, remainder := new(big.Int), new(big.Int)
	units.QuoRem(amount.Num(), amount.Denom(), remainder)
	if roundUp && remainder.Sign() > 0 {
		units.Add(units, big.NewInt(1))
	}
	return amount.SetFrac(units, big.NewInt(scale))
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
	// This namespace is shared by persisted v1 order, quote and settlement IDs.
	const identityNamespace = "tradingbot.papersim.v1/"
	h.Write([]byte(identityNamespace + domain + "/"))
	h.Write(payload)
	b := h.Sum(nil)
	b[6] = b[6]&0x0f | 0x80
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
