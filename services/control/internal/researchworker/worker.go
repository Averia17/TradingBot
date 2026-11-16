// Package researchworker processes leased research jobs through the Control API.
package researchworker

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

const completionAttempts = 3

var errLeaseEnded = errors.New("research lease is no longer usable")

type Config struct {
	BaseURL           string
	WorkerID          string
	PollInterval      time.Duration
	HeartbeatInterval time.Duration
	RequestTimeout    time.Duration
	Logger            *slog.Logger
}

// AnalyzeFunc returns the complete request for one assignment. It must stop on
// cancellation, so the worker can discard a lost attempt before claiming again.
type AnalyzeFunc func(context.Context, Assignment) (json.RawMessage, error)

type worker struct {
	cfg     Config
	client  *controlClient
	analyze AnalyzeFunc
}

// Run polls Control until ctx is canceled or it receives a permanent claim error.
func Run(ctx context.Context, cfg Config, analyze AnalyzeFunc) error {
	if !workerPattern.MatchString(cfg.WorkerID) || cfg.PollInterval <= 0 ||
		cfg.HeartbeatInterval <= 0 || cfg.RequestTimeout <= 0 || analyze == nil {
		return errors.New("worker requires a valid identity, positive timeouts, and an analyzer")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	client, err := newControlClient(cfg.BaseURL, cfg.RequestTimeout)
	if err != nil {
		return err
	}
	defer client.http.CloseIdleConnections()
	w := worker{cfg: cfg, client: client, analyze: analyze}
	return w.poll(ctx)
}

func (w worker) poll(ctx context.Context) error {
	claimID := newClaimID()
	for ctx.Err() == nil {
		a, err := w.client.claim(ctx, w.cfg.WorkerID, claimID)
		if ctx.Err() != nil {
			break
		}
		switch {
		case err != nil && retryable(err):
			w.cfg.Logger.Warn("research claim response unavailable", "error", err)
		case statusIs(err, http.StatusConflict):
			// A conflicted claim has a resolved outcome and cannot be reused.
			claimID = newClaimID()
		case err != nil:
			return fmt.Errorf("claim research job: %w", err)
		case a.JobID != "":
			if err := w.process(ctx, a); err != nil && ctx.Err() == nil {
				w.cfg.Logger.Warn("research attempt incomplete", "job_id", a.JobID, "attempt", a.Attempt, "error", err)
			} else if err == nil {
				w.cfg.Logger.Info("research result saved", "job_id", a.JobID, "attempt", a.Attempt)
			}
			claimID = newClaimID()
			continue
		}
		if !wait(ctx, w.cfg.PollInterval) {
			break
		}
	}
	return nil
}

func newClaimID() string {
	var random [16]byte
	rand.Read(random[:])
	random[6] = random[6]&0x0f | 0x40
	random[8] = random[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", random[:4], random[4:6], random[6:8], random[8:10], random[10:])
}

func wait(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return ctx.Err() == nil
	}
}

func (w worker) process(ctx context.Context, a Assignment) error {
	workCtx, cancel := context.WithDeadline(ctx, a.AttemptDeadline)
	defer cancel()
	a, err := w.renew(workCtx, a)
	if err != nil {
		return err
	}
	type output struct {
		body json.RawMessage
		err  error
	}
	result := make(chan output, 1)
	go func(input Assignment) {
		body, err := w.analyze(workCtx, input)
		result <- output{body, err}
	}(a)
	// Share no mutable lease state with the analyzer. All renewal and submit
	// transitions are serialized here, using the last acknowledged lease.
	heartbeat := time.NewTimer(w.untilHeartbeat(a))
	defer heartbeat.Stop()
	for {
		select {
		case <-workCtx.Done():
			<-result
			return errLeaseEnded
		case <-heartbeat.C:
			a, err = w.renew(workCtx, a)
			if err != nil {
				cancel()
				<-result
				return err
			}
			heartbeat.Reset(w.untilHeartbeat(a))
		case out := <-result:
			if out.err != nil || !json.Valid(out.body) {
				return errors.New("research analyzer failed to produce a completion request")
			}
			if workCtx.Err() != nil || !time.Now().Before(a.LeaseExpiresAt) {
				return errLeaseEnded
			}
			return w.publish(ctx, a, out.body)
		}
	}
}

func (w worker) untilHeartbeat(a Assignment) time.Duration {
	// No heartbeat can extend the lease when it already covers the attempt.
	if a.LeaseExpiresAt.Equal(a.AttemptDeadline) {
		return time.Until(a.AttemptDeadline)
	}
	return min(w.cfg.HeartbeatInterval, time.Until(a.LeaseExpiresAt)/2)
}

func (w worker) renew(ctx context.Context, a Assignment) (Assignment, error) {
	// On transport uncertainty, only the previously acknowledged expiry is
	// safe to use. Stop the analysis if no renewal arrives by that deadline.
	leaseCtx, cancel := context.WithDeadline(ctx, a.LeaseExpiresAt)
	defer cancel()
	for leaseCtx.Err() == nil {
		next, err := w.client.heartbeat(leaseCtx, a)
		if err == nil && leaseCtx.Err() == nil {
			return next, nil
		}
		if leaseCtx.Err() != nil || statusIs(err, http.StatusConflict) || statusIs(err, http.StatusNotFound) {
			break
		}
		if !retryable(err) {
			return Assignment{}, fmt.Errorf("renew research lease: %w", err)
		}
		if !wait(leaseCtx, min(w.cfg.PollInterval, w.cfg.HeartbeatInterval)) {
			break
		}
	}
	return Assignment{}, errLeaseEnded
}

func (w worker) publish(ctx context.Context, a Assignment, body []byte) error {
	// First dispatch is fenced by the last known lease. A lost response after
	// commit can be replayed by Control even after expiry, using the exact body.
	leaseCtx, cancel := context.WithDeadline(ctx, a.LeaseExpiresAt)
	err := w.client.complete(leaseCtx, a, body)
	cancel()
	for sent := 1; sent < completionAttempts && retryable(err); sent++ {
		if !wait(ctx, w.cfg.PollInterval) {
			return ctx.Err()
		}
		err = w.client.complete(ctx, a, body)
	}
	if err != nil {
		return fmt.Errorf("publish research result: %w", err)
	}
	return nil
}
