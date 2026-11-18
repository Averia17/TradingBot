package paperquotes

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"time"
	"unicode/utf8"

	"tradingbot/control/internal/contract"
)

// SummarizeStudy compares observations only within their pinned route and
// exposure. All missing slots remain visible in the declared sample.
func SummarizeStudy(planRaw []byte, planSHA string, archiveRaw []byte, archiveSHA string) (StudyReport, error) {
	var plan StudyPlan
	var archive StudyArchive
	if !pinnedStudyInput(planRaw, planSHA, MaxStudyPlanBytes) {
		return StudyReport{}, errors.New("invalid quote study plan SHA-256 or input size")
	}
	if !pinnedStudyInput(archiveRaw, archiveSHA, MaxStudyArchiveBytes) {
		return StudyReport{}, errors.New("invalid quote study observation SHA-256 or input size")
	}
	if contract.Decode(planRaw, studyPlanSchema, &plan) != nil ||
		contract.Decode(archiveRaw, studyArchiveSchema, &archive) != nil {
		return StudyReport{}, errors.New("invalid quote study plan or observation schema")
	}
	if plan.ID != archive.StudyID || planSHA != archive.PlanSHA256 {
		return StudyReport{}, errors.New("observations do not bind to the pinned study plan")
	}
	slots, err := studySlots(plan)
	if err != nil {
		return StudyReport{}, err
	}
	observations := make(map[string]StudyObservation, len(archive.Observations))
	for _, observation := range archive.Observations {
		if _, exists := observations[observation.SlotID]; exists {
			return StudyReport{}, fmt.Errorf("duplicate observation for slot %q", observation.SlotID)
		}
		observations[observation.SlotID] = observation
	}
	var report = StudyReport{
		Version: Version, Source: Source, ID: plan.ID, Scope: "simulated_one_way_quote_cost",
		PlanSHA256: planSHA, ArchiveSHA256: archiveSHA, CoverageComplete: true,
		Cells: make([]StudyCellReport, 0, len(plan.Cells)),
	}
	tradingDays := make(map[string]bool)
	seenQuotes := make(map[[2]string]bool)
	for _, cell := range plan.Cells {
		cellSlots := slots[cell.ID]
		rows := make([]StudyResult, 0, len(cellSlots))
		for _, slot := range cellSlots {
			tradingDays[slot.Day] = true
			observation, exists := observations[slot.ID]
			if !exists {
				rows = append(rows, StudyResult{Slot: slot, Outcome: "missing"})
				continue
			}
			row, err := compareStudyObservation(plan, cell, slot, observation)
			if err != nil {
				return StudyReport{}, fmt.Errorf("slot %q: %w", slot.ID, err)
			}
			if row.QuoteID != "" {
				key := [2]string{cell.RouteID, row.QuoteID}
				if seenQuotes[key] {
					return StudyReport{}, fmt.Errorf("duplicate quote ID for route %q", cell.RouteID)
				}
				seenQuotes[key] = true
			}
			rows = append(rows, row)
			delete(observations, slot.ID)
		}
		summary := summarizeStudyCell(plan, cell, rows)
		report.CoverageComplete = report.CoverageComplete && summary.Missing == 0
		report.Cells = append(report.Cells, summary)
	}
	if len(observations) > 0 {
		return StudyReport{}, errors.New("observations contain an unregistered slot")
	}
	report.DeclaredDays = len(tradingDays)
	return report, nil
}

func pinnedStudyInput(raw []byte, expectedSHA string, limit int) bool {
	if len(raw) == 0 || len(raw) > limit || !utf8.Valid(raw) {
		return false
	}
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:]) == expectedSHA
}

