// Package jobs_http transfers durable research and paper-application data
// between the jobs owner and trusted paper processes.
package jobs_http

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"
	"time"

	"tradingbot/trading-api/internal/jobs"
)

const (
	PathPrefix       = "/internal/v1/jobs/"
	maxDocumentBytes = 256 * 1024
	maxRequestBytes  = 1024*1024 + 4096
	maxResponseBytes = 16 * 1024 * 1024
)

var (
	uuidPattern        = regexp.MustCompile("^[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$")
	digestPattern      = regexp.MustCompile("^[0-9a-f]{64}$")
	errInvalidResponse = errors.New("invalid Trading API jobs response")
	errInvalidRequest  = errors.New("invalid Trading API jobs request")
)

func validToken(token string) bool {
	return len(token) >= 32 && len(token) <= 512 && strings.IndexFunc(token, func(r rune) bool { return r <= 32 || r >= 127 }) == -1
}

// Byte slices intentionally serialize as base64. Embedded JSON can contain
// significant bytes for a persisted SHA-256 and must not be re-encoded.
type jobDocument struct {
	ID        string    `json:"job_id"`
	Request   []byte    `json:"request"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
}

func jobWire(job jobs.Job) jobDocument {
	return jobDocument{job.ID, job.Request, job.Status, job.CreatedAt}
}

func (d jobDocument) value() jobs.Job {
	return jobs.Job{ID: d.ID, Request: d.Request, Status: d.Status, CreatedAt: d.CreatedAt}
}

type resultDocument struct {
	JobID       string    `json:"job_id"`
	Attempt     int       `json:"attempt"`
	CompletedAt time.Time `json:"completed_at"`
	Payload     []byte    `json:"payload"`
}

func resultWire(result jobs.Result) resultDocument {
	return resultDocument{result.JobID, result.Attempt, result.CompletedAt, result.Payload}
}

func (d resultDocument) value() jobs.Result {
	return jobs.Result{JobID: d.JobID, Attempt: d.Attempt, CompletedAt: d.CompletedAt, Payload: d.Payload}
}

type completedPaperDocument struct {
	Job      jobDocument       `json:"job"`
	Result   resultDocument    `json:"result"`
	Dispatch *dispatchDocument `json:"dispatch,omitempty"`
}

type dispatchDocument struct {
	JobID         string `json:"job_id"`
	AccountID     string `json:"account_id"`
	Request       []byte `json:"request"`
	RequestSHA256 string `json:"request_sha256"`
}

func dispatchWire(task jobs.PaperDispatchTask) dispatchDocument {
	return dispatchDocument{task.JobID, task.AccountID, task.Request, hex.EncodeToString(task.RequestSHA256[:])}
}

func (d dispatchDocument) value() (jobs.PaperDispatchTask, error) {
	digest := sha256.Sum256(d.Request)
	if !uuidPattern.MatchString(d.JobID) || !uuidPattern.MatchString(d.AccountID) ||
		hex.EncodeToString(digest[:]) != d.RequestSHA256 {
		return jobs.PaperDispatchTask{}, errInvalidResponse
	}
	return jobs.PaperDispatchTask{
		JobID: d.JobID, RequestSHA256: digest,
		PaperDispatchInput: jobs.PaperDispatchInput{AccountID: d.AccountID, Request: d.Request},
	}, nil
}

type clockDocument struct {
	Now time.Time `json:"now"`
}

type createRequest struct {
	ID           string     `json:"job_id"`
	Request      []byte     `json:"request"`
	SnapshotID   string     `json:"snapshot_id"`
	Snapshot     []byte     `json:"snapshot"`
	AccountID    string     `json:"account_id"`
	Application  []byte     `json:"application"`
	RegistrySHA  string     `json:"registry_sha256,omitempty"`
	ReplayOnly   bool       `json:"replay_only,omitempty"`
	AdmitThrough *time.Time `json:"admit_through,omitempty"`
}

type finishRequest struct {
	AccountID     string `json:"account_id"`
	RequestSHA256 string `json:"request_sha256"`
	Status        string `json:"status"`
}
