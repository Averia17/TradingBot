package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func validateRiskRefusalReport(t *testing.T, data []byte) {
	t.Helper()
	const schemaID = "https://tradingbot.local/contracts/paper-risk-refusals.schema.json"
	schemaFile := filepath.Join("..", "..", "..", "..", "contracts", "paper-risk-refusals.schema.json")
	raw, err := os.ReadFile(schemaFile)
	if err != nil {
		t.Fatal(err)
	}
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	compiler.AssertFormat()
	if err := compiler.AddResource(schemaID, document); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile(schemaID)
	if err != nil {
		t.Fatal(err)
	}
	output, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(output); err != nil {
		t.Fatal(err)
	}
}

func TestPaperRiskRefusalExampleMatchesSchema(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "contracts", "valid-paper-risk-refusals.json"))
	if err != nil {
		t.Fatal(err)
	}
	validateRiskRefusalReport(t, raw)
}
