package jobs_http

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"tradingbot/trading-api/internal/accounts"
	"tradingbot/trading-api/internal/contract"
	"tradingbot/trading-api/internal/jobs"
	"tradingbot/trading-api/internal/paper_workflow"
	"tradingbot/trading-api/internal/trading_api"
)

type Store interface {
	Now(context.Context) (time.Time, error)
	Get(context.Context, string) (jobs.Job, error)
	GetResult(context.Context, string) (jobs.Result, error)
	ReadCompletedPaperResearch(context.Context, string) (jobs.CompletedPaperResearch, error)
	GetPaperDispatch(context.Context, string) (jobs.PaperDispatchTask, error)
	ReadyPaperDispatch(context.Context, string, string) ([]jobs.PaperDispatchTask, error)
	FinishPaperDispatch(context.Context, jobs.PaperDispatchTask, string) error
	Create(context.Context, jobs.CreateJob, func() error) (jobs.Job, bool, error)
	CreateBoundPaperJob(context.Context, jobs.BoundPaperJob, func() error) (jobs.Job, bool, error)
}

type Config struct {
	Token          string
	RegistrySHA256 string
	MaxResearchAge time.Duration
}

type handler struct {
	store          Store
	registrySHA256 string
	maxResearchAge time.Duration
}

func NewHandler(store Store, config Config) (http.Handler, error) {
	if store == nil || !validToken(config.Token) || config.MaxResearchAge <= 0 ||
		config.RegistrySHA256 != "" && !digestPattern.MatchString(config.RegistrySHA256) {
		return nil, errors.New("invalid internal jobs HTTP configuration")
	}
	h := handler{store, config.RegistrySHA256, config.MaxResearchAge}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+PathPrefix+"clock", h.clock)
	mux.HandleFunc("GET "+PathPrefix+"ready", h.ready)
	mux.HandleFunc("GET "+PathPrefix+"{id}", h.get)
	mux.HandleFunc("GET "+PathPrefix+"{id}/result", h.result)
	mux.HandleFunc("GET "+PathPrefix+"{id}/completed-paper", h.completedPaper)
	mux.HandleFunc("GET "+PathPrefix+"{id}/dispatch", h.dispatch)
	mux.HandleFunc("POST "+PathPrefix+"{$}", h.create)
	mux.HandleFunc("POST "+PathPrefix+"{id}/dispatch", h.finish)

	token := sha256.Sum256([]byte("Bearer " + config.Token))
	writes := make(chan struct{}, 4)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := sha256.Sum256([]byte(r.Header.Get("Authorization")))
		if subtle.ConstantTimeCompare(token[:], header[:]) != 1 {
			writeError(w, http.StatusUnauthorized, "invalid_internal_credentials")
			return
		}
		if r.Method == http.MethodPost {
			select {
			case writes <- struct{}{}:
				defer func() { <-writes }()
			default:
				writeError(w, http.StatusServiceUnavailable, "jobs_busy")
				return
			}
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		mux.ServeHTTP(w, r.WithContext(ctx))
	}), nil
}

func (h handler) clock(w http.ResponseWriter, r *http.Request) {
	now, err := h.store.Now(r.Context())
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, clockDocument{now})
}

func (h handler) get(w http.ResponseWriter, r *http.Request) {
	id, ok := readID(w, r)
	if !ok {
		return
	}
	job, err := h.store.Get(r.Context(), id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, jobWire(job))
}

func (h handler) result(w http.ResponseWriter, r *http.Request) {
	id, ok := readID(w, r)
	if !ok {
		return
	}
	result, err := h.store.GetResult(r.Context(), id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resultWire(result))
}

func (h handler) completedPaper(w http.ResponseWriter, r *http.Request) {
	id, ok := readID(w, r)
	if !ok {
		return
	}
	completed, err := h.store.ReadCompletedPaperResearch(r.Context(), id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	document := completedPaperDocument{Job: jobWire(completed.Job), Result: resultWire(completed.Result)}
	if completed.Dispatch != nil {
		dispatch := dispatchWire(*completed.Dispatch)
		document.Dispatch = &dispatch
	}
	writeJSON(w, http.StatusOK, document)
}

func (h handler) dispatch(w http.ResponseWriter, r *http.Request) {
	id, ok := readID(w, r)
	if !ok {
		return
	}
	task, err := h.store.GetPaperDispatch(r.Context(), id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, dispatchWire(task))
}

func (h handler) ready(w http.ResponseWriter, r *http.Request) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(query["account_id"]) != 1 || len(query["after"]) > 1 ||
		(len(query) != 1 && (len(query) != 2 || !query.Has("after"))) {
		writeError(w, http.StatusBadRequest, "invalid_query")
		return
	}
	account, after := query.Get("account_id"), query.Get("after")
	if !uuidPattern.MatchString(account) || after != "" && !uuidPattern.MatchString(after) {
		writeError(w, http.StatusBadRequest, "invalid_query")
		return
	}
	tasks, err := h.store.ReadyPaperDispatch(r.Context(), account, after)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	documents := make([]dispatchDocument, 0, len(tasks))
	for _, task := range tasks {
		documents = append(documents, dispatchWire(task))
	}
	writeJSON(w, http.StatusOK, documents)
}

