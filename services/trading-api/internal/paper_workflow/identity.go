package paper_workflow

import (
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"time"

	"tradingbot/trading-api/internal/accounts"
	"tradingbot/trading-api/internal/contract"
	"tradingbot/trading-api/internal/jobs"
	"tradingbot/trading-api/internal/research_scheduler"
)

const IdentityVersion = "0.2"

var archivePathPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,511}$`)

type ArchiveReference = research_scheduler.ArchiveReference

type OwnerIdentity struct {
	Registry  ArchiveReference `json:"identity_registry"`
	QuoteTape ArchiveReference `json:"quote_tape"`
}

type ResearchSecurityBinding struct {
	Symbol     string `json:"symbol"`
	SecurityID string `json:"security_id"`
}

type BoundSecurity struct {
	Symbol     string              `json:"symbol"`
	SecurityID string              `json:"security_id"`
	Instrument accounts.Instrument `json:"instrument"`
	ExpiresAt  *time.Time          `json:"expires_at,omitempty"`
}

type IdentityBinding struct {
	OwnerIdentity
	ProjectedSnapshotSHA256 string          `json:"projected_snapshot_sha256"`
	Securities              []BoundSecurity `json:"security_bindings"`
}

// ReviewOrigin records the exact opening verified for an owner-scheduled review.
type ReviewOrigin struct {
	OpeningOrderID           string `json:"opening_order_id"`
	OpeningApplicationSHA256 string `json:"opening_application_sha256"`
}

func (r Request) validFollow() bool {
	if r.Intent != nil || !uuidPattern.MatchString(r.JobID) {
		return false
	}
	if r.Version == Version {
		return r.Identity == nil && r.ReviewScheduleSHA256 == "" && r.ReviewOrigin == nil
	}
	entry := entryDigest.MatchString(r.EntryScheduleSHA256) && r.ReviewScheduleSHA256 == ""
	review := entryDigest.MatchString(r.ReviewScheduleSHA256) && r.EntryScheduleSHA256 == ""
	if r.ReviewOrigin != nil && (!review || !uuidPattern.MatchString(r.ReviewOrigin.OpeningOrderID) ||
		!entryDigest.MatchString(r.ReviewOrigin.OpeningApplicationSHA256)) {
		return false
	}
	return r.Version == IdentityVersion && r.Identity != nil && (entry || review) &&
		entryDigest.MatchString(r.Identity.ProjectedSnapshotSHA256) &&
		validArchiveReference(r.Identity.Registry) && validArchiveReference(r.Identity.QuoteTape)
}

func validArchiveReference(ref ArchiveReference) bool {
	if !entryDigest.MatchString(ref.SHA256) || !archivePathPattern.MatchString(ref.Ref) {
		return false
	}
	for _, part := range strings.Split(ref.Ref, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

func (r Request) matchesIdentityJob(job storedJob, portfolio contract.Portfolio) bool {
	binding := r.Identity
	boundJob := job.Version == "0.2" || (job.Version == "0.3" && job.AnalysisMode == jobs.ProviderAnalysisMode)
	if binding == nil || r.Version != IdentityVersion || !boundJob ||
		job.IdentityRegistry == nil || *job.IdentityRegistry != binding.Registry ||
		len(binding.Securities) != len(job.RequestedSymbols) || len(job.SecurityBindings) != len(binding.Securities) {
		return false
	}
	policy := make(map[string]accounts.Instrument, len(r.SnapshotContext.Policy.Instruments))
	for _, rule := range r.SnapshotContext.Policy.Instruments {
		policy[rule.Symbol] = rule.Instrument
	}
	for i, security := range binding.Securities {
		if job.RequestedSymbols[i] != security.Symbol || job.SecurityBindings[i] != (ResearchSecurityBinding{
			Symbol: security.Symbol, SecurityID: security.SecurityID,
		}) || security.SecurityID == "" || policy[security.Symbol] != security.Instrument ||
			(security.ExpiresAt != nil && !security.ExpiresAt.After(portfolio.AsOf)) {
			return false
		}
	}
	return true
}

func (r Request) matchesIdentity(job storedJob, result storedResult, portfolio contract.Portfolio) bool {
	if r.Identity == nil {
		return r.Version == Version && job.Version == "0.1" && (result.Version == "0.1" || result.Version == "0.2") &&
			job.IdentityRegistry == nil && len(job.SecurityBindings) == 0
	}
	boundResult := (job.Version == "0.2" && result.Version == "0.3") || (job.Version == "0.3" && result.Version == "0.4")
	if !boundResult || !r.matchesIdentityJob(job, portfolio) {
		return false
	}
	var metadata struct {
		Provenance struct {
			Registry  ArchiveReference          `json:"identity_registry"`
			Bindings  []ResearchSecurityBinding `json:"security_bindings"`
			Projected string                    `json:"projected_snapshot_sha256"`
		} `json:"provenance"`
	}
	return json.Unmarshal(result.Metadata, &metadata) == nil && metadata.Provenance.Registry == r.Identity.Registry &&
		metadata.Provenance.Projected == r.Identity.ProjectedSnapshotSHA256 && slices.Equal(metadata.Provenance.Bindings, job.SecurityBindings)
}

// CheckArchivedIdentityDispatch verifies a stored owner binding against its
// immutable research documents. The caller checks their durable byte hashes.
func CheckArchivedIdentityDispatch(raw []byte, job jobs.Job, receipt *jobs.Result, accountID, registrySHA string, now time.Time) bool {
	var req Request
	var input storedJob
	if contract.Decode(raw, contract.PaperApplication, &req) != nil || req.Version != IdentityVersion ||
		!req.validFollow() || req.JobID != job.ID || req.SnapshotContext.AccountID != accountID ||
		req.Identity.Registry.SHA256 != registrySHA ||
		contract.Decode(job.Request, contract.ResearchIdentityJobs, &input) != nil ||
		input.ID != job.ID || input.PolicyVersion != req.SnapshotContext.Policy.Version {
		return false
	}
	if receipt != nil {
		_, err := validateCompletedResearch(job, *receipt, req, now)
		return err == nil
	}
	portfolio, err := contract.ParsePortfolio(input.Portfolio)
	return err == nil && !portfolio.AsOf.After(job.CreatedAt) &&
		req.SnapshotContext.CheckPortfolio(portfolio) == nil && req.matchesIdentityJob(input, portfolio)
}

func (r Request) deadline(deadline time.Time) time.Time {
	deadline = capDeadlineAtMarkExpiry(r.SnapshotContext, deadline)
	if r.Identity != nil {
		for _, security := range r.Identity.Securities {
			if security.ExpiresAt != nil && security.ExpiresAt.Before(deadline) {
				deadline = *security.ExpiresAt
			}
		}
	}
	return deadline
}
