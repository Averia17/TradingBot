package analytics

import (
	"errors"
	"math/big"
	"time"
)

func validPeriod(period string) bool { return period == "7d" || period == "30d" || period == "all" }

func pendingPerformance(period, benchmarkID string) Performance {
	benchmarkStatus := "missing"
	if benchmarkID == "" {
		benchmarkStatus = "not_configured"
	}
	return Performance{Period: period, Status: "insufficient_history", BenchmarkID: benchmarkID,
		BenchmarkStatus: benchmarkStatus, OperatingStatus: "coverage_missing"}
}

type performancePoint struct {
	value    valuedState
	realized *big.Rat
	fees     *big.Rat
}

func calculatePerformance(in reportInput, policy Policy, index valuationIndex, period string) (Performance, error) {
	out := pendingPerformance(period, policy.BenchmarkID)
	end := utcDay(in.now).Add(-time.Nanosecond)
	start := in.state.FundedAt
	days := map[string]int{"7d": 7, "30d": 30}[period]
	if days != 0 {
		start = maxTime(start, end.AddDate(0, 0, -days))
	}
	if !end.After(start) {
		return out, nil
	}
	out.StartAt, out.EndAt = &start, &end
	if end.Sub(start) > 3660*24*time.Hour {
		out.Status = "incomplete"
		return out, nil
	}
	first, last, drawdown, observations, expected, err := scanPerformance(in.events, index, policy.MarkMaxAge, start, end)
	if err != nil {
		return Performance{}, err
	}
	out.Observations = observations
	out.RealizedPnLUSD = ratText(new(big.Rat).Sub(last.realized, first.realized))
	out.ExecutionFeesUSD = ratText(new(big.Rat).Sub(last.fees, first.fees))
	out.Status = "incomplete"
	if first.value.nav == nil || last.value.nav == nil {
		return out, nil
	}
	pnl := new(big.Rat).Sub(last.value.nav, first.value.nav)
	out.PnLUSD = ratText(pnl)
	out.UnrealizedChangeUSD = ratText(new(big.Rat).Sub(last.value.unrealized, first.value.unrealized))

	base := first.value.nav
	var returnPct *big.Rat
	if base.Sign() > 0 {
		returnPct = percent(pnl, base)
		out.ReturnPct = ratText(returnPct)
		if observations >= 2 && observations == expected {
			out.Status = "complete"
			out.MaxDrawdownPct = ratText(drawdown)
		}
	}
	benchmarkReturn := benchmarkPerformance(&out, index, policy, start, end)
	if benchmarkReturn != nil && returnPct != nil {
		out.ExcessPctPoints = ratText(new(big.Rat).Sub(returnPct, benchmarkReturn))
	}
	if costs := costsFor(index, start, end, in.now); costs != nil {
		out.OperatingStatus = "complete"
		out.OperatingCostsUSD = ratText(costs)
		net := new(big.Rat).Sub(pnl, costs)
		out.OperatingPnLUSD = ratText(net)
		if base.Sign() > 0 {
			operatingReturn := percent(net, base)
			out.OperatingReturnPct = ratText(operatingReturn)
			if benchmarkReturn != nil {
				out.OperatingExcessPctPoints = ratText(operatingReturn.Sub(operatingReturn, benchmarkReturn))
			}
		}
	}
	return out, nil
}

func scanPerformance(events []AccountEvent, index valuationIndex, maxAge time.Duration, start, end time.Time) (
	first, last performancePoint, drawdown *big.Rat, observations, expected int, err error,
) {
	state, cursor := newState(), 0
	drawdown = new(big.Rat)
	var peak *big.Rat
	for cutoff := start; !cutoff.After(end); cutoff = nextDayEnd(cutoff) {
		for cursor < len(events) && !events[cursor].RecordedAt.After(cutoff) {
			event := events[cursor]
			if err := state.apply(event); err != nil {
				return first, last, nil, 0, 0, err
			}
			cursor++
			// The funding instant is the first baseline, even if the source
			// clock gave some of the following versions the same timestamp.
			if cutoff.Equal(start) && event.AccountVersion == 1 && start.Equal(state.FundedAt) {
				break
			}
		}
		if state.Version == 0 {
			return first, last, nil, 0, 0, errors.New("missing funding at performance cutoff")
		}
		last = performancePoint{value: evaluate(state, index, cutoff, maxAge, false),
			realized: dollars(state.RealizedUSD), fees: dollars(state.FeesUSD)}
		if expected == 0 {
			first = last
		}
		expected++
		if nav := last.value.nav; nav != nil {
			observations++
			if peak == nil || nav.Cmp(peak) > 0 {
				peak = nav
			}
			if peak.Sign() > 0 {
				fall := percent(new(big.Rat).Sub(peak, nav), peak)
				if fall.Cmp(drawdown) > 0 {
					drawdown = fall
				}
			}
		}
	}
	return first, last, drawdown, observations, expected, nil
}

func benchmarkPerformance(out *Performance, index valuationIndex, policy Policy, start, end time.Time) *big.Rat {
	if policy.BenchmarkID == "" {
		return nil
	}
	first, last := latestAt(index.benchmarks, start), latestAt(index.benchmarks, end)
	if first == nil || last == nil || start.Sub(first.ObservedAt) > policy.MarkMaxAge || end.Sub(last.ObservedAt) > policy.MarkMaxAge ||
		first.SourceID != last.SourceID || first.SourceKind != last.SourceKind {
		return nil
	}
	base, close := dollars(first.Benchmark.Level), dollars(last.Benchmark.Level)
	change := percent(close.Sub(close, base), base)
	out.BenchmarkStatus, out.BenchmarkReturnPct = "complete", ratText(change)
	return change
}

func costsFor(index valuationIndex, start, end, knownAt time.Time) *big.Rat {
	byDay := make(map[string]map[string]string)
	day := utcDay(start)
	if atDayEnd(start) {
		day = day.AddDate(0, 0, 1)
	}
	for ; !day.After(end); day = day.AddDate(0, 0, 1) {
		label := day.Format(time.DateOnly)
		for _, row := range index.coverage {
			if !row.AvailableAt.After(knownAt) && row.Coverage.FirstDay <= label && label <= row.Coverage.LastDay {
				byDay[label] = row.Coverage.Categories
				break
			}
		}
		if len(byDay[label]) == 0 {
			return nil
		}
		for _, status := range byDay[label] {
			if status != "complete" && status != "zero" {
				return nil
			}
		}
	}
	costs := new(big.Rat)
	for _, row := range index.costs {
		if row.ObservedAt.After(start) && !row.ObservedAt.After(end) {
			day := row.ObservedAt.UTC().Format(time.DateOnly)
			if row.AvailableAt.After(knownAt) || byDay[day][row.Cost.Category] != "complete" {
				return nil
			}
			costs.Add(costs, dollars(row.Cost.AmountUSD))
		}
	}
	return costs
}

func utcDay(at time.Time) time.Time {
	year, month, day := at.UTC().Date()
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
}

func atDayEnd(at time.Time) bool { return at.Equal(utcDay(at).AddDate(0, 0, 1).Add(-time.Nanosecond)) }

func nextDayEnd(at time.Time) time.Time {
	if atDayEnd(at) {
		at = at.Add(time.Nanosecond)
	}
	return utcDay(at).AddDate(0, 0, 1).Add(-time.Nanosecond)
}

func maxTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return b
	}
	return a
}
