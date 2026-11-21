package paper_thesis

import (
	"context"
	"fmt"
	"math/big"
	"strings"
	"time"

	"tradingbot/control/internal/ledger"
	"tradingbot/control/internal/paper_memory"
	"tradingbot/control/internal/paper_workflow"
)

const OutcomeVersion = "0.1"

type OutcomeReport struct {
	Version          string                          `json:"schema_version"`
	Status           string                          `json:"status"`
	InputSHA256      string                          `json:"input_sha256"`
	Source           string                          `json:"source_mode"`
	AccountID        string                          `json:"account_id"`
	AccountVersion   int64                           `json:"account_version"`
	AsOf             time.Time                       `json:"as_of"`
	JobID            string                          `json:"research_job_id"`
	DecisionID       string                          `json:"decision_id"`
	OpeningOrderID   string                          `json:"opening_order_id"`
	Instrument       ledger.Instrument               `json:"instrument"`
	FirstFillEventID string                          `json:"first_fill_event_id,omitempty"`
	OpenedAt         *time.Time                      `json:"opened_at,omitempty"`
	HorizonDeadline  *time.Time                      `json:"horizon_deadline,omitempty"`
	Actual           paper_memory.OpeningAmounts     `json:"actual"`
	Horizon          *HorizonOutcome                 `json:"horizon,omitempty"`
	Benchmark        *BenchmarkOutcome               `json:"benchmark,omitempty"`
	OpeningIdentity  *paper_workflow.OpeningIdentity `json:"opening_identity,omitempty"`
	ExitApplications []ExitApplication               `json:"exit_applications,omitempty"`
}

type HorizonOutcome struct {
	AsOf                time.Time                        `json:"as_of"`
	AccountVersion      int64                            `json:"account_version"`
	Basis               paper_memory.OpeningAmounts      `json:"basis"`
	ExitObservation     *ExitObservation                 `json:"exit_observation,omitempty"`
	EstimatedOpenNetUSD *string                          `json:"estimated_open_net_usd"`
	NetPnLUSD           *string                          `json:"net_pnl_usd"`
	TapeSHA256          string                           `json:"exit_tape_sha256,omitempty"`
	QuoteSHA256         string                           `json:"exit_quote_sha256,omitempty"`
	ExitRegistry        *paper_workflow.ArchiveReference `json:"exit_identity_registry,omitempty"`
}

type BenchmarkOutcome struct {
	ID        string          `json:"series_id"`
	Status    string          `json:"status"`
	Start     *BenchmarkLevel `json:"start_level,omitempty"`
	End       *BenchmarkLevel `json:"end_level,omitempty"`
	ReturnBPS *string         `json:"return_bps"`
}

// BuildOutcome uses the first known opening execution as the horizon anchor.
// It never includes a fill or external observation first known after the fixed
// horizon in its horizon result. The independent as-of view includes later fills.
func BuildOutcome(ctx context.Context, research ResearchSource, source openingSource, input OutcomeInput, knownThrough time.Time) (OutcomeReport, error) {
	return BuildOutcomeFromArchive(ctx, research, source, input, knownThrough, "")
}

