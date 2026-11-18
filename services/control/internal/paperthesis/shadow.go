package paperthesis

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/big"
	"time"
	"unicode/utf8"

	"tradingbot/control/internal/contract"
	"tradingbot/control/internal/jobs"
	"tradingbot/control/internal/ledger"
	"tradingbot/control/internal/paperapply"
	"tradingbot/control/internal/papersim"
)

const MaxShadowInputBytes = 16 * 1024

type shadowRequest struct {
	Version                  string    `json:"schema_version"`
	Source                   string    `json:"source_mode"`
	AccountID                string    `json:"account_id"`
	ResearchJobID            string    `json:"research_job_id"`
	OpeningOrderID           string    `json:"opening_order_id"`
	AsOf                     time.Time `json:"as_of"`
	MaxObservationAgeSeconds int       `json:"max_observation_age_seconds"`
	EntrySHA256              string    `json:"entry_sha256"`
	ExitSHA256               string    `json:"exit_sha256"`
}

type shadowObservation struct {
	Version string `json:"schema_version"`
	Source  string `json:"source_mode"`
	Side    string `json:"side"`
	ExitObservation
}

type pinnedShadowObservation struct {
	SHA256      string            `json:"sha256"`
	Observation shadowObservation `json:"observation"`
}

type ShadowInput struct {
	request shadowRequest
	digest  string
	entry   pinnedShadowObservation
	exit    pinnedShadowObservation
}

// ParseShadowInput verifies each original byte sequence against its own pin.
func ParseShadowInput(raw []byte, digest string, entry, exit []byte) (ShadowInput, error) {
	var req shadowRequest
	if !matchShadowBytes(raw, digest) || contract.Decode(raw, contract.PaperShadowOutcomeRequest, &req) != nil ||
		req.AsOf.Year() <= 0 || req.AsOf.IsZero() || req.EntrySHA256 == req.ExitSHA256 {
		return ShadowInput{}, fmt.Errorf("%w: paper shadow request or SHA-256", ErrInvalid)
	}
	buy, err := parseShadowObservation(entry, req.EntrySHA256, "BUY")
	if err != nil {
		return ShadowInput{}, err
	}
	sell, err := parseShadowObservation(exit, req.ExitSHA256, "SELL")
	if err != nil {
		return ShadowInput{}, err
	}
	if buy.Observation.ID == sell.Observation.ID {
		return ShadowInput{}, fmt.Errorf("%w: paper shadow observations must have distinct identities", ErrInvalid)
	}
	return ShadowInput{request: req, digest: digest, entry: buy, exit: sell}, nil
}

func matchShadowBytes(raw []byte, digest string) bool {
	return len(raw) <= MaxShadowInputBytes && utf8.Valid(raw) && shadowDigest(raw) == digest
}

func shadowDigest(raw []byte) string {
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:])
}

func parseShadowObservation(raw []byte, digest, side string) (pinnedShadowObservation, error) {
	var obs shadowObservation
	if !matchShadowBytes(raw, digest) || contract.Decode(raw, contract.PaperShadowObservation, &obs) != nil || obs.Side != side {
		return pinnedShadowObservation{}, fmt.Errorf("%w: paper shadow %s observation or SHA-256", ErrInvalid, side)
	}
	obs.ObservedAt, obs.AvailableAt, obs.ExpiresAt = obs.ObservedAt.UTC(), obs.AvailableAt.UTC(), obs.ExpiresAt.UTC()
	gross := decimal(obs.GrossUSD)
	if !validWindow(obs.ObservedAt, obs.AvailableAt) || !obs.ExpiresAt.After(obs.AvailableAt) ||
		gross.Sign() <= 0 || (side == "SELL" && gross.Cmp(decimal(obs.FeeUSD)) < 0) {
		return pinnedShadowObservation{}, fmt.Errorf("%w: paper shadow %s observation time or amounts", ErrInvalid, side)
	}
	return pinnedShadowObservation{SHA256: digest, Observation: obs}, nil
}

type ShadowLedgerSource interface {
	GetApplication(context.Context, string) (ledger.ApplicationRecord, error)
	GetOrder(context.Context, string, string) (ledger.Order, error)
	Events(context.Context, string, int64, int) ([]ledger.Event, error)
}

