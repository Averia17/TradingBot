package jobs_http

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"time"

	"tradingbot/trading-api/internal/accounts"
	"tradingbot/trading-api/internal/contract"
	"tradingbot/trading-api/internal/jobs"
)

type Client struct {
	origin string
	token  string
	http   *http.Client
}

func NewClient(origin, token string) (*Client, error) {
	u, err := url.Parse(origin)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") ||
		u.User != nil || (u.Path != "" && u.Path != "/") || u.RawPath != "" ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || !validToken(token) {
		return nil, errors.New("invalid Trading API origin or internal token")
	}
	dialer := &net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}
	return &Client{
		origin: u.Scheme + "://" + u.Host,
		token:  token,
		http: &http.Client{
			Timeout: 6 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
			Transport: &http.Transport{
				DialContext:           dialer.DialContext,
				TLSHandshakeTimeout:   3 * time.Second,
				ResponseHeaderTimeout: 5 * time.Second,
				IdleConnTimeout:       30 * time.Second,
				MaxConnsPerHost:       8,
			},
		},
	}, nil
}

func (c *Client) Close() { c.http.CloseIdleConnections() }

func (c *Client) Now(ctx context.Context) (time.Time, error) {
	var result clockDocument
	if _, err := c.request(ctx, http.MethodGet, "clock", nil, &result); err != nil {
		return time.Time{}, err
	}
	if result.Now.IsZero() {
		return time.Time{}, errInvalidResponse
	}
	return result.Now, nil
}

func (c *Client) Get(ctx context.Context, id string) (jobs.Job, error) {
	if !uuidPattern.MatchString(id) {
		return jobs.Job{}, errInvalidRequest
	}
	var result jobDocument
	if _, err := c.request(ctx, http.MethodGet, id, nil, &result); err != nil {
		return jobs.Job{}, err
	}
	if result.ID != id || result.CreatedAt.IsZero() || !json.Valid(result.Request) {
		return jobs.Job{}, errInvalidResponse
	}
	return result.value(), nil
}

func (c *Client) GetResult(ctx context.Context, id string) (jobs.Result, error) {
	if !uuidPattern.MatchString(id) {
		return jobs.Result{}, errInvalidRequest
	}
	var result resultDocument
	if _, err := c.request(ctx, http.MethodGet, id+"/result", nil, &result); err != nil {
		return jobs.Result{}, err
	}
	if result.JobID != id || result.Attempt < 1 || result.CompletedAt.IsZero() || !json.Valid(result.Payload) {
		return jobs.Result{}, errInvalidResponse
	}
	return result.value(), nil
}

func (c *Client) ReadCompletedPaperResearch(ctx context.Context, id string) (jobs.CompletedPaperResearch, error) {
	if !uuidPattern.MatchString(id) {
		return jobs.CompletedPaperResearch{}, errInvalidRequest
	}
	var doc completedPaperDocument
	if _, err := c.request(ctx, http.MethodGet, id+"/completed-paper", nil, &doc); err != nil {
		return jobs.CompletedPaperResearch{}, err
	}
	if doc.Job.ID != id || doc.Job.Status != "completed" || !json.Valid(doc.Job.Request) ||
		doc.Job.CreatedAt.IsZero() || doc.Result.JobID != id || doc.Result.Attempt < 1 ||
		doc.Result.CompletedAt.Before(doc.Job.CreatedAt) || !json.Valid(doc.Result.Payload) {
		return jobs.CompletedPaperResearch{}, errInvalidResponse
	}
	completed := jobs.CompletedPaperResearch{
		CompletedResearch: jobs.CompletedResearch{Job: doc.Job.value(), Result: doc.Result.value()},
	}
	if doc.Dispatch != nil {
		dispatch, err := doc.Dispatch.value()
		if err != nil || dispatch.JobID != id {
			return jobs.CompletedPaperResearch{}, errInvalidResponse
		}
		completed.Dispatch = &dispatch
	}
	return completed, nil
}

func (c *Client) GetPaperDispatch(ctx context.Context, id string) (jobs.PaperDispatchTask, error) {
	if !uuidPattern.MatchString(id) {
		return jobs.PaperDispatchTask{}, errInvalidRequest
	}
	var result dispatchDocument
	if _, err := c.request(ctx, http.MethodGet, id+"/dispatch", nil, &result); err != nil {
		return jobs.PaperDispatchTask{}, err
	}
	if result.JobID != id {
		return jobs.PaperDispatchTask{}, errInvalidResponse
	}
	return result.value()
}

func (c *Client) ReadyPaperDispatch(ctx context.Context, account, after string) ([]jobs.PaperDispatchTask, error) {
	if !uuidPattern.MatchString(account) || after != "" && !uuidPattern.MatchString(after) {
		return nil, errInvalidRequest
	}
	query := url.Values{"account_id": []string{account}}
	if after != "" {
		query.Set("after", after)
	}
	var result []dispatchDocument
	if _, err := c.request(ctx, http.MethodGet, "ready?"+query.Encode(), nil, &result); err != nil {
		return nil, err
	}
	if result == nil || len(result) > jobs.MaxPaperDispatchPage {
		return nil, errInvalidResponse
	}
	tasks := make([]jobs.PaperDispatchTask, 0, len(result))
	for _, doc := range result {
		task, err := doc.value()
		if err != nil || task.AccountID != account || task.JobID <= after {
			return nil, errInvalidResponse
		}
		tasks = append(tasks, task)
		after = task.JobID
	}
	return tasks, nil
}

