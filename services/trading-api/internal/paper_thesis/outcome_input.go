package paper_thesis

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"time"

	"tradingbot/trading-api/internal/accounts"
	"tradingbot/trading-api/internal/contract"
	"tradingbot/trading-api/internal/paper_workflow"
)

const MaxOutcomeInputBytes = 4 * 1024 * 1024

type OutcomeRequest struct {
	Version                  string                           `json:"schema_version"`
	AccountID                string                           `json:"account_id"`
	OpeningOrderID           string                           `json:"opening_order_id"`
	AsOf                     time.Time                        `json:"as_of"`
	Source                   string                           `json:"source_mode"`
	MaxObservationAgeSeconds int                              `json:"max_observation_age_seconds"`
	ExitObservations         []ExitObservation                `json:"exit_observations"`
	ExitTape                 *paper_workflow.ArchiveReference `json:"exit_tape,omitempty"`
	ExitRegistry             *paper_workflow.ArchiveReference `json:"exit_identity_registry,omitempty"`
	Benchmark                *BenchmarkInput                  `json:"benchmark,omitempty"`
}

// ExitObservation is an executable simulated SELL observation for the exact
// raw quantity. The quoted amounts are for that whole quantity.
type ExitObservation struct {
	ID          string              `json:"observation_id"`
	Instrument  accounts.Instrument `json:"instrument"`
	QuantityRaw string              `json:"quantity_raw"`
	GrossUSD    string              `json:"gross_usd"`
	FeeUSD      string              `json:"fee_usd"`
	ObservedAt  time.Time           `json:"observed_at"`
	AvailableAt time.Time           `json:"available_at"`
	ExpiresAt   time.Time           `json:"expires_at"`
}

type BenchmarkInput struct {
	ID     string           `json:"series_id"`
	Levels []BenchmarkLevel `json:"levels"`
}

type BenchmarkLevel struct {
	Level       string    `json:"level"`
	ObservedAt  time.Time `json:"observed_at"`
	AvailableAt time.Time `json:"available_at"`
}

type OutcomeInput struct {
	request OutcomeRequest
	digest  string
}

// ParseOutcomeRequest pins the exact local input and validates the external
// observations before they can be used for a marked outcome.
func ParseOutcomeRequest(raw []byte, expectedSHA256 string) (OutcomeInput, error) {
	if len(raw) > MaxOutcomeInputBytes {
		return OutcomeInput{}, errors.New("paper thesis outcome request exceeds the byte limit")
	}
	hash := sha256.Sum256(raw)
	if hex.EncodeToString(hash[:]) != expectedSHA256 {
		return OutcomeInput{}, errors.New("paper thesis outcome request SHA-256 mismatch")
	}
	var request OutcomeRequest
	if err := contract.Decode(raw, contract.PaperThesisOutcomeRequest, &request); err != nil || request.AsOf.Year() <= 0 || request.AsOf.IsZero() {
		return OutcomeInput{}, errors.New("invalid paper thesis outcome request schema")
	}
	if request.ExitTape != nil && (request.ExitRegistry == nil || !fs.ValidPath(request.ExitTape.Ref) || !fs.ValidPath(request.ExitRegistry.Ref) ||
		request.ExitTape.Ref == request.ExitRegistry.Ref || request.ExitTape.SHA256 == request.ExitRegistry.SHA256) {
		return OutcomeInput{}, errors.New("invalid independent paper thesis exit archive pins")
	}
	type window struct {
		Observed  time.Time
		Available time.Time
	}
	type quoteKey struct {
		window
		Instrument accounts.Instrument
		Quantity   string
	}
	ids := make(map[string]bool)
	observations := make(map[quoteKey]bool)
	for i := range request.ExitObservations {
		quote := &request.ExitObservations[i]
		quote.ObservedAt, quote.AvailableAt, quote.ExpiresAt = quote.ObservedAt.UTC(), quote.AvailableAt.UTC(), quote.ExpiresAt.UTC()
		key := quoteKey{window{quote.ObservedAt, quote.AvailableAt}, quote.Instrument, quote.QuantityRaw}
		gross := decimal(quote.GrossUSD)
		if !validWindow(quote.ObservedAt, quote.AvailableAt) || !quote.ExpiresAt.After(quote.ObservedAt) ||
			gross.Sign() <= 0 || gross.Cmp(decimal(quote.FeeUSD)) < 0 || ids[quote.ID] || observations[key] {
			return OutcomeInput{}, errors.New("invalid or ambiguous paper thesis exit observation")
		}
		ids[quote.ID], observations[key] = true, true
	}
	if request.Benchmark != nil {
		levels := make(map[window]bool)
		for i := range request.Benchmark.Levels {
			level := &request.Benchmark.Levels[i]
			level.ObservedAt, level.AvailableAt = level.ObservedAt.UTC(), level.AvailableAt.UTC()
			key := window{level.ObservedAt, level.AvailableAt}
			if !validWindow(level.ObservedAt, level.AvailableAt) || decimal(level.Level).Sign() <= 0 || levels[key] {
				return OutcomeInput{}, errors.New("invalid or ambiguous paper thesis benchmark level")
			}
			levels[key] = true
		}
	}
	return OutcomeInput{request: request, digest: expectedSHA256}, nil
}

func validWindow(observed, available time.Time) bool {
	return observed.Year() > 0 && !observed.IsZero() && !available.Before(observed)
}
