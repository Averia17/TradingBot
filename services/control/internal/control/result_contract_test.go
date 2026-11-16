package control

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"tradingbot/control/internal/jobs"
)

func TestResearchResultWireContract(t *testing.T) {
	for _, name := range []string{"valid-result-request.json", "valid-offline-result-request.json"} {
		t.Run(name, func(t *testing.T) {
			checkResearchResultWireContract(t, fixture(t, name))
		})
	}
}

func checkResearchResultWireContract(t *testing.T, request []byte) {
	t.Helper()
	const schemaFile = "research-result.schema.json"
	compiler := wireSchemaCompiler(t, schemaFile, "research-job.schema.json", "portfolio.schema.json", "schema.json")
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(request))
	if err != nil {
		t.Fatal(err)
	}
	validateWire(t, compiler, schemaFile, "CompleteRequest", value)

	now := time.Date(2026, 10, 4, 12, 1, 0, 0, time.UTC)
	var saved jobs.Result
	store := stubJobStore{
		complete: func(_ context.Context, input jobs.CompleteJob, verify func(jobs.Job, time.Time) error) (jobs.Result, bool, error) {
			if input.JobID != jobID || input.WorkerID != "fixture-worker" || input.Token != "00000000-0000-0000-0000-000000000020" {
				t.Fatal("wire result authority was not passed correctly")
			}
			if err := verify(jobs.Job{ID: jobID, Status: "leased", Request: jobBody(t)}, now); err != nil {
				t.Fatal(err)
			}
			saved = jobs.Result{JobID: input.JobID, Attempt: 1, CompletedAt: now, Payload: input.Payload}
			return saved, true, nil
		},
		getResult: func(_ context.Context, _ string) (jobs.Result, error) {
			return saved, nil
		},
	}
	handler := NewHandler(time.Hour, func() time.Time { return now }, store)
	for _, endpoint := range []struct {
		method string
		path   string
		body   json.RawMessage
		status int
	}{
		{http.MethodPost, jobsPath + "/" + jobID + "/complete", request, http.StatusCreated},
		{http.MethodGet, jobsPath + "/" + jobID + "/result", nil, http.StatusOK},
	} {
		t.Run(endpoint.method, func(t *testing.T) {
			response := callJobAPI(handler, endpoint.method, endpoint.path, endpoint.body)
			defer response.Body.Close()
			if response.StatusCode != endpoint.status {
				t.Fatalf("got HTTP %d, want %d", response.StatusCode, endpoint.status)
			}
			result, err := jsonschema.UnmarshalJSON(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			validateWire(t, compiler, schemaFile, "CompleteResponse", result)
		})
	}
}
