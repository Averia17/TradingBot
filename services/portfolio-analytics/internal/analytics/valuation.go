package analytics

import (
	"cmp"
	_ "embed"
	"errors"
	"slices"
	"sort"
	"time"
)

//go:embed paper-valuation-event.schema.json
var valuationSchemaJSON []byte

var valuationSchema = compileSchema("paper-valuation-event", valuationSchemaJSON)

type Valuation struct {
	Version     string     `json:"version"`
	AccountID   string     `json:"account_id"`
	ID          string     `json:"id"`
	Kind        string     `json:"kind"`
	SourceID    string     `json:"source_id"`
	SourceKind  string     `json:"source_kind"`
	ObservedAt  time.Time  `json:"observed_at"`
	AvailableAt time.Time  `json:"available_at"`
	Mark        *Mark      `json:"mark,omitempty"`
	Benchmark   *Benchmark `json:"benchmark,omitempty"`
	Cost        *Cost      `json:"operating_cost,omitempty"`
	Coverage    *Coverage  `json:"cost_coverage,omitempty"`
}

type Mark struct {
	Instrument
	PriceUSD string `json:"price_usd"`
}

type Benchmark struct {
	SeriesID string `json:"series_id"`
	Level    string `json:"level"`
}

type Cost struct {
	Category  string `json:"category"`
	AmountUSD string `json:"amount_usd"`
}

type Coverage struct {
	FirstDay   string            `json:"first_day"`
	LastDay    string            `json:"last_day"`
	Categories map[string]string `json:"categories"`
}

func DecodeValuation(data []byte) (Valuation, error) {
	var row Valuation
	if err := decodeRecord(data, valuationSchema, &row); err != nil {
		return row, err
	}
	if row.ObservedAt.IsZero() || row.AvailableAt.Before(row.ObservedAt) {
		return row, errors.New("invalid observation and availability order")
	}
	if c := row.Coverage; c != nil {
		first, firstErr := time.Parse(time.DateOnly, c.FirstDay)
		last, lastErr := time.Parse(time.DateOnly, c.LastDay)
		if firstErr != nil || lastErr != nil || last.Before(first) || row.ObservedAt.Before(last.AddDate(0, 0, 1)) {
			return row, errors.New("cost coverage requires closed UTC dates")
		}
	}
	return row, nil
}

type valuationIndex struct {
	marks     map[Instrument]valuationTimeline
	benchmark valuationTimeline
	costs     []Valuation
	coverage  []Valuation
}

func indexValuations(rows []Valuation, benchmarkID string) valuationIndex {
	index := valuationIndex{marks: make(map[Instrument]valuationTimeline)}
	for _, row := range rows {
		switch row.Kind {
		case "mark":
			key := row.Mark.Instrument
			index.marks[key] = append(index.marks[key], row)
		case "benchmark":
			if row.Benchmark.SeriesID == benchmarkID {
				index.benchmark = append(index.benchmark, row)
			}
		case "operating_cost":
			index.costs = append(index.costs, row)
		case "cost_coverage":
			index.coverage = append(index.coverage, row)
		}
	}
	for instrument, rows := range index.marks {
		index.marks[instrument] = buildValuationTimeline(rows)
	}
	index.benchmark = buildValuationTimeline(index.benchmark)
	slices.SortFunc(index.coverage, compareValuationPriority)
	return index
}

func compareValuationPriority(a, b Valuation) int {
	if byObservation := b.ObservedAt.Compare(a.ObservedAt); byObservation != 0 {
		return byObservation
	}
	if byAvailability := b.AvailableAt.Compare(a.AvailableAt); byAvailability != 0 {
		return byAvailability
	}
	return cmp.Compare(a.ID, b.ID)
}

func (v Valuation) visibleAt() time.Time { return maxTime(v.ObservedAt, v.AvailableAt) }

// A timeline contains only observations that become the best visible mark.
// Its rows are ordered by when the information becomes usable.
type valuationTimeline []Valuation

func buildValuationTimeline(rows []Valuation) valuationTimeline {
	slices.SortFunc(rows, func(a, b Valuation) int {
		if byVisibility := a.visibleAt().Compare(b.visibleAt()); byVisibility != 0 {
			return byVisibility
		}
		return compareValuationPriority(a, b)
	})
	timeline := rows[:0]
	for _, row := range rows {
		if len(timeline) == 0 || compareValuationPriority(row, timeline[len(timeline)-1]) < 0 {
			timeline = append(timeline, row)
		}
	}
	clear(rows[len(timeline):])
	return timeline
}

func (t valuationTimeline) latestVisibleAt(cutoff time.Time) *Valuation {
	next := sort.Search(len(t), func(i int) bool { return t[i].visibleAt().After(cutoff) })
	if next == 0 {
		return nil
	}
	return &t[next-1]
}
