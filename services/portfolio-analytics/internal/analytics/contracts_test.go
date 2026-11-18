package analytics

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestWireSchemasMatchPublishedContracts(t *testing.T) {
	for name, embedded := range map[string][]byte{
		"paper-account-event.schema.json":   accountSchemaJSON,
		"paper-account-head.schema.json":    headSchemaJSON,
		"paper-valuation-event.schema.json": valuationSchemaJSON,
	} {
		if !bytes.Equal(embedded, contractFile(t, name)) {
			t.Errorf("embedded schema %s differs from the shared contract", name)
		}
	}
	if _, err := DecodeAccount(contractFile(t, "valid-paper-account-event.json")); err != nil {
		t.Errorf("shared account event: %v", err)
	}
	if _, err := DecodeValuation(contractFile(t, "valid-paper-valuation-event.json")); err != nil {
		t.Errorf("shared valuation: %v", err)
	}
}

func TestGeneratedReportMatchesPublishedContract(t *testing.T) {
	schema := compileSchema("paper-portfolio-report", contractFile(t, "paper-portfolio-report.schema.json"))
	for _, current := range []bool{true, false} {
		input := reportFixture(t)
		if !current {
			input.head.AccountVersion++
		}
		report, err := buildReport(input, testPolicy(), "7d")
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(report)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		if err := schema.Validate(decoded); err != nil {
			t.Errorf("report fails schema with current=%v: %v", current, err)
		}
	}
	fixture, err := jsonschema.UnmarshalJSON(bytes.NewReader(contractFile(t, "valid-paper-portfolio-report.json")))
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(fixture); err != nil {
		t.Errorf("shared report fixture: %v", err)
	}
}

func contractFile(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "contracts", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}