func (h handler) finish(w http.ResponseWriter, r *http.Request) {
	id, ok := readID(w, r)
	if !ok {
		return
	}
	var input finishRequest
	if !readDocument(w, r, contract.InternalJobFinish, &input) {
		return
	}
	var task jobs.PaperDispatchTask
	task.JobID, task.AccountID = id, input.AccountID
	_, _ = hex.Decode(task.RequestSHA256[:], []byte(input.RequestSHA256))
	if err := h.store.FinishPaperDispatch(r.Context(), task, input.Status); err != nil {
		writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h handler) create(w http.ResponseWriter, r *http.Request) {
	var input createRequest
	if !readDocument(w, r, contract.InternalJobCreate, &input) {
		return
	}
	job, portfolio, application, err := h.validateCreate(input)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid_paper_job")
		return
	}
	ctx := r.Context()
	started := time.Now()
	now, err := h.store.Now(ctx)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	admit := func() error {
		if input.ReplayOnly {
			return jobs.ErrNotFound
		}
		at := now.Add(time.Since(started))
		if input.AdmitThrough != nil && at.After(*input.AdmitThrough) {
			return jobs.ErrAdmissionDeadline
		}
		if portfolio.AsOf.After(at) || at.Sub(portfolio.AsOf) > h.maxResearchAge ||
			application.SnapshotContext.CheckMarks(at) != nil {
			return accounts.ErrStale
		}
		if application.Identity != nil {
			for _, binding := range application.Identity.Securities {
				if binding.ExpiresAt != nil && !binding.ExpiresAt.After(at) {
					return accounts.ErrStale
				}
			}
		}
		return nil
	}

	var saved jobs.Job
	var created bool
	if input.RegistrySHA == "" {
		saved, created, err = h.store.Create(ctx, job, admit)
	} else {
		saved, created, err = h.store.CreateBoundPaperJob(
			ctx, jobs.BoundPaperJob{Job: job, OwnerRegistrySHA256: input.RegistrySHA}, admit)
	}
	if err != nil {
		writeStoreError(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, jobWire(saved))
}

func (h handler) validateCreate(input createRequest) (jobs.CreateJob, contract.Portfolio, paper_workflow.Request, error) {
	var zeroJob jobs.CreateJob
	var zeroPortfolio contract.Portfolio
	var application paper_workflow.Request
	if !uuidPattern.MatchString(input.ID) || !uuidPattern.MatchString(input.SnapshotID) ||
		!uuidPattern.MatchString(input.AccountID) || len(input.Request) > maxDocumentBytes ||
		len(input.Snapshot) > maxDocumentBytes || len(input.Application) > maxDocumentBytes {
		return zeroJob, zeroPortfolio, application, errInvalidRequest
	}
	portfolio, err := contract.ParsePortfolio(input.Snapshot)
	if err != nil || portfolio.ID != input.SnapshotID ||
		input.AdmitThrough != nil && input.AdmitThrough.Before(portfolio.AsOf) {
		return zeroJob, zeroPortfolio, application, errInvalidRequest
	}
	var fields map[string]json.RawMessage
	var id, version, policyVersion string
	if contract.Decode(input.Request, contract.ResearchJobs, &fields) != nil ||
		json.Unmarshal(fields["job_id"], &id) != nil || id != input.ID ||
		json.Unmarshal(fields["job_schema_version"], &version) != nil ||
		json.Unmarshal(fields["policy_version"], &policyVersion) != nil ||
		!bytes.Equal(fields["portfolio"], input.Snapshot) ||
		contract.Decode(input.Application, contract.PaperApplication, &application) != nil ||
		application.JobID != id || application.SnapshotContext.AccountID != input.AccountID ||
		application.SnapshotContext.Policy.Version != policyVersion ||
		application.Intent != nil || application.SnapshotContext.CheckPortfolio(portfolio) != nil {
		return zeroJob, zeroPortfolio, application, errInvalidRequest
	}
	job := jobs.CreateJob{
		ID: id, Request: input.Request, SnapshotID: input.SnapshotID, Snapshot: input.Snapshot,
		PaperDispatch: &jobs.PaperDispatchInput{AccountID: input.AccountID, Request: input.Application},
	}
	if version == "0.1" {
		if input.RegistrySHA != "" || application.Identity != nil || application.Version != paper_workflow.Version {
			return zeroJob, zeroPortfolio, application, errInvalidRequest
		}
	} else if h.registrySHA256 == "" || input.RegistrySHA != h.registrySHA256 ||
		application.Version != paper_workflow.IdentityVersion || application.Identity == nil ||
		application.Identity.Registry.SHA256 != h.registrySHA256 ||
		!trading_api.CheckArchivedIdentityRequest(jobs.Job{ID: id, Request: input.Request}, h.registrySHA256) {
		return zeroJob, zeroPortfolio, application, errInvalidRequest
	}
	return job, portfolio, application, nil
}

func readID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if !uuidPattern.MatchString(id) {
		writeError(w, http.StatusBadRequest, "invalid_job_id")
		return "", false
	}
	return id, true
}

func readDocument(w http.ResponseWriter, r *http.Request, schema *jsonschema.Schema, target any) bool {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_content_type")
		return false
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	var oversized *http.MaxBytesError
	if errors.As(err, &oversized) {
		writeError(w, http.StatusRequestEntityTooLarge, "body_too_large")
		return false
	}
	if err != nil || contract.Decode(raw, schema, target) != nil {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return false
	}
	return true
}

func writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, jobs.ErrNotFound):
		writeError(w, http.StatusNotFound, "job_not_found")
	case errors.Is(err, jobs.ErrConflict):
		writeError(w, http.StatusConflict, "job_conflict")
	case errors.Is(err, jobs.ErrAdmissionDeadline):
		writeError(w, http.StatusUnprocessableEntity, "admission_deadline")
	case errors.Is(err, accounts.ErrStale):
		writeError(w, http.StatusUnprocessableEntity, "stale_snapshot")
	default:
		writeError(w, http.StatusServiceUnavailable, "jobs_storage_unavailable")
	}
}

func writeError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]string{"error": code})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
