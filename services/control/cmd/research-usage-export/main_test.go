package main

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"tradingbot/control/internal/jobs"
)

func TestPermitArchiveLabelsPreparedProviderTruthfully(t *testing.T) {
	schema, err := os.ReadFile("../../../../contracts/research-permit-archive.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	const schemaURI = "https://tradingbot.local/contracts/research-permit-archive.schema.json"
	compiler := jsonschema.NewCompiler()
	definition, err := jsonschema.UnmarshalJSON(bytes.NewReader(schema))
	if err != nil {
		t.Fatal(err)
	}
	if err := compiler.AddResource(schemaURI, definition); err != nil {
		t.Fatal(err)
	}
	check, err := compiler.Compile(schemaURI)
	if err != nil {
		t.Fatal(err)
	}
	for mode, source := range map[string]string{
		"offline": "synthetic_fixture", jobs.ProviderAnalysisMode: "provider_reported",
	} {
		archive := projectArchive(jobs.ResearchCallUsage{
			AnalysisMode: mode, JobID: "00000000-0000-0000-0000-000000000001",
			Status: "queued", PermitHistoryKnown: true,
		})
		if archive.Source != source {
			t.Fatalf("mode %s: source %q, want %q", mode, archive.Source, source)
		}
		raw, err := json.Marshal(archive)
		if err != nil {
			t.Fatal(err)
		}
		wire, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		if err := check.Validate(wire); err != nil {
			t.Fatal(err)
		}
	}
}
