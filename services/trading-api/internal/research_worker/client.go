package research_worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"strings"
	"time"

	"tradingbot/trading-api/internal/contract"
)

const jobsPath = "/v1/research/jobs/"
const maxResponseBytes = 512 * 1024

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var workerPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// Assignment holds the immutable input and lease identity returned by Control.
type Assignment struct {
	JobID           string          `json:"job_id"`
	WorkerID        string          `json:"worker_id"`
	ClaimID         string          `json:"claim_id"`
	LeaseToken      string          `json:"lease_token"`
	Attempt         int             `json:"attempt"`
	LeaseExpiresAt  time.Time       `json:"lease_expires_at"`
	AttemptDeadline time.Time       `json:"attempt_deadline"`
	Request         json.RawMessage `json:"request,omitempty"`
}

type leaseResponse struct {
	Assignment
	Status string `json:"status"`
}

type controlClient struct {
	baseURL string
	http    *http.Client
}

func newControlClient(baseURL string, timeout time.Duration) (*controlClient, error) {
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" ||
		u.User != nil || u.ForceQuery || u.RawQuery != "" || strings.Contains(baseURL, "#") ||
		(u.Path != "" && u.Path != "/") || u.RawPath != "" {
		return nil, errors.New("control URL must be an HTTP or HTTPS origin without credentials")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// The control API is a direct service connection, not a proxy target.
	transport.Proxy = nil
	return &controlClient{
		baseURL: strings.TrimSuffix(u.String(), "/"),
		http: &http.Client{
			Transport: transport,
			Timeout:   timeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

type responseError struct {
	status    int
	message   string
	retryable bool
}

func (e *responseError) Error() string {
	if e.status != 0 {
		return fmt.Sprintf("control returned HTTP %d", e.status)
	}
	return e.message
}

func retryable(err error) bool {
	var response *responseError
	return errors.As(err, &response) && response.retryable
}

func statusIs(err error, status int) bool {
	var response *responseError
	return errors.As(err, &response) && response.status == status
}

func invalidResponse() error {
	// A response can be corrupted after Control committed the operation.
	return &responseError{message: "invalid control response", retryable: true}
}

func (c *controlClient) post(ctx context.Context, path string, body []byte, expected ...int) ([]byte, int, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	if len(body) > 256*1024 {
		return nil, 0, errors.New("control request exceeds body limit")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+jobsPath+path, bytes.NewReader(body))
	if err != nil {
		return nil, 0, errors.New("invalid control request")
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		return nil, 0, &responseError{message: "control request failed", retryable: true}
	}
	defer res.Body.Close()
	for _, status := range expected {
		if res.StatusCode == status {
			response, err := io.ReadAll(io.LimitReader(res.Body, maxResponseBytes+1))
			if err != nil || len(response) > maxResponseBytes {
				return nil, 0, invalidResponse()
			}
			return response, status, nil
		}
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 4096))
	return nil, res.StatusCode, &responseError{
		status: res.StatusCode,
		retryable: res.StatusCode == http.StatusRequestTimeout ||
			res.StatusCode == http.StatusTooManyRequests || res.StatusCode >= 500 || res.StatusCode/100 == 2,
	}
}

func (c *controlClient) claim(ctx context.Context, workerID, claimID string) (Assignment, error) {
	body, _ := json.Marshal(map[string]string{"worker_id": workerID, "claim_id": claimID})
	data, status, err := c.post(ctx, "claim", body, http.StatusOK, http.StatusNoContent)
	if err != nil || status == http.StatusNoContent {
		return Assignment{}, err
	}
	assignment, err := decodeLease(data, workerID, claimID)
	if err != nil {
		return Assignment{}, err
	}
	// The producer validates the full immutable envelope before using it. Here
	// we check that the lease is attached to the expected job/protocol.
	var request struct {
		ID      string `json:"job_id"`
		Version string `json:"job_schema_version"`
	}
	if json.Unmarshal(assignment.Request, &request) != nil || request.ID != assignment.JobID || request.Version != "0.1" {
		return Assignment{}, invalidResponse()
	}
	return assignment, nil
}

func (c *controlClient) heartbeat(ctx context.Context, a Assignment) (Assignment, error) {
	body, _ := json.Marshal(map[string]string{"worker_id": a.WorkerID, "lease_token": a.LeaseToken})
	data, _, err := c.post(ctx, a.JobID+"/heartbeat", body, http.StatusOK)
	if err != nil {
		return Assignment{}, err
	}
	r, err := decodeLease(data, a.WorkerID, a.ClaimID)
	if err != nil {
		return Assignment{}, err
	}
	if r.JobID != a.JobID || r.LeaseToken != a.LeaseToken || r.Attempt != a.Attempt ||
		!r.AttemptDeadline.Equal(a.AttemptDeadline) || r.Request != nil || r.LeaseExpiresAt.Before(a.LeaseExpiresAt) {
		return Assignment{}, invalidResponse()
	}
	a.LeaseExpiresAt = r.LeaseExpiresAt
	return a, nil
}

func decodeLease(data []byte, workerID, claimID string) (Assignment, error) {
	var r leaseResponse
	if contract.Decode(data, nil, &r) != nil || r.Status != "leased" || r.WorkerID != workerID || r.ClaimID != claimID ||
		!uuidPattern.MatchString(r.JobID) || !uuidPattern.MatchString(r.LeaseToken) || r.Attempt < 1 || r.Attempt > 3 ||
		r.LeaseExpiresAt.IsZero() || r.AttemptDeadline.IsZero() || r.LeaseExpiresAt.After(r.AttemptDeadline) {
		return Assignment{}, invalidResponse()
	}
	return r.Assignment, nil
}

func (c *controlClient) complete(ctx context.Context, a Assignment, body []byte) error {
	data, _, err := c.post(ctx, a.JobID+"/complete", body, http.StatusCreated, http.StatusOK)
	if err != nil {
		return err
	}
	var r struct {
		JobID       string          `json:"job_id"`
		Status      string          `json:"status"`
		Attempt     int             `json:"attempt"`
		CompletedAt time.Time       `json:"completed_at"`
		Result      json.RawMessage `json:"result"`
	}
	if contract.Decode(data, nil, &r) != nil || r.JobID != a.JobID || r.Status != "completed" ||
		r.Attempt != a.Attempt || r.CompletedAt.IsZero() || !sameResult(body, r.Result) {
		return invalidResponse()
	}
	return nil
}

// Control omits lease credentials and canonicalizes result object keys before
// persisting them. Compare the immutable contents without float conversion.
func sameResult(body, receipt []byte) bool {
	var sent, saved map[string]any
	sentDecoder := json.NewDecoder(bytes.NewReader(body))
	sentDecoder.UseNumber()
	savedDecoder := json.NewDecoder(bytes.NewReader(receipt))
	savedDecoder.UseNumber()
	if sentDecoder.Decode(&sent) != nil || savedDecoder.Decode(&saved) != nil ||
		sent["result_schema_version"] != "0.1" || sent["batch"] == nil || sent["reports"] == nil || sent["metadata"] == nil {
		return false
	}
	delete(sent, "worker_id")
	delete(sent, "lease_token")
	return reflect.DeepEqual(sent, saved)
}