func (c *Client) FinishPaperDispatch(ctx context.Context, task jobs.PaperDispatchTask, status string) error {
	if !uuidPattern.MatchString(task.JobID) || !uuidPattern.MatchString(task.AccountID) {
		return errInvalidRequest
	}
	input := finishRequest{task.AccountID, hex.EncodeToString(task.RequestSHA256[:]), status}
	_, err := c.request(ctx, http.MethodPost, task.JobID+"/dispatch", input, nil)
	return err
}

// Create is the owner-side paper admission route. A normal research job
// continues to use the Trading API's existing public research route.
func (c *Client) Create(ctx context.Context, input jobs.CreateJob, admit func() error) (jobs.Job, bool, error) {
	return c.create(ctx, input, "", admit)
}

func (c *Client) CreateBoundPaperJob(ctx context.Context, input jobs.BoundPaperJob, admit func() error) (jobs.Job, bool, error) {
	return c.create(ctx, input.Job, input.OwnerRegistrySHA256, admit)
}

func (c *Client) create(ctx context.Context, input jobs.CreateJob, registrySHA string, admit func() error) (jobs.Job, bool, error) {
	if input.PaperDispatch == nil || !uuidPattern.MatchString(input.ID) {
		return jobs.Job{}, false, errInvalidRequest
	}
	request := createRequest{
		ID: input.ID, Request: input.Request, SnapshotID: input.SnapshotID, Snapshot: input.Snapshot,
		AccountID: input.PaperDispatch.AccountID, Application: input.PaperDispatch.Request, RegistrySHA: registrySHA,
	}
	if !input.AdmitThrough.IsZero() {
		deadline := input.AdmitThrough.UTC()
		request.AdmitThrough = &deadline
	}
	var admissionError error
	switch _, err := c.Get(ctx, input.ID); {
	case err == nil:
		// The owner checks exact bytes without creating a replacement record.
		request.ReplayOnly = true
	case !errors.Is(err, jobs.ErrNotFound):
		return jobs.Job{}, false, err
	case admit != nil:
		admissionError = admit()
		// A concurrent caller may have committed while this callback ran.
		// Prefer a matching existing job but never create after a refusal.
		request.ReplayOnly = admissionError != nil
	}

	var response jobDocument
	status, err := c.request(ctx, http.MethodPost, "", request, &response)
	if admissionError != nil && errors.Is(err, jobs.ErrNotFound) {
		return jobs.Job{}, false, admissionError
	}
	if err != nil {
		return jobs.Job{}, false, err
	}
	if response.ID != input.ID || response.CreatedAt.IsZero() || !bytes.Equal(response.Request, input.Request) ||
		request.ReplayOnly && status != http.StatusOK {
		return jobs.Job{}, false, errInvalidResponse
	}
	return response.value(), status == http.StatusCreated, nil
}

func (c *Client) request(ctx context.Context, method, path string, input, target any) (int, error) {
	var body io.Reader
	if input != nil {
		raw, err := json.Marshal(input)
		if err != nil || len(raw) > maxRequestBytes {
			return 0, errInvalidRequest
		}
		body = bytes.NewReader(raw)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.origin+PathPrefix+path, body)
	if err != nil {
		return 0, errInvalidRequest
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return 0, errors.New("Trading API jobs request failed")
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusNotFound:
		return response.StatusCode, jobs.ErrNotFound
	case http.StatusConflict:
		return response.StatusCode, jobs.ErrConflict
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge, http.StatusUnsupportedMediaType:
		return response.StatusCode, errInvalidRequest
	case http.StatusUnprocessableEntity:
		var payload struct {
			Error string `json:"error"`
		}
		raw, err := readResponse(response.Body, 4096)
		if err == nil && contract.Decode(raw, nil, &payload) == nil {
			switch payload.Error {
			case "stale_snapshot":
				return response.StatusCode, accounts.ErrStale
			case "admission_deadline":
				return response.StatusCode, jobs.ErrAdmissionDeadline
			}
		}
		return response.StatusCode, errInvalidRequest
	}
	if target == nil && response.StatusCode == http.StatusNoContent {
		return response.StatusCode, nil
	}
	if target == nil || response.StatusCode != http.StatusOK && response.StatusCode != http.StatusCreated {
		return response.StatusCode, fmt.Errorf("Trading API jobs request returned HTTP %d", response.StatusCode)
	}
	media, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		return response.StatusCode, errInvalidResponse
	}
	raw, err := readResponse(response.Body, maxResponseBytes)
	if err != nil || contract.Decode(raw, nil, target) != nil {
		return response.StatusCode, errInvalidResponse
	}
	return response.StatusCode, nil
}

func readResponse(body io.Reader, limit int64) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil || int64(len(raw)) > limit {
		return nil, errInvalidResponse
	}
	return raw, nil
}