func studySlots(plan StudyPlan) (map[string][]StudySlot, error) {
	cellIDs, dimensions := make(map[string]bool), make(map[StudyCell]bool)
	for _, cell := range plan.Cells {
		if cellIDs[cell.ID] || cell.Wrapper.Chain != cell.Settlement.Chain ||
			cell.Wrapper.key() == cell.Settlement.key() || decimal(cell.ReferenceUSD).Sign() <= 0 {
			return nil, errors.New("invalid or duplicate quote study cell")
		}
		cellIDs[cell.ID] = true
		cell.ID, cell.ReferenceUSD = "", exactDecimal(decimal(cell.ReferenceUSD))
		if dimensions[cell] {
			return nil, errors.New("duplicate quote study cell dimensions")
		}
		dimensions[cell] = true
	}
	slots := make(map[string][]StudySlot, len(plan.Cells))
	slotIDs := make(map[string]bool, len(plan.Slots))
	type cellTime struct {
		cell string
		at   time.Time
	}
	cellTimes := make(map[cellTime]bool, len(plan.Slots))
	for _, slot := range plan.Slots {
		key := cellTime{slot.CellID, slot.ScheduledAt}
		if slotIDs[slot.ID] || cellTimes[key] || !cellIDs[slot.CellID] ||
			slot.ScheduledAt.IsZero() || slot.Day != slot.ScheduledAt.Format(time.DateOnly) {
			return nil, errors.New("invalid or duplicate quote study slot")
		}
		slots[slot.CellID] = append(slots[slot.CellID], slot)
		slotIDs[slot.ID], cellTimes[key] = true, true
	}
	if len(slots) != len(plan.Cells) {
		return nil, errors.New("every declared study cell needs at least one slot")
	}
	for _, ordered := range slots {
		slices.SortFunc(ordered, func(a, b StudySlot) int {
			if order := a.ScheduledAt.Compare(b.ScheduledAt); order != 0 {
				return order
			}
			return cmp.Compare(a.ID, b.ID)
		})
	}
	return slots, nil
}

func compareStudyObservation(plan StudyPlan, cell StudyCell, slot StudySlot, observation StudyObservation) (StudyResult, error) {
	deadline := slot.ScheduledAt.Add(time.Duration(plan.RequestWindowMS) * time.Millisecond)
	if observation.RequestedAt.Before(slot.ScheduledAt) || !observation.RequestedAt.Before(deadline) ||
		observation.ReceivedAt.Before(observation.RequestedAt) {
		return StudyResult{}, errors.New("request or response is outside the study timing limits")
	}
	seconds := observation.ReceivedAt.Unix() - observation.RequestedAt.Unix()
	nanos := observation.ReceivedAt.Nanosecond() - observation.RequestedAt.Nanosecond()
	latency := big.NewRat(seconds*1000, 1)
	latency.Add(latency, big.NewRat(int64(nanos), int64(time.Millisecond)))
	row := StudyResult{
		Slot: slot, Outcome: observation.Status, RequestedAt: &observation.RequestedAt,
		ReceivedAt: &observation.ReceivedAt, LatencyMS: latency.FloatString(6),
	}
	if observation.Status != "comparison" {
		return row, nil
	}
	doc, err := parseDocument(observation.Comparison)
	if err != nil {
		return StudyResult{}, err
	}
	q := doc.Quotes[0]
	notional := new(big.Rat).Mul(decimal(doc.Reference.USDPerShare), decimal(doc.Market.TargetShares))
	if !doc.AsOf.Equal(observation.ReceivedAt) || doc.Market.UnderlyingID != cell.UnderlyingID ||
		doc.Market.Side != cell.Side || doc.Market.Chain != cell.Wrapper.Chain ||
		doc.Market.MaxAgeSeconds != plan.MaxAgeSeconds ||
		q.Wrapper != cell.Wrapper || q.Settlement != cell.Settlement ||
		notional.Cmp(decimal(cell.ReferenceUSD)) != 0 ||
		q.ObservedAt.Before(observation.RequestedAt) || q.ObservedAt.After(observation.ReceivedAt) {
		return StudyResult{}, errors.New("quote comparison disagrees with its registered cell or observation")
	}
	row.QuoteID, row.QuoteMode, row.ObservedAt, row.ExpiresAt = q.ID, q.Mode, &q.ObservedAt, &q.ExpiresAt
	if !fresh(doc.Reference.ObservedAt, doc.Reference.ExpiresAt, doc.AsOf, time.Duration(plan.MaxAgeSeconds)*time.Second) {
		row.Outcome, row.Exclusion = "comparison_excluded", "reference_stale"
		return row, nil
	}
	evaluator, err := newEvaluator(doc)
	if err != nil {
		return StudyResult{}, err
	}
	revision := activeRevision(evaluator.byMint[q.Wrapper.key()], q.ObservedAt)
	if revision.Revision != "" && revision.IssuerID != cell.IssuerID {
		return StudyResult{}, errors.New("quote issuer differs from its registered cell")
	}
	quote, _ := evaluator.compare(q)
	row.Quote = &quote
	if quote.Reason != "" {
		row.Outcome, row.Exclusion = "comparison_excluded", quote.Reason
		if quote.Reason == "quote_stale" && !doc.AsOf.Before(q.ExpiresAt) {
			row.Exclusion = "quote_expired"
		}
		return row, nil
	}
	row.Outcome, quote.Rank = "priced", 1
	return row, nil
}

