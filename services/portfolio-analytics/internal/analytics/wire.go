package analytics

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"time"
	"unicode/utf8"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

const (
	WireVersion    = "1.0"
	AccountTopic   = "tradingbot.portfolio.account.paper.v1"
	HeadTopic      = "tradingbot.portfolio.account-head.paper.v1"
	ValuationTopic = "tradingbot.portfolio.valuation.paper.v1"
	maxRecordBytes = 8 * 1024
)

//go:embed paper-account-event.schema.json
var accountSchemaJSON []byte

//go:embed paper-account-head.schema.json
var headSchemaJSON []byte

var (
	accountSchema    = compileSchema("paper-account-event", accountSchemaJSON)
	headSchema       = compileSchema("paper-account-head", headSchemaJSON)
	accountIDPattern = regexp.MustCompile("^[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$")
)

type Instrument struct {
	InstrumentID    string `json:"instrument_id"`
	RegistryVersion string `json:"registry_version"`
	Decimals        int    `json:"decimals"`
}

type Balance struct {
	Instrument
	QuantityRaw         string `json:"quantity_raw"`
	ReservedQuantityRaw string `json:"reserved_quantity_raw"`
}

type Fill struct {
	Side          string    `json:"side"`
	QuantityRaw   string    `json:"quantity_raw"`
	GrossUSD      string    `json:"gross_usd"`
	FeeUSD        string    `json:"fee_usd"`
	ObservedAt    time.Time `json:"observed_at"`
	OrderID       string    `json:"order_id"`
	DecisionID    string    `json:"decision_id"`
	QuoteID       string    `json:"quote_id"`
	ResearchJobID string    `json:"research_job_id,omitempty"`
}

type AccountEvent struct {
	Version         string    `json:"version"`
	AccountID       string    `json:"account_id"`
	EventID         string    `json:"event_id"`
	AccountVersion  int64     `json:"account_version"`
	Kind            string    `json:"kind"`
	RecordedAt      time.Time `json:"recorded_at"`
	CashUSD         string    `json:"cash_usd"`
	ReservedCashUSD string    `json:"reserved_cash_usd"`
	Position        *Balance  `json:"position,omitempty"`
	Fill            *Fill     `json:"fill,omitempty"`
}

type AccountHead struct {
	Version        string    `json:"version"`
	AccountID      string    `json:"account_id"`
	AccountVersion int64     `json:"account_version"`
	ObservedAt     time.Time `json:"observed_at"`
}

func ValidAccountID(value string) bool { return accountIDPattern.MatchString(value) }

func DecodeAccount(data []byte) (AccountEvent, error) {
	var event AccountEvent
	if err := decodeRecord(data, accountSchema, &event); err != nil {
		return event, err
	}
	if event.RecordedAt.IsZero() || usdAmount(event.ReservedCashUSD).Cmp(usdAmount(event.CashUSD)) > 0 {
		return event, errors.New("invalid account event time or cash reserve")
	}
	if event.Position != nil && rawAmount(event.Position.ReservedQuantityRaw).Cmp(rawAmount(event.Position.QuantityRaw)) > 0 {
		return event, errors.New("invalid position reserve")
	}
	if event.Kind == "paper_account.created" && (event.EventID != event.AccountID || usdAmount(event.ReservedCashUSD).Sign() != 0) {
		return event, errors.New("invalid opening account event")
	}
	if f := event.Fill; f != nil {
		if f.ObservedAt.IsZero() || f.ObservedAt.After(event.RecordedAt) || f.OrderID == event.EventID ||
			(f.Side == "SELL" && usdAmount(f.FeeUSD).Cmp(usdAmount(f.GrossUSD)) > 0) {
			return event, errors.New("invalid fill identity, time or proceeds")
		}
	}
	return event, nil
}

func DecodeHead(data []byte) (AccountHead, error) {
	var head AccountHead
	if err := decodeRecord(data, headSchema, &head); err != nil {
		return head, err
	}
	if head.ObservedAt.IsZero() {
		return head, errors.New("invalid account head time")
	}
	return head, nil
}

func compileSchema(name string, input []byte) *jsonschema.Schema {
	uri := "https://tradingbot.local/contracts/" + name + ".schema.json"
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(input))
	if err != nil {
		panic(err)
	}
	compiler := jsonschema.NewCompiler()
	compiler.AssertFormat()
	if err := compiler.AddResource(uri, value); err != nil {
		panic(err)
	}
	schema, err := compiler.Compile(uri)
	if err != nil {
		panic(err)
	}
	return schema
}

func decodeRecord(data []byte, schema *jsonschema.Schema, dst any) error {
	if len(data) > maxRecordBytes || !utf8.Valid(data) {
		return errors.New("invalid JSON record size or encoding")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	src, err := readObject(decoder, 0)
	if err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("unexpected data after JSON object")
	}
	if err := schema.Validate(src); err != nil {
		// Validation errors can include the original input, which is untrusted.
		return errors.New("record failed wire schema validation")
	}
	if err := json.Unmarshal(data, dst); err != nil {
		return errors.New("record could not be decoded")
	}
	return nil
}

// The record contracts contain objects and scalars. encoding/json by itself
// accepts a repeated key, which would make an event's identity ambiguous.
func readObject(decoder *json.Decoder, depth int) (map[string]any, error) {
	if depth > 4 {
		return nil, errors.New("record is nested too deeply")
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return nil, errors.New("expected a JSON object")
	}
	fields := make(map[string]any)
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok {
			return nil, errors.New("invalid JSON key")
		}
		if _, exists := fields[key]; exists {
			return nil, errors.New("duplicate JSON key")
		}
		var data json.RawMessage
		if err := decoder.Decode(&data); err != nil {
			return nil, errors.New("invalid JSON value")
		}
		nested := json.NewDecoder(bytes.NewReader(data))
		nested.UseNumber()
		if len(data) > 0 && data[0] == '{' {
			value, err := readObject(nested, depth+1)
			if err != nil {
				return nil, err
			}
			fields[key] = value
		} else {
			value, err := nested.Token()
			if err != nil {
				return nil, errors.New("invalid JSON scalar")
			}
			if _, ok := value.(json.Delim); ok {
				return nil, errors.New("unexpected JSON delimiter")
			}
			fields[key] = value
		}
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return nil, errors.New("invalid end of JSON object")
	}
	return fields, nil
}
