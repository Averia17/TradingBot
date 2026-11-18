package control

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"tradingbot/control/internal/jobs"
)

const identityJobFixture = "valid-research-identity-job.json"

func TestBoundResearchJobRequiresDeploymentPinAndPreservesIdentity(t *testing.T) {
	raw := fixture(t, identityJobFixture)
	now := time.Date(2026, 10, 4, 12, 1, 0, 0, time.UTC)
	var request []byte
	store := stubJobStore{create: func(_ context.Context, job jobs.CreateJob, admit func() error) (jobs.Job, bool, error) {
		if job.PaperDispatch != nil || admit() != nil {
			t.Fatal("bound research job was not research-only and fresh")
		}
		created := request == nil
		if !created && !bytes.Equal(request, job.Request) {
			return jobs.Job{}, false, jobs.ErrConflict
		}
		request = bytes.Clone(job.Request)
		return jobs.Job{ID: job.ID, Request: job.Request, Status: "queued", CreatedAt: now}, created, nil
	}}
	pin := strings.Repeat("e", 64)
	disabled := NewHandler(time.Hour, func() time.Time { return now }, store)
	unavailable := callJobAPI(disabled, http.MethodPost, jobsPath, raw)
	defer unavailable.Body.Close()
	assertResponse(t, unavailable, 422,
		`{"error":"research_identity_registry_unavailable"}`)
	if request != nil {
		t.Fatal("disabled identity job reached storage")
	}
	if _, err := NewHandlerWithResearchIdentity(time.Hour, time.Now, store, nil, strings.ToUpper(pin)); err == nil {
		t.Fatal("accepted a non-canonical deployment hash")
	}
	handler, err := NewHandlerWithResearchIdentity(time.Hour, func() time.Time { return now }, store, nil, pin)
	if err != nil {
		t.Fatal(err)
	}
	res := callJobAPI(handler, http.MethodPost, jobsPath, []byte(strings.ReplaceAll(string(raw), pin, strings.Repeat("f", 64))))
	defer res.Body.Close()
	assertResponse(t, res, 422, `{"error":"research_identity_registry_mismatch"}`)
	for _, test := range []struct {
		body   []byte
		status int
	}{
		{raw, 201},
		{append([]byte("\n "), raw...), 200},
		{bytes.Replace(raw, []byte("fixture-tesla"), []byte("different-tesla"), 1), 409},
		{bytes.Replace(raw, []byte("test/research-identity.json"), []byte("different/identity.json"), 1), 409},
	} {
		response := callJobAPI(handler, http.MethodPost, jobsPath, test.body)
		response.Body.Close()
		if response.StatusCode != test.status {
			t.Fatalf("got status %d, want %d", response.StatusCode, test.status)
		}
	}
	for _, field := range []string{`"identity_registry":`, `"security_bindings":`, "fixture-tesla"} {
		if !bytes.Contains(request, []byte(field)) {
			t.Fatalf("durable request lost %s", field)
		}
	}
}

func TestBoundResearchJobRejectsAmbiguousBindings(t *testing.T) {
	handler, err := NewHandlerWithResearchIdentity(time.Hour, time.Now, stubJobStore{}, nil, strings.Repeat("e", 64))
	if err != nil {
		t.Fatal(err)
	}
	raw := fixture(t, identityJobFixture)
	for _, test := range []struct {
		old, new string
	}{
		{`"TSLA", "NVDA"`, `"NVDA", "TSLA"`},
		{"fixture-tesla", "fixture-nvidia"},
		{"fixture-tesla", "market:fixture-tesla"},
		{"test/research-identity.json", "test/../research-identity.json"},
		{"test/research-identity.json", "test//research-identity.json"},
		{`"symbol": "TSLA",`, `"alias": "TSLA", "symbol": "TSLA",`},
	} {
		t.Run(test.new, func(t *testing.T) {
			body := bytes.Replace(raw, []byte(test.old), []byte(test.new), 1)
			response := callJobAPI(handler, http.MethodPost, jobsPath, body)
			defer response.Body.Close()
			assertResponse(t, response, 422, `{"error":"invalid_job_request"}`)
		})
	}
}

func TestIdentityResearchCapabilitiesFollowDeploymentPin(t *testing.T) {
	for _, pin := range []string{"", strings.Repeat("e", 64)} {
		handler, err := NewHandlerWithResearchIdentity(time.Hour, time.Now, stubJobStore{}, nil, pin)
		if err != nil {
			t.Fatal(err)
		}
		response := callJobAPI(handler, http.MethodGet, "/v1/capabilities", nil)
		defer response.Body.Close()
		var capability struct {
			Legacy  string   `json:"research_job_schema_version"`
			Jobs    []string `json:"research_job_schema_versions"`
			Results []string `json:"research_result_schema_versions"`
		}
		if err := json.NewDecoder(response.Body).Decode(&capability); err != nil ||
			capability.Legacy != "0.1" || slices.Contains(capability.Jobs, "0.2") != (pin != "") ||
			slices.Contains(capability.Results, "0.3") != (pin != "") {
			t.Fatalf("incorrect identity readiness: %+v, %v", capability, err)
		}
	}
}
