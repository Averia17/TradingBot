package paper_check

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/santhosh-tekuri/jsonschema/v6"

	"tradingbot/trading-api/internal/accounts_http"
	"tradingbot/trading-api/internal/contract"
)

const (
	AccountsPath = "/internal/v1/paper/diagnostics"
	JobsPath     = "/internal/v1/research/diagnostics"

	maxInput  = 64 << 10
	maxOutput = 4 << 20
)

type researchRequest struct {
	AccountID string `json:"account_id"`
	JobID     string `json:"job_id,omitempty"`
	Pipeline  bool   `json:"pipeline"`
}

type researchDiagnostics struct {
	CheckedAt time.Time             `json:"checked_at"`
	Budget    *researchBudget       `json:"budget,omitempty"`
	Issues    []issue               `json:"issues"`
	Rows      []researchPipelineRow `json:"pipeline_rows"`
	Truncated bool                  `json:"truncated"`
}

type accountRequest struct {
	AccountID    string        `json:"account_id"`
	UnknownAfter time.Duration `json:"unknown_after_ns"`
	StallAfter   time.Duration `json:"stall_after_ns"`
	Pipeline     bool          `json:"pipeline"`
	JobIDs       []string      `json:"job_ids,omitempty"`
	JobsCutoff   time.Time     `json:"jobs_cutoff,omitzero"`
}

type accountDiagnostics struct {
	AccountID         string               `json:"account_id"`
	CheckedAt         time.Time            `json:"checked_at"`
	AccountVersion    int64                `json:"account_version"`
	ProjectionVersion int64                `json:"projection_version"`
	Issues            []issue              `json:"issues"`
	Applications      []accountPipelineRow `json:"applications"`
	FillRelay         fillRelay            `json:"fill_relay"`
}

func NewJobsHandler(dsn, token string) http.Handler {
	return newDiagnosticsHandler(token, contract.PaperJobsDiagnosticsRequest, func(ctx context.Context, req researchRequest) (researchDiagnostics, error) {
		var reply researchDiagnostics
		if !uuid.MatchString(req.AccountID) || (req.JobID != "" && !uuid.MatchString(req.JobID)) {
			return reply, errInvalidDiagnostics
		}
		err := readDiagnostics(ctx, dsn, func(tx pgx.Tx, checkedAt time.Time) error {
			r := report{AccountID: req.AccountID, CheckedAt: checkedAt, Issues: []issue{}}
			if req.JobID != "" {
				r.Research = &researchBudget{JobID: req.JobID}
				if err := checkBudget(ctx, tx, &r); err != nil {
					return err
				}
			}
			reply.CheckedAt, reply.Budget, reply.Issues = checkedAt, r.Research, r.Issues
			reply.Rows = []researchPipelineRow{}
			if req.Pipeline {
				var err error
				reply.Rows, reply.Truncated, err = readResearchPipeline(ctx, tx, req.AccountID, req.JobID, checkedAt)
				return err
			}
			return nil
		})
		return reply, err
	})
}

func NewAccountsHandler(dsn, token string) http.Handler {
	return newDiagnosticsHandler(token, contract.PaperAccountDiagnosticsRequest, func(ctx context.Context, req accountRequest) (accountDiagnostics, error) {
		var reply accountDiagnostics
		if !validAccountRequest(req) {
			return reply, errInvalidDiagnostics
		}
		err := readDiagnostics(ctx, dsn, func(tx pgx.Tx, checkedAt time.Time) error {
			r := report{AccountID: req.AccountID, CheckedAt: checkedAt, Issues: []issue{}}
			if err := checkAccount(ctx, tx, req.UnknownAfter, &r); err != nil {
				return err
			}
			reply.Applications = []accountPipelineRow{}
			if req.Pipeline {
				// The account checks use the owner's single repeatable read.
				// Pipeline receipts are limited to the jobs owner's earlier view.
				asOf := checkedAt
				if req.JobsCutoff.Before(asOf) {
					asOf = req.JobsCutoff
				}
				applications, err := readAccountPipeline(ctx, tx, req.AccountID, req.JobIDs, asOf)
				if err != nil {
					return err
				}
				r.Pipeline = &paperPipeline{}
				if err := checkFillRelay(ctx, tx, req.StallAfter, &r); err != nil {
					return err
				}
				reply.Applications, reply.FillRelay = applications, r.Pipeline.FillRelay
			}
			reply.AccountID, reply.CheckedAt, reply.AccountVersion, reply.ProjectionVersion, reply.Issues =
				req.AccountID, checkedAt, r.AccountVersion, r.ProjectionVersion, r.Issues
			return nil
		})
		return reply, err
	})
}

func validAccountRequest(req accountRequest) bool {
	if !uuid.MatchString(req.AccountID) || req.UnknownAfter <= 0 || req.StallAfter <= 0 ||
		len(req.JobIDs) > pipelinePageSize || req.Pipeline == req.JobsCutoff.IsZero() ||
		!req.Pipeline && len(req.JobIDs) != 0 {
		return false
	}
	seen := make(map[string]bool, len(req.JobIDs))
	for _, id := range req.JobIDs {
		if !uuid.MatchString(id) || seen[id] {
			return false
		}
		seen[id] = true
	}
	return true
}

