package fill_event

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func loadContractFile(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("../../../../contracts/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func eventSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	const schemaURL = "https://tradingbot.local/trade-event.schema.json"
	data := loadContractFile(t, "trade-event.schema.json")
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	compiler.AssertFormat()
	if err := compiler.AddResource(schemaURL, value); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile(schemaURL)
	if err != nil {
		t.Fatal(err)
	}
	return schema
}

func TestTradeEventFixture(t *testing.T) {
	data := loadContractFile(t, "valid-trade-event.json")
	event, err := Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	if event.Quantity != "0.000000000000000001" || event.Price != "139.2500" {
		t.Fatal("decimal strings lost precision or scale")
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	if err := eventSchema(t).Validate(value); err != nil {
		t.Fatal(err)
	}
	roundTrip, err := Decode(encoded)
	if err != nil || roundTrip != event {
		t.Fatalf("round trip = %+v with error %v", roundTrip, err)
	}
}

func TestTradeEventSchemaMatchesDecoder(t *testing.T) {
	schema := eventSchema(t)
	fixture := loadContractFile(t, "valid-trade-event.json")
	tests := []struct {
		name  string
		key   string
		value string
		valid bool
	}{
		{name: "published fixture", valid: true},
		{name: "largest supported decimal", key: "price", value: `"` + strings.Repeat("9", 38) + "." + strings.Repeat("9", 18) + `"`, valid: true},
		{name: "live mode", key: "mode", value: `"live"`, valid: true},
		{name: "exchange fixture source", key: "source", value: `"exchange_fixture"`, valid: true},
		{name: "unknown source", key: "source", value: `"venue"`},
		{name: "explicit empty source", key: "source", value: `""`},
		{name: "null source", key: "source", value: `null`},
		{name: "sell side", key: "side", value: `"SELL"`, valid: true},
		{name: "unsupported version", key: "version", value: `"1.1"`},
		{name: "research mode", key: "mode", value: `"research"`},
		{name: "invalid side", key: "side", value: `"HOLD"`},
		{name: "numeric price", key: "price", value: `139.25`},
		{name: "zero price", key: "price", value: `"0.000"`},
		{name: "negative quantity", key: "quantity", value: `"-1"`},
		{name: "exponent quantity", key: "quantity", value: `"1e-8"`},
		{name: "excess precision", key: "quantity", value: `"0.0000000000000000001"`},
		{name: "excess integer digits", key: "price", value: `"` + strings.Repeat("9", 39) + `"`},
		{name: "leading zero", key: "price", value: `"01"`},
		{name: "invalid identifier", key: "instrument_id", value: `"ABC\n"`},
		{name: "overlong identifier", key: "account_id", value: `"` + strings.Repeat("a", 129) + `"`},
		{name: "invalid currency", key: "currency", value: `"usd"`},
		{name: "invalid date", key: "filled_at", value: `"2026-02-31T00:00:00Z"`},
		{name: "non-UTC offset", key: "filled_at", value: `"2026-11-15T12:00:00+03:00"`},
		{name: "subnanosecond timestamp", key: "filled_at", value: `"2026-11-15T09:00:00.0000000001Z"`},
		{name: "missing field", key: "side"},
		{name: "unknown field", key: "target_chat", value: `"another-recipient"`},
		{name: "null field", key: "id", value: `null`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(fixture, &fields); err != nil {
				t.Fatal(err)
			}
			if tt.value != "" {
				fields[tt.key] = json.RawMessage(tt.value)
			} else if tt.key != "" {
				delete(fields, tt.key)
			}
			data, err := json.Marshal(fields)
			if err != nil {
				t.Fatal(err)
			}
			value, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			if err := schema.Validate(value); (err == nil) != tt.valid {
				t.Errorf("schema error = %v, want valid = %t", err, tt.valid)
			}
			if _, err := Decode(data); (err == nil) != tt.valid {
				t.Errorf("decoder error = %v, want valid = %t", err, tt.valid)
			}
		})
	}
}

func TestTradeEventRejectsAmbiguousWireData(t *testing.T) {
	fixture := loadContractFile(t, "valid-trade-event.json")
	tests := map[string][]byte{
		"duplicate key": bytes.Replace(fixture, []byte(`"mode":`), []byte(`"mode":"live","m\u006fde":`), 1),
		"alias key":     bytes.Replace(fixture, []byte(`"filled_at":`), []byte(`"FILLED_AT":`), 1),
		"trailing JSON": append(bytes.Clone(fixture), []byte(` {}`)...),
		"oversize":      append(bytes.Clone(fixture), bytes.Repeat([]byte(" "), 4096)...),
		"nested value":  bytes.Replace(fixture, []byte(`"USD"`), []byte(`{"iso":"USD"}`), 1),
		"live fixture":  bytes.Replace(fixture, []byte(`"mode": "paper"`), []byte(`"mode":"live","source":"exchange_fixture"`), 1),
	}
	for name, data := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode(data); err == nil {
				t.Fatal("accepted ambiguous wire data")
			}
		})
	}
}

func TestTradeEventValidatesBeforeEncoding(t *testing.T) {
	event, err := Decode(loadContractFile(t, "valid-trade-event.json"))
	if err != nil {
		t.Fatal(err)
	}
	event.FilledAt = event.FilledAt.In(time.FixedZone("east", 3*3600))
	if err := event.Validate(); err == nil {
		t.Fatal("accepted a non-UTC timestamp before encoding")
	}
	event.FilledAt = time.Time{}
	if err := event.Validate(); err == nil {
		t.Fatal("accepted an unset timestamp before encoding")
	}
}
