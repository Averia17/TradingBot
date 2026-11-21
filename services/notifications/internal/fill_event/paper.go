package fill_event

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"time"
	"unicode/utf8"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// PaperTopic carries authoritative virtual ledger fills under the 1.1 contract.
const PaperTopic = "tradingbot.trade.filled.paper.v1"

//go:embed paper-fill-event.schema.json
var paperSchemaJSON []byte

var paperSchema = compilePaperSchema()

type PaperEvent struct {
	Version         string    `json:"version"`
	ID              string    `json:"id"`
	Mode            string    `json:"mode"`
	Source          string    `json:"source"`
	AccountID       string    `json:"account_id"`
	OrderID         string    `json:"order_id"`
	DecisionID      string    `json:"decision_id"`
	QuoteID         string    `json:"quote_id"`
	ResearchJobID   string    `json:"research_job_id,omitempty"`
	FillEventID     string    `json:"fill_event_id"`
	InstrumentID    string    `json:"instrument_id"`
	RegistryVersion string    `json:"registry_version"`
	Decimals        int       `json:"decimals"`
	Side            string    `json:"side"`
	QuantityRaw     string    `json:"quantity_raw"`
	GrossUSD        string    `json:"gross_usd"`
	FeeUSD          string    `json:"fee_usd"`
	Currency        string    `json:"currency"`
	FilledAt        time.Time `json:"filled_at"`
	RecordedAt      time.Time `json:"recorded_at"`
}

func compilePaperSchema() *jsonschema.Schema {
	const name = "https://tradingbot.local/paper-fill-event.schema.json"
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(paperSchemaJSON))
	if err != nil {
		panic(err)
	}
	compiler := jsonschema.NewCompiler()
	compiler.AssertFormat()
	if err := compiler.AddResource(name, value); err != nil {
		panic(err)
	}
	schema, err := compiler.Compile(name)
	if err != nil {
		panic(err)
	}
	return schema
}

// DecodePaper accepts exactly one committed paper-fill object.
func DecodePaper(data []byte) (PaperEvent, error) {
	if len(data) > maxSize || !utf8.Valid(data) {
		return PaperEvent{}, errInvalidJSON
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return PaperEvent{}, errInvalidJSON
	}
	fields := make(map[string]any, 20)
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok {
			return PaperEvent{}, errInvalidJSON
		}
		if _, duplicate := fields[key]; duplicate {
			return PaperEvent{}, errInvalidJSON
		}
		var value any
		valueStart := decoder.InputOffset()
		if err := decoder.Decode(&value); err != nil {
			return PaperEvent{}, errInvalidJSON
		}
		// Match time.Time's wire decoder, which requires an unescaped timestamp.
		if (key == "filled_at" || key == "recorded_at") &&
			bytes.IndexByte(data[valueStart:decoder.InputOffset()], '\\') >= 0 {
			return PaperEvent{}, errInvalidJSON
		}
		switch value := value.(type) {
		case string:
		case json.Number:
			if key != "decimals" || len(value.String()) > 2 {
				return PaperEvent{}, errInvalidJSON
			}
		default:
			return PaperEvent{}, errInvalidJSON
		}
		fields[key] = value
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return PaperEvent{}, errInvalidJSON
	}
	if _, err := decoder.Token(); err != io.EOF {
		return PaperEvent{}, errInvalidJSON
	}
	if err := paperSchema.Validate(fields); err != nil {
		return PaperEvent{}, errors.New("invalid paper fill schema")
	}
	event, err := paperFillFromFields(fields)
	if err != nil {
		return PaperEvent{}, errInvalidJSON
	}
	if event.ID != "paper:"+event.AccountID+":"+event.FillEventID || event.FillEventID == event.AccountID ||
		event.FillEventID == event.OrderID || event.RecordedAt.Before(event.FilledAt) {
		return PaperEvent{}, errors.New("invalid paper fill identity or recording time")
	}
	return event, nil
}

func paperFillFromFields(fields map[string]any) (PaperEvent, error) {
	// The schema has already checked all required fields and exact string amounts.
	stringField := func(name string) string {
		value, _ := fields[name].(string)
		return value
	}
	precision, _ := fields["decimals"].(json.Number)
	decimals, err := precision.Int64()
	if err != nil {
		return PaperEvent{}, err
	}
	filledAt, err := time.Parse(time.RFC3339Nano, stringField("filled_at"))
	if err != nil {
		return PaperEvent{}, err
	}
	recordedAt, err := time.Parse(time.RFC3339Nano, stringField("recorded_at"))
	if err != nil {
		return PaperEvent{}, err
	}
	return PaperEvent{
		Version:         stringField("version"),
		ID:              stringField("id"),
		Mode:            stringField("mode"),
		Source:          stringField("source"),
		AccountID:       stringField("account_id"),
		OrderID:         stringField("order_id"),
		DecisionID:      stringField("decision_id"),
		QuoteID:         stringField("quote_id"),
		ResearchJobID:   stringField("research_job_id"),
		FillEventID:     stringField("fill_event_id"),
		InstrumentID:    stringField("instrument_id"),
		RegistryVersion: stringField("registry_version"),
		Decimals:        int(decimals),
		Side:            stringField("side"),
		QuantityRaw:     stringField("quantity_raw"),
		GrossUSD:        stringField("gross_usd"),
		FeeUSD:          stringField("fee_usd"),
		Currency:        stringField("currency"),
		FilledAt:        filledAt,
		RecordedAt:      recordedAt,
	}, nil
}