var errInvalidDiagnostics = errors.New("invalid paper diagnostics data")

func newDiagnosticsHandler[I, O any](token string, schema *jsonschema.Schema, inspect func(context.Context, I) (O, error)) http.Handler {
	expected := sha256.Sum256([]byte("Bearer " + token))
	active := make(chan struct{}, 2)
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		provided := sha256.Sum256([]byte(req.Header.Get("Authorization")))
		if token == "" || subtle.ConstantTimeCompare(provided[:], expected[:]) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if req.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		select {
		case active <- struct{}{}:
			defer func() { <-active }()
		default:
			http.Error(w, "paper diagnostics unavailable", http.StatusServiceUnavailable)
			return
		}
		media, _, _ := mime.ParseMediaType(req.Header.Get("Content-Type"))
		if media != "application/json" {
			http.Error(w, "unsupported content type", http.StatusUnsupportedMediaType)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, req.Body, maxInput))
		var input I
		if err != nil || !utf8.Valid(body) || contract.Decode(body, schema, &input) != nil {
			http.Error(w, "invalid paper diagnostics request", http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(req.Context(), 7*time.Second)
		defer cancel()
		output, err := inspect(ctx, input)
		if errors.Is(err, errInvalidDiagnostics) {
			http.Error(w, "invalid paper diagnostics request", http.StatusBadRequest)
			return
		}
		if err != nil {
			http.Error(w, "paper diagnostics unavailable", http.StatusServiceUnavailable)
			return
		}
		raw, err := json.Marshal(output)
		if err != nil || len(raw) > maxOutput {
			http.Error(w, "paper diagnostics unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(raw)
	})
}

func checkRemote(ctx context.Context, accountOrigin, researchOrigin string, unknownAfter, stallAfter time.Duration, r *report) error {
	token, err := accounts_http.InternalToken()
	if err != nil || token == "" {
		return errInvalidDiagnostics
	}
	client := &http.Client{
		Timeout: 8 * time.Second,
		Transport: &http.Transport{
			DialContext:         (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
			TLSHandshakeTimeout: 3 * time.Second, MaxResponseHeaderBytes: 8 << 10,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	defer client.CloseIdleConnections()
	var research researchDiagnostics
	if r.Research != nil || r.Pipeline != nil {
		req := researchRequest{AccountID: r.AccountID, Pipeline: r.Pipeline != nil}
		if r.Research != nil {
			req.JobID = r.Research.JobID
		}
		if err := diagnosticHTTP(ctx, client, token, researchOrigin, JobsPath, req, &research, contract.PaperJobsDiagnosticsResponse); err != nil {
			return err
		}
		if research.CheckedAt.IsZero() || len(research.Rows) > pipelinePageSize ||
			(req.JobID != "" && (research.Budget == nil || research.Budget.JobID != req.JobID)) {
			return errInvalidDiagnostics
		}
		r.Research = research.Budget
	}
	req := accountRequest{
		AccountID: r.AccountID, UnknownAfter: unknownAfter, StallAfter: stallAfter,
		Pipeline: r.Pipeline != nil,
	}
	if req.Pipeline {
		req.JobIDs, req.JobsCutoff = pipelineJobIDs(research.Rows), research.CheckedAt
	}
	var account accountDiagnostics
	if err := diagnosticHTTP(ctx, client, token, accountOrigin, AccountsPath, req, &account, contract.PaperAccountDiagnosticsResponse); err != nil {
		return err
	}
	if account.AccountID != r.AccountID || account.CheckedAt.IsZero() || len(account.Applications) > len(req.JobIDs) {
		return errInvalidDiagnostics
	}
	r.CheckedAt, r.AccountVersion, r.ProjectionVersion = account.CheckedAt, account.AccountVersion, account.ProjectionVersion
	r.Issues = append(r.Issues, account.Issues...)
	r.Issues = append(r.Issues, research.Issues...)
	if r.Pipeline != nil {
		if research.CheckedAt.Before(r.CheckedAt) {
			r.CheckedAt = research.CheckedAt
		}
		composePipeline(r, research.Rows, research.Truncated, account.Applications, stallAfter)
		r.Pipeline.FillRelay = account.FillRelay
	}
	return nil
}

func diagnosticHTTP(ctx context.Context, client *http.Client, token, origin, path string, request, result any, schema *jsonschema.Schema) error {
	u, err := url.Parse(origin)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") ||
		u.User != nil || (u.Path != "" && u.Path != "/") || u.RawPath != "" ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return errInvalidDiagnostics
	}
	u.Path = path
	raw, err := json.Marshal(request)
	if err != nil || len(raw) > maxInput {
		return errInvalidDiagnostics
	}
	message, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(raw))
	if err != nil {
		return errInvalidDiagnostics
	}
	message.Header.Set("Content-Type", "application/json")
	message.Header.Set("Authorization", "Bearer "+token)
	response, err := client.Do(message)
	if err != nil {
		return errors.New("paper diagnostics HTTP unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return errors.New("paper diagnostics HTTP unavailable")
	}
	media, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	raw, err = io.ReadAll(io.LimitReader(response.Body, maxOutput+1))
	if err != nil || len(raw) > maxOutput || media != "application/json" || contract.Decode(raw, schema, result) != nil {
		return errInvalidDiagnostics
	}
	return nil
}