func summarizeStudyCell(plan StudyPlan, cell StudyCell, rows []StudyResult) StudyCellReport {
	result := StudyCellReport{
		Cell: cell, Expected: len(rows), Slots: rows,
		Outcomes: make(map[string]int), Exclusions: make(map[string]int),
	}
	var costs, latency []*big.Rat
	plannedDays, observedDays, pricedDays := make(map[string]bool), make(map[string]bool), make(map[string]bool)
	for _, row := range rows {
		result.Outcomes[row.Outcome]++
		plannedDays[row.Slot.Day] = true
		if row.Outcome == "missing" {
			result.Missing++
			continue
		}
		latency = append(latency, decimal(row.LatencyMS))
		observedDays[row.Slot.Day] = true
		if row.Exclusion != "" {
			result.Exclusions[row.Exclusion]++
		}
		if row.Outcome != "priced" {
			continue
		}
		// Recover exact shortfall from exact totals. Rounded display costs
		// must not be used to select a percentile.
		referenceUSD := decimal(cell.ReferenceUSD)
		cost := new(big.Rat).Sub(decimal(row.Quote.EffectiveUSD), referenceUSD)
		if cell.Side == "SELL" {
			cost.Neg(cost)
		}
		cost.Quo(cost, referenceUSD).Mul(cost, big.NewRat(10000, 1))
		costs = append(costs, cost)
		pricedDays[row.Slot.Day] = true
	}
	result.Observed, result.Priced = len(latency), len(costs)
	result.DeclaredDays, result.ObservedDays, result.PricedDays = len(plannedDays), len(observedDays), len(pricedDays)
	result.UnsuccessfulObserved = result.Observed - result.Priced
	if result.Observed > 0 {
		rate := big.NewRat(int64(result.UnsuccessfulObserved)*10000, int64(result.Observed)).FloatString(8)
		result.UnsuccessfulObservedRateBPS = &rate
	}
	result.CostBPS = describeStudyQuantiles(costs, plan.MinPricedSamples, result.PricedDays, plan.MinPricedDays, 8)
	result.LatencyMS = describeStudyQuantiles(latency, plan.MinPricedSamples, result.ObservedDays, plan.MinPricedDays, 6)
	return result
}

func describeStudyQuantiles(samples []*big.Rat, minSamples, days, minDays, digits int) StudyQuantiles {
	result := StudyQuantiles{N: len(samples), Basis: "nearest_rank"}
	if len(samples) < minSamples {
		result.Basis = "insufficient_samples"
		return result
	}
	if days < minDays {
		result.Basis = "insufficient_trading_days"
		return result
	}
	slices.SortFunc(samples, (*big.Rat).Cmp)
	nearest := func(percent, resolutionN int) *string {
		if len(samples) < resolutionN {
			return nil
		}
		rank := (len(samples)*percent + 99) / 100
		value := samples[rank-1].FloatString(digits)
		return &value
	}
	result.P50, result.P95, result.P99 = nearest(50, 2), nearest(95, 20), nearest(99, 100)
	return result
}
