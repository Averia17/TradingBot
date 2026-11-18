// Package papernav values virtual accounts at closed UTC day ends.
package papernav

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math/big"
	"slices"
	"strings"
	"time"

	"tradingbot/control/internal/contract"
	"tradingbot/control/internal/ledger"
	"tradingbot/control/internal/papermemory"
)

const (
	MaxInputBytes = 4 * 1024 * 1024
	maxDays       = 366
	version       = "0.1"
)

type Request struct {
	Version           string `json:"schema_version"`
	AccountID         string `json:"account_id"`
	StartDay          string `json:"start_day"`
	EndDay            string `json:"end_day"`
	MarkMaxAgeSeconds int    `json:"mark_max_age_seconds"`
	Source            Source `json:"valuation_source"`
	Marks             []Mark `json:"marks"`
}

type Source struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

type Mark struct {
	ID          string            `json:"mark_id"`
	Instrument  ledger.Instrument `json:"instrument"`
	PriceUSD    string            `json:"price_usd"`
	ObservedAt  time.Time         `json:"observed_at"`
	AvailableAt time.Time         `json:"available_at"`
}

type Input struct {
	request Request
	digest  string
	cutoffs []time.Time
}

type Report struct {
	Version           string              `json:"schema_version"`
	Status            string              `json:"status"`
	AccountID         string              `json:"account_id"`
	InputSHA256       string              `json:"input_sha256"`
	Source            Source              `json:"valuation_source"`
	MarkMaxAgeSeconds int                 `json:"mark_max_age_seconds"`
	NavBasis          string              `json:"nav_basis"`
	ExternalCostsUSD  *string             `json:"external_costs_usd"`
	MarkPolicy        string              `json:"mark_policy"`
	LedgerHashPolicy  string              `json:"ledger_hash_policy"`
	Funding           papermemory.Funding `json:"initial_funding"`
	DailyNAV          []Row               `json:"daily_nav"`
}

type Row struct {
	Day            string     `json:"day"`
	Cutoff         time.Time  `json:"cutoff"`
	AccountVersion int64      `json:"account_version"`
	LastEventID    string     `json:"last_event_id"`
	LedgerSHA256   string     `json:"ledger_prefix_sha256"`
	CashUSD        string     `json:"cash_usd"`
	FeesUSD        string     `json:"recorded_fees_usd"`
	NAVUSD         *string    `json:"nav_usd"`
	Positions      []Position `json:"positions"`
}

type Position struct {
	Instrument  ledger.Instrument `json:"instrument"`
	QuantityRaw string            `json:"quantity_raw"`
	Mark        *Mark             `json:"mark,omitempty"`
	Issue       string            `json:"issue,omitempty"`
}

// Parse pins exact request bytes and prepares the chronological mark stream.
func Parse(raw []byte, expectedSHA256 string) (Input, error) {
	if len(raw) > MaxInputBytes {
		return Input{}, errors.New("paper NAV request exceeds the byte limit")
	}
	sum := sha256.Sum256(raw)
	if digest := hex.EncodeToString(sum[:]); digest != expectedSHA256 {
		return Input{}, errors.New("paper NAV request SHA-256 mismatch")
	}
	var req Request
	if err := contract.Decode(raw, contract.PaperNAVRequest, &req); err != nil {
		return Input{}, errors.New("invalid paper NAV request schema")
	}
	start, err := time.Parse(time.DateOnly, req.StartDay)
	if err != nil || start.Year() == 0 {
		return Input{}, errors.New("invalid paper NAV start day")
	}
	end, err := time.Parse(time.DateOnly, req.EndDay)
	if err != nil || end.Before(start) || end.Sub(start) >= maxDays*24*time.Hour {
		return Input{}, errors.New("paper NAV day range must contain 1 to 366 days")
	}
	var cutoffs []time.Time
	for date := start; !date.After(end); date = date.AddDate(0, 0, 1) {
		cutoffs = append(cutoffs, date.AddDate(0, 0, 1).Add(-time.Nanosecond))
	}
	if err := prepareMarks(req.Marks); err != nil {
		return Input{}, err
	}
	return Input{request: req, digest: expectedSHA256, cutoffs: cutoffs}, nil
}

