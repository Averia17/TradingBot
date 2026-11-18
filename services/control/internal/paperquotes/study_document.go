package paperquotes

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

const (
	MaxStudyPlanBytes    = 4 << 20
	MaxStudyArchiveBytes = 32 << 20
)

//go:embed study.schema.json
var studySchema []byte

var studyPlanSchema, studyArchiveSchema, studyReportSchema = compileStudySchemas()

func compileStudySchemas() (*jsonschema.Schema, *jsonschema.Schema, *jsonschema.Schema) {
	compiler := jsonschema.NewCompiler()
	compiler.AssertFormat()
	const origin = "https://tradingbot.local/"
	for name, data := range map[string][]byte{
		"paper-quote-study.schema.json":      studySchema,
		"paper-quote-comparison.schema.json": schemaDocument,
	} {
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
		if err != nil {
			panic(err)
		}
		if err := compiler.AddResource(origin+name, doc); err != nil {
			panic(err)
		}
	}
	compile := func(name string) *jsonschema.Schema {
		schema, err := compiler.Compile(origin + "paper-quote-study.schema.json#/$defs/" + name)
		if err != nil {
			panic(err)
		}
		return schema
	}
	return compile("Plan"), compile("Archive"), compile("Report")
}

type StudyPlan struct {
	Version          string      `json:"schema_version"`
	Source           string      `json:"source_mode"`
	ID               string      `json:"study_id"`
	MinPricedSamples int         `json:"min_priced_samples"`
	MinPricedDays    int         `json:"min_priced_trading_days"`
	RequestWindowMS  int         `json:"request_window_ms"`
	MaxAgeSeconds    int         `json:"max_age_seconds"`
	Cells            []StudyCell `json:"cells"`
	Slots            []StudySlot `json:"slots"`
}

type StudyCell struct {
	ID           string `json:"cell_id"`
	RouteID      string `json:"route_id"`
	UnderlyingID string `json:"underlying_id"`
	IssuerID     string `json:"issuer_id"`
	Side         string `json:"side"`
	Session      string `json:"session"`
	ReferenceUSD string `json:"reference_usd"`
	Wrapper      Token  `json:"wrapper"`
	Settlement   Token  `json:"settlement"`
}

type StudySlot struct {
	ID          string    `json:"slot_id"`
	CellID      string    `json:"cell_id"`
	Day         string    `json:"trading_day"`
	ScheduledAt time.Time `json:"scheduled_at"`
}

type StudyArchive struct {
	Version      string             `json:"schema_version"`
	Source       string             `json:"source_mode"`
	StudyID      string             `json:"study_id"`
	PlanSHA256   string             `json:"plan_sha256"`
	Observations []StudyObservation `json:"observations"`
}

type StudyObservation struct {
	SlotID      string          `json:"slot_id"`
	RequestedAt time.Time       `json:"requested_at"`
	ReceivedAt  time.Time       `json:"received_at"`
	Status      string          `json:"status"`
	Comparison  json.RawMessage `json:"comparison,omitempty"`
}

type StudyReport struct {
	Version          string            `json:"schema_version"`
	Source           string            `json:"source_mode"`
	ID               string            `json:"study_id"`
	Scope            string            `json:"scope"`
	ExecutionEnabled bool              `json:"execution_enabled"`
	MarketEvidence   bool              `json:"market_evidence"`
	PlanSHA256       string            `json:"plan_sha256"`
	ArchiveSHA256    string            `json:"observations_sha256"`
	CoverageComplete bool              `json:"declared_coverage_complete"`
	DeclaredDays     int               `json:"declared_trading_days"`
	Cells            []StudyCellReport `json:"cells"`
}

type StudyCellReport struct {
	Cell                        StudyCell      `json:"cell"`
	Expected                    int            `json:"expected"`
	Observed                    int            `json:"observed"`
	Priced                      int            `json:"priced"`
	Missing                     int            `json:"missing"`
	DeclaredDays                int            `json:"declared_trading_days"`
	ObservedDays                int            `json:"observed_trading_days"`
	PricedDays                  int            `json:"priced_trading_days"`
	UnsuccessfulObserved        int            `json:"unsuccessful_observed"`
	UnsuccessfulObservedRateBPS *string        `json:"unsuccessful_observed_rate_bps"`
	Outcomes                    map[string]int `json:"outcomes"`
	Exclusions                  map[string]int `json:"comparison_exclusions"`
	CostBPS                     StudyQuantiles `json:"one_way_quote_cost_bps"`
	LatencyMS                   StudyQuantiles `json:"latency_ms"`
	Slots                       []StudyResult  `json:"slots"`
}

type StudyQuantiles struct {
	N     int     `json:"n"`
	P50   *string `json:"p50"`
	P95   *string `json:"p95"`
	P99   *string `json:"p99"`
	Basis string  `json:"basis"`
}

type StudyResult struct {
	Slot        StudySlot  `json:"slot"`
	Outcome     string     `json:"outcome"`
	RequestedAt *time.Time `json:"requested_at,omitempty"`
	ReceivedAt  *time.Time `json:"received_at,omitempty"`
	LatencyMS   string     `json:"latency_ms,omitempty"`
	QuoteID     string     `json:"quote_id,omitempty"`
	QuoteMode   string     `json:"quote_mode,omitempty"`
	ObservedAt  *time.Time `json:"quote_observed_at,omitempty"`
	ExpiresAt   *time.Time `json:"quote_expires_at,omitempty"`
	Quote       *Row       `json:"quote,omitempty"`
	Exclusion   string     `json:"comparison_exclusion,omitempty"`
}
