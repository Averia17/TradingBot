package trading_api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"tradingbot/trading-api/internal/jobs"
)

const schemaRoot = "https://tradingbot.local/contracts/"

func wireSchemaCompiler(t *testing.T, files ...string) *jsonschema.Compiler {
	t.Helper()
	compiler := jsonschema.NewCompiler()
	compiler.AssertFormat()
	for _, name := range files {
		schema, err := jsonschema.UnmarshalJSON(bytes.NewReader(fixture(t, name)))
		if err != nil {
			t.Fatal(err)
		}
		if err := compiler.AddResource(schemaRoot+name, schema); err != nil {
			t.Fatal(err)
		}
	}
	return compiler
}

func validateWire(t *testing.T, compiler *jsonschema.Compiler, file, name string, payload any) {
	t.Helper()
	schema, err := compiler.Compile(schemaRoot + file + "#/$defs/" + name)
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(payload); err != nil {
		t.Fatal(err)
	}
}

func TestResearchLeaseWireContract(t *testing.T) {
	const schemaFile = "research-job.schema.json"
	compiler := wireSchemaCompiler(t, schemaFile, "portfolio.schema.json")
	var requests map[string]json.RawMessage
	if err := json.Unmarshal(fixture(t, "valid-lease-requests.json"), &requests); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 4, 12, 1, 0, 0, time.UTC)
	lease := jobs.Lease{
		Job:       jobs.Job{ID: jobID, Status: "leased", Request: jobBody(t)},
		WorkerID:  "offline-worker-01",
		ClaimID:   "00000000-0000-4000-8000-000000000011",
		Token:     "00000000-0000-4000-8000-000000000012",
		Attempt:   1,
		ExpiresAt: now.Add(time.Minute),
		Deadline:  now.Add(15 * time.Minute),
	}
	store := stubJobStore{
		claim: func(_ context.Context, worker, claim string, _ time.Duration) (jobs.Lease, error) {
			if worker != lease.WorkerID || claim != lease.ClaimID {
				t.Fatal("wire claim differs from accepted store arguments")
			}
			return lease, nil
		},
		heartbeat: func(_ context.Context, id, worker, token string) (jobs.Lease, error) {
			if id != lease.Job.ID || worker != lease.WorkerID || token != lease.Token {
				t.Fatal("wire heartbeat differs from accepted store arguments")
			}
			return lease, nil
		},
		authorize: func(_ context.Context, id, worker, token, callID string) (jobs.CallPermit, bool, error) {
			if id != lease.Job.ID || worker != lease.WorkerID || token != lease.Token || callID != "00000000-0000-4000-8000-000000000013" {
				t.Fatal("wire call authorization differs from accepted store arguments")
			}
			return jobs.CallPermit{JobID: id, CallID: callID, Attempt: 1, Remaining: 31}, true, nil
		},
	}
	handler := NewHandler(time.Hour, func() time.Time { return now }, store)
	for _, endpoint := range []struct {
		fixture string
		path    string
		request string
		result  string
		status  int
	}{
		{"claim", jobsPath + "/claim", "ClaimRequest", "ClaimResponse", http.StatusOK},
		{"heartbeat", jobsPath + "/" + jobID + "/heartbeat", "HeartbeatRequest", "HeartbeatResponse", http.StatusOK},
		{"authorize_call", jobsPath + "/" + jobID + "/authorize-call", "AuthorizeCallRequest", "AuthorizeCallResponse", http.StatusCreated},
	} {
		t.Run(endpoint.fixture, func(t *testing.T) {
			payload, err := jsonschema.UnmarshalJSON(bytes.NewReader(requests[endpoint.fixture]))
			if err != nil {
				t.Fatal(err)
			}
			validateWire(t, compiler, schemaFile, endpoint.request, payload)

			response := callJobAPI(handler, http.MethodPost, endpoint.path, requests[endpoint.fixture])
			defer response.Body.Close()
			if response.StatusCode != endpoint.status {
				t.Fatalf("got HTTP %d, want %d", response.StatusCode, endpoint.status)
			}
			payload, err = jsonschema.UnmarshalJSON(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			validateWire(t, compiler, schemaFile, endpoint.result, payload)
		})
	}
}
