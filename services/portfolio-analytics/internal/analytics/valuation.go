package analytics

import (
	_ "embed"
	"errors"
	"slices"
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
	marks      map[Instrument][]Valuation
	benchmarks []Valuation
	costs      []Valuation
	coverage   []Valuation
}

func indexValuations(rows []Valuation, benchmarkID string) valuationIndex {
	index := valuationIndex{marks: make(map[Instrument][]Valuation)}
	for _, row := range rows {
		switch row.Kind {
		case "mark":
			key := row.Mark.Instrument
			index.marks[key] = append(index.marks[key], row)
		case "benchmark":
			if row.Benchmark.SeriesID == benchmarkID {
				index.benchmarks = append(index.benchmarks, row)
			}
		case "operating_cost":
			index.costs = append(index.costs, row)
		case "cost_coverage":
			index.coverage = append(index.coverage, row)
		}
	}
	for _, rows := range index.marks {
		sortValuations(rows)
	}
	sortValuations(index.benchmarks)
	sortValuations(index.coverage)
	return index
}

func sortValuations(rows []Valuation) {
	slices.SortFunc(rows, func(a, b Valuation) int {
		if cmp := b.ObservedAt.Compare(a.ObservedAt); cmp != 0 {
			return cmp
		}
		if cmp := b.AvailableAt.Compare(a.AvailableAt); cmp != 0 {
			return cmp
		}
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
}

func latestAt(rows []Valuation, cutoff time.Time) *Valuation {
	for i := range rows {
		if !rows[i].AvailableAt.After(cutoff) && !rows[i].ObservedAt.After(cutoff) {
			return &rows[i]
		}
	}
	return nil
}