type ShadowReport struct {
	Version             string                   `json:"schema_version"`
	Source              string                   `json:"source_mode"`
	Basis               string                   `json:"outcome_basis"`
	Status              string                   `json:"status"`
	InputSHA256         string                   `json:"input_sha256"`
	AccountID           string                   `json:"account_id"`
	ResearchJobID       string                   `json:"research_job_id"`
	OpeningOrderID      string                   `json:"opening_order_id"`
	DecisionID          string                   `json:"decision_id"`
	AsOf                time.Time                `json:"as_of"`
	HorizonDeadline     time.Time                `json:"horizon_deadline"`
	RejectionReason     string                   `json:"rejection_reason"`
	RejectionEventID    string                   `json:"rejection_event_id"`
	RejectedAt          time.Time                `json:"rejected_at"`
	RejectedQuoteSHA256 string                   `json:"rejected_quote_sha256"`
	Entry               pinnedShadowObservation  `json:"entry"`
	Exit                *pinnedShadowObservation `json:"exit,omitempty"`
	Label               *ShadowLabel             `json:"label,omitempty"`
}

type ShadowLabel struct {
	AvailableAt     time.Time `json:"available_at"`
	EntryCostUSD    string    `json:"entry_cost_usd"`
	ExitProceedsUSD string    `json:"exit_proceeds_usd"`
	NetPnLUSD       string    `json:"hypothetical_net_pnl_usd"`
	ReturnBPS       string    `json:"hypothetical_return_bps"`
}

func ReadShadowReport(ctx context.Context, dsn string, input ShadowInput) (ShadowReport, error) {
	var report ShadowReport
	err := ledger.WithReadSnapshot(ctx, dsn, func(snapshot ledger.ReadSnapshot) error {
		// The separate research read is immutable and its receipt must
		// predate the reservation in this ledger snapshot.
		research, err := jobs.ReadCompletedResearch(ctx, dsn, []string{input.request.ResearchJobID})
		if err != nil {
			return err
		}
		report, err = BuildShadowOutcome(ctx, research[0], snapshot, input, snapshot.Cutoff())
		return err
	})
	return report, err
}

// BuildShadowOutcome observes a completed rejection receipt and its original
// cancellation, then evaluates the separate hypothetical entry and exit.
func BuildShadowOutcome(ctx context.Context, research jobs.CompletedResearch, source ShadowLedgerSource, input ShadowInput, knownThrough time.Time) (ShadowReport, error) {
	req := input.request
	cutoff := req.AsOf.UTC()
	if input.digest == "" || knownThrough.IsZero() || cutoff.After(knownThrough) {
		return ShadowReport{}, fmt.Errorf("%w: paper shadow cutoff exceeds ledger knowledge", ErrInvalid)
	}
	order, err := source.GetOrder(ctx, req.AccountID, req.OpeningOrderID)
	if err != nil {
		return ShadowReport{}, fmt.Errorf("read paper shadow order: %w", err)
	}
	if order.AccountID != req.AccountID || order.OrderID != req.OpeningOrderID || order.ResearchJobID != req.ResearchJobID ||
		order.Side != "BUY" || order.Status != "cancelled" || order.AccountVersion < 2 || order.CreatedAt.IsZero() ||
		len(order.Fills) != 0 || order.Fill != nil || order.FilledQuantityRaw != "0" {
		return ShadowReport{}, fmt.Errorf("%w: paper shadow order must be a cancelled research opening with no fills", ErrInvalid)
	}
	app, err := source.GetApplication(ctx, req.ResearchJobID)
	if err != nil {
		return ShadowReport{}, fmt.Errorf("read paper shadow application: %w", err)
	}
	quoteHash, err := checkRejectionReceipt(app, order)
	if err != nil {
		return ShadowReport{}, err
	}
	event, err := readRejectionEvent(ctx, source, order, cutoff)
	if err != nil {
		return ShadowReport{}, err
	}
	intent, err := checkOpeningDecision(research.Job, research.Result, order.ReservationRequest, order.CreatedAt)
	if err != nil {
		return ShadowReport{}, err
	}

	entry, exit := input.entry.Observation, input.exit.Observation
	maxAge := time.Duration(req.MaxObservationAgeSeconds) * time.Second
	if entry.Instrument != order.Instrument || exit.Instrument != order.Instrument ||
		entry.QuantityRaw != order.QuantityRaw || exit.QuantityRaw != order.QuantityRaw ||
		!entry.ObservedAt.After(event.RecordedAt) || !entry.AvailableAt.Before(order.ExpiresAt) ||
		!knownObservation(entry.ObservedAt, entry.AvailableAt, entry.AvailableAt, maxAge) || entry.AvailableAt.After(cutoff) ||
		decimal(entry.GrossUSD).Cmp(decimal(order.GrossUSD)) > 0 || decimal(entry.FeeUSD).Cmp(decimal(order.MaxFeeUSD)) > 0 ||
		!exit.ObservedAt.After(entry.AvailableAt) {
		return ShadowReport{}, fmt.Errorf("%w: paper shadow observations do not match the order or entry window", ErrInvalid)
	}
	deadline := entry.AvailableAt.Add(time.Duration(intent.Horizon.Max) * time.Hour)
	if deadline.Year() > 9999 {
		return ShadowReport{}, fmt.Errorf("%w: paper shadow horizon is outside the supported calendar", ErrInvalid)
	}
	report := ShadowReport{
		Version: "0.1", Source: papersim.Source, Basis: "counterfactual", Status: "immature",
		InputSHA256: input.digest, AccountID: req.AccountID, ResearchJobID: req.ResearchJobID,
		OpeningOrderID: order.OrderID, DecisionID: order.DecisionID, AsOf: cutoff, HorizonDeadline: deadline,
		RejectionReason: "simulated_rejection", RejectionEventID: event.ID, RejectedAt: event.RecordedAt.UTC(),
		RejectedQuoteSHA256: quoteHash, Entry: input.entry,
	}
	if cutoff.Before(deadline) {
		return report, nil
	}
	if !knownObservation(exit.ObservedAt, exit.AvailableAt, deadline, maxAge) || !exit.ExpiresAt.After(deadline) {
		report.Status = "unpriced_at_horizon"
		return report, nil
	}
	cost := new(big.Rat).Add(decimal(entry.GrossUSD), decimal(entry.FeeUSD))
	proceeds := new(big.Rat).Sub(decimal(exit.GrossUSD), decimal(exit.FeeUSD))
	pnl := new(big.Rat).Sub(proceeds, cost)
	returnBPS := new(big.Rat).Mul(new(big.Rat).Quo(pnl, cost), big.NewRat(10000, 1))
	report.Status, report.Exit = "labelled", &input.exit
	report.Label = &ShadowLabel{
		AvailableAt: deadline, EntryCostUSD: formatDecimal(cost), ExitProceedsUSD: formatDecimal(proceeds),
		NetPnLUSD: formatDecimal(pnl), ReturnBPS: formatDecimal(returnBPS),
	}
	return report, nil
}