func prepareMarks(marks []Mark) error {
	type observation struct {
		Instrument ledger.Instrument
		At         time.Time
		Available  time.Time
	}
	ids := make(map[string]bool, len(marks))
	observations := make(map[observation]bool, len(marks))
	for i := range marks {
		m := &marks[i]
		m.ObservedAt, m.AvailableAt = m.ObservedAt.UTC(), m.AvailableAt.UTC()
		key := observation{m.Instrument, m.ObservedAt, m.AvailableAt}
		if ids[m.ID] || observations[key] {
			return errors.New("duplicate paper NAV mark identity or observation")
		}
		if m.ObservedAt.IsZero() || m.AvailableAt.Before(m.ObservedAt) || decimal(m.PriceUSD).Sign() <= 0 {
			return errors.New("invalid paper NAV mark price or timestamps")
		}
		ids[m.ID], observations[key] = true, true
	}
	slices.SortFunc(marks, func(a, b Mark) int { return a.AvailableAt.Compare(b.AvailableAt) })
	return nil
}

// Build values the closed UTC dates entirely inside the supplied ledger view.
func Build(ctx context.Context, ledgerView papermemory.EventReader, input Input, knownThrough time.Time) (Report, error) {
	if input.digest == "" || len(input.cutoffs) == 0 || input.cutoffs[len(input.cutoffs)-1].After(knownThrough) {
		return Report{}, errors.New("paper NAV requires a pinned request with closed UTC days")
	}
	req := input.request
	out := Report{
		Version: version, Status: "complete", AccountID: req.AccountID,
		InputSHA256: input.digest, Source: req.Source, MarkMaxAgeSeconds: req.MarkMaxAgeSeconds,
		NavBasis: "execution_net", MarkPolicy: "last_locally_available_observation_at_utc_day_end",
		LedgerHashPolicy: "recorded_event_json_lines_v1", DailyNAV: make([]Row, 0, len(input.cutoffs)),
	}
	age := time.Duration(req.MarkMaxAgeSeconds) * time.Second
	current := make(map[ledger.Instrument]Mark)
	marks := req.Marks
	err := papermemory.WalkValuations(ctx, ledgerView, req.AccountID, input.cutoffs, func(snapshot papermemory.ValuationState) error {
		for len(marks) > 0 && !marks[0].AvailableAt.After(snapshot.Cutoff) {
			next := marks[0]
			if prior, found := current[next.Instrument]; !found || !next.ObservedAt.Before(prior.ObservedAt) {
				current[next.Instrument] = next
			}
			marks = marks[1:]
		}
		entry := row(snapshot, current, age)
		if entry.NAVUSD == nil {
			out.Status = "incomplete"
		}
		out.Funding = snapshot.Funding
		out.DailyNAV = append(out.DailyNAV, entry)
		return nil
	})
	if err != nil {
		return Report{}, err
	}
	return out, nil
}

func row(snapshot papermemory.ValuationState, current map[ledger.Instrument]Mark, maxAge time.Duration) Row {
	result := Row{
		Day: snapshot.Cutoff.Format(time.DateOnly), Cutoff: snapshot.Cutoff, AccountVersion: snapshot.AccountVersion,
		LastEventID: snapshot.LastEventID, LedgerSHA256: snapshot.LedgerSHA256, CashUSD: snapshot.CashUSD,
		FeesUSD: snapshot.FeesUSD, Positions: make([]Position, 0, len(snapshot.Quantities)),
	}
	nav := decimal(result.CashUSD)
	complete := true
	for instrument, raw := range snapshot.Quantities {
		item := Position{Instrument: instrument, QuantityRaw: raw.String()}
		mark, ok := current[instrument]
		switch {
		case !ok:
			item.Issue = "mark_missing"
		case snapshot.Cutoff.Sub(mark.ObservedAt) > maxAge:
			item.Mark, item.Issue = &mark, "mark_stale"
		default:
			item.Mark = &mark
			denom := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(instrument.Decimals)), nil)
			quantity := new(big.Rat).SetFrac(raw, denom)
			nav.Add(nav, quantity.Mul(quantity, decimal(mark.PriceUSD)))
		}
		complete = complete && item.Issue == ""
		result.Positions = append(result.Positions, item)
	}
	slices.SortFunc(result.Positions, func(a, b Position) int {
		if c := strings.Compare(a.Instrument.ID, b.Instrument.ID); c != 0 {
			return c
		}
		if c := strings.Compare(a.Instrument.RegistryVersion, b.Instrument.RegistryVersion); c != 0 {
			return c
		}
		return a.Instrument.Decimals - b.Instrument.Decimals
	})
	if complete {
		// Positions can carry more precision than scale-8 cash. Sum all exact
		// values first, then round the nonnegative total down once.
		const scale = 100000000
		numerator := new(big.Int).Mul(nav.Num(), big.NewInt(scale))
		units := numerator.Quo(numerator, nav.Denom())
		value := new(big.Rat).SetFrac(units, big.NewInt(scale)).FloatString(8)
		result.NAVUSD = &value
	}
	return result
}

func decimal(value string) *big.Rat {
	result, _ := new(big.Rat).SetString(value) // The wire or ledger projection already validated this amount.
	return result
}
