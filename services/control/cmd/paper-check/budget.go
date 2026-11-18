package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
)

type researchBudget struct {
	JobID                 string `json:"job_id"`
	Status                string `json:"job_status,omitempty"`
	Limit                 *int   `json:"max_llm_calls,omitempty"`
	DurablePermits        int    `json:"durable_permits"`
	UnconfirmedPermits    int    `json:"unconfirmed_permits"`
	GuaranteedRemaining   *int   `json:"guaranteed_remaining_llm_calls,omitempty"`
	LegacyBudgetUnknown   bool   `json:"legacy_budget_unknown"`
	CompletedAttempt      *int   `json:"completed_attempt,omitempty"`
	ReportedCalls         *int   `json:"reported_llm_calls,omitempty"`
	MissingTokenCallUsage *int   `json:"missing_token_call_usage,omitempty"`
}

func checkBudget(ctx context.Context, tx pgx.Tx, r *report) error {
	b := r.Research
	var request, requestHash, completion, completionHash []byte
	var currentAttempt, completionPermits, distinctRemainders, minRemainder, maxRemainder int
	err := tx.QueryRow(ctx, `SELECT j.status, j.attempt_count, j.legacy_budget_unknown, j.request, j.request_sha256,
		r.payload, r.payload_sha256, r.attempt, permits.total, permits.completed, permits.distinct_remainders,
		permits.min_remainder, permits.max_remainder
		FROM research_jobs j LEFT JOIN research_job_results r ON r.job_id=j.id
		CROSS JOIN LATERAL (
			SELECT count(*)::integer AS total,
				count(*) FILTER (WHERE p.attempt=r.attempt)::integer AS completed,
				count(DISTINCT p.remaining_llm_calls)::integer AS distinct_remainders,
				COALESCE(min(p.remaining_llm_calls),0) AS min_remainder,
				COALESCE(max(p.remaining_llm_calls),0) AS max_remainder
			FROM research_call_permits p WHERE p.job_id=j.id
		) permits WHERE j.id=$1`, b.JobID).Scan(&b.Status, &currentAttempt, &b.LegacyBudgetUnknown, &request, &requestHash,
		&completion, &completionHash, &b.CompletedAttempt, &b.DurablePermits, &completionPermits, &distinctRemainders,
		&minRemainder, &maxRemainder)
	if errors.Is(err, pgx.ErrNoRows) {
		r.add("research_job_not_found")
		return nil
	}
	if err != nil {
		return err
	}
	if b.LegacyBudgetUnknown {
		r.add("research_budget_legacy_unknown")
	}
	var input struct {
		Version string `json:"job_schema_version"`
		ID      string `json:"job_id"`
		Limit   int    `json:"max_llm_calls"`
	}
	if !matchesDigest(request, requestHash) || json.Unmarshal(request, &input) != nil || input.Version != "0.1" ||
		input.ID != b.JobID || input.Limit < 1 || input.Limit > 64 {
		r.add("research_budget_invalid")
	} else {
		b.Limit = &input.Limit
		remaining := input.Limit - b.DurablePermits
		switch {
		case remaining < 0:
			r.add("research_budget_overrun")
		case b.DurablePermits > 0 && (distinctRemainders != b.DurablePermits || minRemainder != remaining || maxRemainder != input.Limit-1):
			r.add("research_permit_sequence_mismatch")
		case !b.LegacyBudgetUnknown:
			b.GuaranteedRemaining = &remaining
		}
	}

	b.UnconfirmedPermits = b.DurablePermits
	if (b.Status == "completed") != (b.CompletedAttempt != nil) || (b.CompletedAttempt != nil && *b.CompletedAttempt != currentAttempt) {
		r.add("research_result_state_mismatch")
	} else if b.CompletedAttempt != nil {
		checkReportedUsage(r, completion, completionHash, completionPermits)
	}
	if b.UnconfirmedPermits > 0 {
		r.add("research_permit_usage_unconfirmed")
	}
	return nil
}

func checkReportedUsage(r *report, payload, hash []byte, permits int) {
	var result struct {
		Version  string `json:"result_schema_version"`
		Metadata struct {
			Calls        *int `json:"llm_calls"`
			MissingUsage *int `json:"missing_usage_calls"`
		} `json:"metadata"`
	}
	if !matchesDigest(payload, hash) || json.Unmarshal(payload, &result) != nil {
		r.add("research_result_integrity_failed")
		return
	}
	usage := result.Metadata
	if (result.Version != "0.1" && result.Version != "0.2") || usage.Calls == nil || *usage.Calls < 0 || *usage.Calls > 64 ||
		(result.Version == "0.1" && *usage.Calls != 0) ||
		(result.Version == "0.2" && (*usage.Calls == 0 || usage.MissingUsage == nil || *usage.MissingUsage < 0 || *usage.MissingUsage > *usage.Calls)) {
		r.add("research_usage_unknown")
		return
	}
	b := r.Research
	b.ReportedCalls = usage.Calls
	if result.Version == "0.2" {
		b.MissingTokenCallUsage = usage.MissingUsage
		if *usage.MissingUsage > 0 {
			r.add("research_token_usage_missing")
		}
	}
	if *usage.Calls != permits {
		r.add("research_usage_mismatch")
		return
	}
	b.UnconfirmedPermits -= permits
}

func matchesDigest(payload, hash []byte) bool {
	digest := sha256.Sum256(payload)
	return bytes.Equal(hash, digest[:])
}
