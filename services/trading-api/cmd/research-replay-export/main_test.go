package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"tradingbot/trading-api/internal/jobs"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "contracts", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func assertExportSchema(t *testing.T, raw []byte) {
	t.Helper()
	compiler := jsonschema.NewCompiler()
	compiler.AssertFormat()
	schema, err := jsonschema.UnmarshalJSON(bytes.NewReader(fixture(t, "research-replay-export.schema.json")))
	if err != nil {
		t.Fatal(err)
	}
	if err := compiler.AddResource("research-replay-export.schema.json", schema); err != nil {
		t.Fatal(err)
	}
	validator, err := compiler.Compile("research-replay-export.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if err := validator.Validate(value); err != nil {
		t.Fatal(err)
	}
}

func TestHistoricalResearchFixtureAndReproducibleExport(t *testing.T) {
	raw := fixture(t, "valid-research-replay-export.json")
	assertExportSchema(t, raw)
	var fixtureExport replayExport
	if err := json.Unmarshal(raw, &fixtureExport); err != nil {
		t.Fatal(err)
	}
	fixtureJob := fixtureExport.Jobs[0]
	input := jobs.CompletedResearch{
		Job: jobs.Job{
			ID: fixtureJob.JobID, Status: "completed", Request: fixtureJob.Request,
			CreatedAt: fixtureJob.CreatedAt,
		},
		Result: jobs.Result{
			JobID: fixtureJob.JobID, Attempt: fixtureJob.Attempt,
			CompletedAt: fixtureJob.CompletedAt, Payload: fixtureJob.Result,
		},
	}
	data, err := buildExport([]jobs.CompletedResearch{input})
	if err != nil {
		t.Fatal(err)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bytes.TrimSpace(data), compact.Bytes()) {
		t.Fatal("fixture does not contain the exact output")
	}

	input.Result.CompletedAt = input.Job.CreatedAt.Add(-time.Microsecond)
	if data, err := buildExport([]jobs.CompletedResearch{input}); err == nil || len(data) != 0 {
		t.Fatal("an impossible receipt was exported")
	}
	input.Result.CompletedAt = fixtureJob.CompletedAt
	input.Result.Attempt++
	if data, err := buildExport([]jobs.CompletedResearch{input}); err == nil || len(data) != 0 {
		t.Fatal("a receipt with mismatched provenance was exported")
	}
	input.Result.Attempt = fixtureJob.Attempt
	input.Result.Payload = bytes.Replace(input.Result.Payload, []byte(`"alpha_measured":false`), []byte(`"alpha_measured":true`), 1)
	if data, err := buildExport([]jobs.CompletedResearch{input}); err == nil || len(data) != 0 {
		t.Fatal("an invalid result source claim was exported")
	}
}

func TestExportRejectsInvalidSelectionsBeforeConnecting(t *testing.T) {
	const id = "f700cba3-c634-4204-8669-1acd78612431"
	tooMany := make([]string, 0, 66)
	for i := range 33 {
		tooMany = append(tooMany, "--job", fmt.Sprintf("f700cba3-c634-4204-8669-%012x", i))
	}
	for _, test := range []struct {
		args []string
		err  string
	}{
		{nil, "provide at least one"},
		{[]string{"--job", "bad"}, "unique lowercase job UUIDs"},
		{[]string{"--job", id, "--job", id}, "unique lowercase job UUIDs"},
		{[]string{"--job", id, "unexpected"}, "provide at least one"},
		{tooMany, "exceeds its job limit"},
	} {
		var out bytes.Buffer
		if err := run(context.Background(), "not a DSN", test.args, &out); err == nil ||
			!strings.Contains(err.Error(), test.err) || out.Len() != 0 {
			t.Fatalf("selection %v returned %q with error %v", test.args, out.Bytes(), err)
		}
	}
}
