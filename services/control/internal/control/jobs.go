package control

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"regexp"
	"strings"
	"time"

	"tradingbot/control/internal/contract"
	"tradingbot/control/internal/jobs"
)

const (
	jobSchemaVersion         = "0.1"
	identityJobSchemaVersion = "0.2"
	maxLLMCalls              = 64
	jobStoreTimeout          = 3 * time.Second
)

var (
	jobUUID        = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	manifestRef    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,511}$`)
	sha256Hex      = regexp.MustCompile(`^[0-9a-f]{64}$`)
	policyVersion  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	errSnapshotAge = errors.New("stale or future portfolio snapshot")
)

type jobStore interface {
	Create(context.Context, jobs.CreateJob, func() error) (jobs.Job, bool, error)
	Get(context.Context, string) (jobs.Job, error)
	Ping(context.Context) error
	Claim(context.Context, string, string, time.Duration) (jobs.Lease, error)
	Heartbeat(context.Context, string, string, string) (jobs.Lease, error)
	AuthorizeCall(context.Context, string, string, string, string) (jobs.CallPermit, bool, error)
	Complete(context.Context, jobs.CompleteJob, func(jobs.Job, time.Time) error) (jobs.Result, bool, error)
	GetResult(context.Context, string) (jobs.Result, error)
}

type jobHTTP struct {
	store                  jobStore
	now                    func() time.Time
	maxResearchAge         time.Duration
	identityRegistrySHA256 string
}

type manifest struct {
	Ref    string `json:"ref"`
	SHA256 string `json:"sha256"`
}

type securityBinding struct {
	Symbol     string `json:"symbol"`
	SecurityID string `json:"security_id"`
}

type jobRequest struct {
	Version          string            `json:"job_schema_version"`
	ID               string            `json:"job_id"`
	Portfolio        json.RawMessage   `json:"portfolio"`
	RequestedSymbols []string          `json:"requested_symbols"`
	DataManifest     manifest          `json:"data_manifest"`
	PolicyVersion    string            `json:"policy_version"`
	MaxLLMCalls      int               `json:"max_llm_calls"`
	IdentityRegistry *manifest         `json:"identity_registry,omitempty"`
	SecurityBindings []securityBinding `json:"security_bindings,omitempty"`
}

func validIdentityRegistry(registry *manifest) bool {
	return registry != nil && manifestRef.MatchString(registry.Ref) && fs.ValidPath(registry.Ref) &&
		sha256Hex.MatchString(registry.SHA256)
}

func (r jobRequest) hasIdentityBindings() bool {
	if !validIdentityRegistry(r.IdentityRegistry) || len(r.SecurityBindings) != len(r.RequestedSymbols) {
		return false
	}
	seen := make(map[string]struct{}, len(r.SecurityBindings))
	for i, binding := range r.SecurityBindings {
		if binding.Symbol != r.RequestedSymbols[i] || !policyVersion.MatchString(binding.SecurityID) {
			return false
		}
		if _, duplicate := seen[binding.SecurityID]; duplicate {
			return false
		}
		seen[binding.SecurityID] = struct{}{}
	}
	return true
}

func (h jobHTTP) create(w http.ResponseWriter, r *http.Request) {
	body, ok := readJSONBody(w, r)
	if !ok {
		return
	}
	var parts map[string]json.RawMessage
	var version string
	if contract.Decode(body, nil, &parts) != nil || json.Unmarshal(parts["job_schema_version"], &version) != nil {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	var request jobRequest
	if version == identityJobSchemaVersion {
		if contract.Decode(body, contract.ResearchIdentityJob, &request) != nil || !request.hasIdentityBindings() {
			writeError(w, http.StatusUnprocessableEntity, "invalid_job_request")
			return
		}
		if h.identityRegistrySHA256 == "" {
			writeError(w, http.StatusUnprocessableEntity, "research_identity_registry_unavailable")
			return
		}
		if request.IdentityRegistry.SHA256 != h.identityRegistrySHA256 {
			writeError(w, http.StatusUnprocessableEntity, "research_identity_registry_mismatch")
			return
		}
	} else {
		var manifestParts map[string]json.RawMessage
		if !hasExactKeys(parts, "job_schema_version", "job_id", "portfolio", "requested_symbols", "data_manifest",
			"policy_version", "max_llm_calls") ||
			json.Unmarshal(parts["data_manifest"], &manifestParts) != nil || !hasExactKeys(manifestParts, "ref", "sha256") ||
			json.Unmarshal(body, &request) != nil {
			writeError(w, http.StatusBadRequest, "invalid_request")
			return
		}
	}
	request.ID = strings.ToLower(request.ID)
	if (request.Version != jobSchemaVersion && request.Version != identityJobSchemaVersion) || !jobUUID.MatchString(request.ID) ||
		!policyVersion.MatchString(request.PolicyVersion) || request.MaxLLMCalls < 1 || request.MaxLLMCalls > maxLLMCalls ||
		!manifestRef.MatchString(request.DataManifest.Ref) || !sha256Hex.MatchString(request.DataManifest.SHA256) ||
		contract.CheckSymbols(request.RequestedSymbols) != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid_job_request")
		return
	}
	portfolio, err := contract.ParsePortfolio(request.Portfolio)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid_portfolio")
		return
	}
	// The snapshot is part of the submitted research evidence. The canonical
	// payload has deterministic JSON fields and keeps financial values as strings.
	portfolio.ID = strings.ToLower(portfolio.ID)
	portfolio.AsOf = portfolio.AsOf.UTC()
	snapshot, err := json.Marshal(portfolio)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid_portfolio")
		return
	}
	request.Portfolio = snapshot
	payload, err := json.Marshal(request)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid_job_request")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), jobStoreTimeout)
	defer cancel()
	job, created, err := h.store.Create(ctx, jobs.CreateJob{
		ID:         request.ID,
		Request:    payload,
		SnapshotID: portfolio.ID,
		Snapshot:   snapshot,
	}, func() error {
		now := h.now()
		if portfolio.AsOf.After(now) || now.Sub(portfolio.AsOf) > h.maxResearchAge {
			return errSnapshotAge
		}
		return nil
	})
	switch {
	case errors.Is(err, jobs.ErrConflict):
		writeError(w, http.StatusConflict, "job_conflict")
	case errors.Is(err, errSnapshotAge):
		writeError(w, http.StatusUnprocessableEntity, "stale_or_future_snapshot")
	case err != nil:
		writeError(w, http.StatusServiceUnavailable, "storage_unavailable")
	default:
		w.Header().Set("Location", "/v1/research/jobs/"+job.ID)
		status := http.StatusOK
		if created {
			status = http.StatusCreated
		}
		writeJob(w, status, job)
	}
}

func (h jobHTTP) get(w http.ResponseWriter, r *http.Request) {
	id := strings.ToLower(r.PathValue("id"))
	if !jobUUID.MatchString(id) {
		writeError(w, http.StatusBadRequest, "invalid_job_id")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), jobStoreTimeout)
	defer cancel()
	job, err := h.store.Get(ctx, id)
	switch {
	case errors.Is(err, jobs.ErrNotFound):
		writeError(w, http.StatusNotFound, "job_not_found")
	case err != nil:
		writeError(w, http.StatusServiceUnavailable, "storage_unavailable")
	default:
		writeJob(w, http.StatusOK, job)
	}
}

func writeJob(w http.ResponseWriter, status int, job jobs.Job) {
	writeJSON(w, status, struct {
		ID        string          `json:"job_id"`
		Status    string          `json:"status"`
		CreatedAt time.Time       `json:"created_at"`
		Request   json.RawMessage `json:"request"`
	}{job.ID, job.Status, job.CreatedAt.UTC(), job.Request})
}