func checkRejectionReceipt(app ledger.ApplicationRecord, order ledger.Order) (string, error) {
	var receipt paperapply.Result
	quote, err := papersim.ParseQuote(app.Quote)
	if err != nil || app.JobID != order.ResearchJobID || app.AccountID != order.AccountID ||
		app.OrderID != order.OrderID || app.SnapshotVersion != order.SnapshotVersion ||
		quote.Source != papersim.Source || quote.Execution != "reject" || quote.Side != order.Side ||
		quote.Instrument != order.Instrument || quote.QuantityRaw != order.QuantityRaw ||
		contract.Decode(app.Result, nil, &receipt) != nil || receipt.Version != paperapply.Version ||
		receipt.Status != "applied" || receipt.JobID != order.ResearchJobID || receipt.Execution == nil {
		return "", fmt.Errorf("%w: paper shadow order has no matching durable rejection receipt", ErrInvalid)
	}
	execution, digest := receipt.Execution, shadowDigest(app.Quote)
	stored, current := execution.Order.ReservationRequest, order.ReservationRequest
	stored.ExpiresAt, current.ExpiresAt = stored.ExpiresAt.UTC(), current.ExpiresAt.UTC()
	if execution.Version != quote.Version || execution.Source != papersim.Source || execution.SimulatedExecution != "reject" ||
		execution.QuoteSHA256 != digest || stored != current || execution.Order.Status != order.Status ||
		execution.Order.AccountVersion != order.AccountVersion || !execution.Order.CreatedAt.Equal(order.CreatedAt) ||
		execution.Order.Fill != nil || len(execution.Order.Fills) != 0 {
		return "", fmt.Errorf("%w: paper shadow receipt conflicts with the rejected order", ErrInvalid)
	}
	return digest, nil
}

func readRejectionEvent(ctx context.Context, source ShadowLedgerSource, order ledger.Order, cutoff time.Time) (ledger.Event, error) {
	events, err := source.Events(ctx, order.AccountID, order.AccountVersion-1, 1)
	if err != nil {
		return ledger.Event{}, fmt.Errorf("read paper shadow rejection: %w", err)
	}
	if len(events) != 1 {
		return ledger.Event{}, fmt.Errorf("%w: missing paper shadow rejection event", ErrInvalid)
	}
	event := events[0]
	want := ledger.ResolutionRequest{AccountID: order.AccountID, OrderID: order.OrderID,
		EventID: papersim.OutcomeEventID(order.OrderID), Outcome: "cancelled"}
	var payload ledger.ResolutionRequest
	if event.ID != want.EventID || event.Kind != "paper_order.cancelled" || event.AccountVersion != order.AccountVersion ||
		event.RecordedAt.Before(order.CreatedAt) || event.RecordedAt.After(cutoff) ||
		contract.Decode(event.Payload, nil, &payload) != nil || payload != want {
		return ledger.Event{}, fmt.Errorf("%w: paper shadow rejection event is invalid or unavailable at cutoff", ErrInvalid)
	}
	return event, nil
}
