package outbox

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestPaperAccountWireContract(t *testing.T) {
	published, err := os.ReadFile("../../../../contracts/paper-account-event.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	normalize := func(raw []byte) []byte { return bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n")) }
	if !bytes.Equal(normalize(published), normalize(paperAccountSchemaJSON)) {
		t.Fatal("embedded paper account schema differs from published schema")
	}
	event, err := os.ReadFile("../../../../contracts/valid-paper-account-event.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodePaperAccountEvent(event); err != nil {
		t.Fatal(err)
	}
	invalid := bytes.Replace(event, []byte(`"cash_usd": "1000"`), []byte(`"cash_usd": 1000`), 1)
	if _, err := decodePaperAccountEvent(invalid); err == nil {
		t.Fatal("accepted JSON number for an exact money field")
	}
}

func TestPaperAccountHeadMatchesPublishedContract(t *testing.T) {
	const url = "https://tradingbot.local/contracts/paper-account-head.schema.json"
	schema, err := os.ReadFile("../../../../contracts/paper-account-head.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(schema))
	if err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	compiler.AssertFormat()
	if err := compiler.AddResource(url, value); err != nil {
		t.Fatal(err)
	}
	headSchema, err := compiler.Compile(url)
	if err != nil {
		t.Fatal(err)
	}
	head := paperAccountHead{
		Version: "1.0", AccountID: "11111111-1111-4111-8111-111111111111", AccountVersion: 1,
		ObservedAt: time.Date(2026, 11, 18, 9, 0, 0, 123456000, time.UTC),
	}
	payload, err := json.Marshal(head)
	if err != nil {
		t.Fatal(err)
	}
	value, err = jsonschema.UnmarshalJSON(bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	if err := headSchema.Validate(value); err != nil {
		t.Fatal(err)
	}
}