func BuildOutcomeFromArchive(ctx context.Context, research ResearchSource, source openingSource, input OutcomeInput, knownThrough time.Time, archiveRoot string) (OutcomeReport, error) {
	req := input.request
	if input.digest == "" || req.AsOf.After(knownThrough) || knownThrough.IsZero() || req.ExitTape != nil && archiveRoot == "" {
		return OutcomeReport{}, ErrInvalid
	}
	opening, err := readOpening(ctx, research, source, req.AccountID, req.OpeningOrderID, req.AsOf)
	if err != nil {
		return OutcomeReport{}, err
	}
	view := opening.view
	if (view.OpeningIdentity != nil) != (req.Version == paper_workflow.IdentityVersion) {
		return OutcomeReport{}, fmt.Errorf("%w: horizon outcome version differs from the opening identity", ErrInvalid)
	}
	actual, err := paper_memory.AttributeOpening(opening.book, opening.fillIDs)
	if err != nil {
		return OutcomeReport{}, err
	}
	report := OutcomeReport{
		Version: req.Version, InputSHA256: input.digest, Source: req.Source,
		AccountID: view.AccountID, AccountVersion: view.AccountVersion, AsOf: view.AsOf,
		JobID: view.JobID, DecisionID: view.DecisionID, OpeningOrderID: view.OrderID, Instrument: view.Instrument,
		FirstFillEventID: view.FirstFillEventID, OpenedAt: view.OpenedAt, HorizonDeadline: view.ExpiresAt, Actual: actual,
		Status: "opening_unfilled", OpeningIdentity: view.OpeningIdentity,
	}
	if view.OpeningIdentity != nil {
		report.ExitApplications, err = verifyIdentitySales(ctx, research, source, opening, actual.ExitFillIDs)
		if err != nil {
			return OutcomeReport{}, err
		}
	}
	if view.OpenedAt == nil {
		if view.State == "unopened" {
			report.Status = "opening_cancelled"
		}
		return report, nil
	}
	deadline := *view.ExpiresAt
	if deadline.Year() > 9999 {
		return OutcomeReport{}, ErrInvalid
	}
	report.Status = "immature"
	if req.AsOf.Before(deadline) {
		return report, nil
	}

	basis, version := actual, view.AccountVersion
	if req.AsOf.After(deadline) {
		frozen, err := readOpening(ctx, research, source, view.AccountID, view.OrderID, deadline)
		if err != nil {
			return OutcomeReport{}, err
		}
		basis, err = paper_memory.AttributeOpening(frozen.book, frozen.fillIDs)
		if err != nil {
			return OutcomeReport{}, err
		}
		version = frozen.view.AccountVersion
	}
	report.Horizon = &HorizonOutcome{AsOf: deadline, AccountVersion: version, Basis: basis}
	age := time.Duration(req.MaxObservationAgeSeconds) * time.Second
	report.Benchmark = benchmarkOutcome(req.Benchmark, *view.OpenedAt, deadline, age)
	if basis.OpenQuantityRaw == "0" {
		report.Status = "settled_at_horizon"
		report.Horizon.NetPnLUSD = &basis.RealizedPnLUSD
		return report, nil
	}
	report.Status = "open_unpriced_at_horizon"
	var exit *ExitObservation
	if view.OpeningIdentity != nil {
		exit, err = identityHorizonExit(archiveRoot, req.ExitTape, req.ExitRegistry, view, report.Horizon, basis.OpenQuantityRaw, age)
		if err != nil {
			return OutcomeReport{}, err
		}
	} else {
		exit = executableExit(req.ExitObservations, view.Instrument, basis.OpenQuantityRaw, deadline, age)
	}
	if exit != nil {
		report.Status = "open_marked_at_horizon"
		estimated := decimal(exit.GrossUSD)
		estimated.Sub(estimated, decimal(exit.FeeUSD))
		net := new(big.Rat).Sub(estimated, decimal(basis.OpenCostBasisUSD))
		net.Add(net, decimal(basis.RealizedPnLUSD))
		report.Horizon.ExitObservation = exit
		report.Horizon.EstimatedOpenNetUSD, report.Horizon.NetPnLUSD = new(formatDecimal(estimated)), new(formatDecimal(net))
	}
	return report, nil
}

func executableExit(observations []ExitObservation, instrument ledger.Instrument, quantity string, cutoff time.Time, maxAge time.Duration) *ExitObservation {
	var chosen *ExitObservation
	for _, value := range observations {
		if value.Instrument != instrument || value.QuantityRaw != quantity || !value.ExpiresAt.After(cutoff) ||
			!knownObservation(value.ObservedAt, value.AvailableAt, cutoff, maxAge) {
			continue
		}
		if chosen == nil || newerObservation(value.ObservedAt, value.AvailableAt, chosen.ObservedAt, chosen.AvailableAt) {
			chosen = &value
		}
	}
	return chosen
}

func benchmarkOutcome(input *BenchmarkInput, start, end time.Time, maxAge time.Duration) *BenchmarkOutcome {
	if input == nil {
		return nil
	}
	out := &BenchmarkOutcome{ID: input.ID, Status: "unpriced"}
	out.Start, out.End = benchmarkAt(input.Levels, start, maxAge), benchmarkAt(input.Levels, end, maxAge)
	if out.Start != nil && out.End != nil && out.End.ObservedAt.After(out.Start.ObservedAt) {
		change := decimal(out.End.Level)
		change.Quo(change, decimal(out.Start.Level)).Sub(change, big.NewRat(1, 1)).Mul(change, big.NewRat(10000, 1))
		out.Status = "complete"
		out.ReturnBPS = new(formatDecimal(change))
	}
	return out
}

func benchmarkAt(levels []BenchmarkLevel, cutoff time.Time, maxAge time.Duration) *BenchmarkLevel {
	var chosen *BenchmarkLevel
	for _, candidate := range levels {
		if knownObservation(candidate.ObservedAt, candidate.AvailableAt, cutoff, maxAge) &&
			(chosen == nil || newerObservation(candidate.ObservedAt, candidate.AvailableAt, chosen.ObservedAt, chosen.AvailableAt)) {
			chosen = &candidate
		}
	}
	return chosen
}

func knownObservation(observed, available, cutoff time.Time, maxAge time.Duration) bool {
	return !available.After(cutoff) && cutoff.Sub(observed) <= maxAge
}

func newerObservation(observed, available, priorObserved, priorAvailable time.Time) bool {
	return observed.After(priorObserved) || observed.Equal(priorObserved) && available.After(priorAvailable)
}

func decimal(value string) *big.Rat {
	number, _ := new(big.Rat).SetString(value) // Validated at the document or ledger boundary.
	return number
}

func formatDecimal(value *big.Rat) string {
	// USD retains its exact scale-8 amount. Percentages round to the nearest
	// 0.00000001 bp, with ties directed away from zero.
	number := strings.TrimRight(strings.TrimRight(value.FloatString(8), "0"), ".")
	if number == "-0" {
		return "0"
	}
	return number
}
