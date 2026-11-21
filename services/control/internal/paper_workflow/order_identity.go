package paper_workflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"tradingbot/control/internal/contract"
	"tradingbot/control/internal/ledger"
	"tradingbot/control/internal/paper_sim"
)

type orderIdentity struct {
	Symbol            string            `json:"symbol"`
	SecurityID        string            `json:"security_id"`
	Instrument        ledger.Instrument `json:"instrument"`
	Registry          ArchiveReference  `json:"identity_registry"`
	ApplicationSHA256 string            `json:"application_sha256"`
	Decision          contract.Intent   `json:"-"`
}

// OpeningIdentity locates a position's original owner-approved virtual BUY.
// Decision is available to thesis projection without changing the wire proof.
type OpeningIdentity orderIdentity

// ExitIdentity identifies the exact owner-approved virtual SELL application.
type ExitIdentity orderIdentity

type applicationReader interface {
	GetApplication(context.Context, string) (ledger.ApplicationRecord, error)
}

// ReadOpeningIdentity verifies immutable opening evidence by exact order,
// even after a later settlement or rotation of the active registry and tape.
func ReadOpeningIdentity(ctx context.Context, research boundResearchStore,
	paper applicationReader, order ledger.Order, now time.Time) (OpeningIdentity, error) {
	identity, err := readOrderIdentity(ctx, research, paper, order, now, "BUY")
	return OpeningIdentity(identity), err
}

// ReadExitIdentity proves that an exit order's accepted research and exact
// owner quote referred to one security. Callers still match its actual fills.
func ReadExitIdentity(ctx context.Context, research boundResearchStore,
	paper applicationReader, order ledger.Order, now time.Time) (ExitIdentity, error) {
	identity, err := readOrderIdentity(ctx, research, paper, order, now, "SELL")
	return ExitIdentity(identity), err
}

func readOrderIdentity(ctx context.Context, research boundResearchStore, paper applicationReader,
	order ledger.Order, now time.Time, side string) (orderIdentity, error) {
	if order.Side != side || !uuidPattern.MatchString(order.ResearchJobID) ||
		order.CreatedAt.IsZero() || order.CreatedAt.After(now) {
		return orderIdentity{}, ErrInvalid
	}
	task, err := research.GetPaperDispatch(ctx, order.ResearchJobID)
	if err != nil {
		return orderIdentity{}, fmt.Errorf("read owner order binding: %w", err)
	}
	var req Request
	if contract.Decode(task.Request, contract.PaperApplication, &req) != nil || req.Version != IdentityVersion ||
		!req.validFollow() || req.JobID != order.ResearchJobID || task.JobID != req.JobID ||
		req.SnapshotContext.AccountID != order.AccountID || task.AccountID != order.AccountID ||
		req.SnapshotContext.SnapshotVersion != order.SnapshotVersion || sha256.Sum256(task.Request) != task.RequestSHA256 {
		return orderIdentity{}, ErrInvalid
	}
	record, err := paper.GetApplication(ctx, req.JobID)
	if err != nil {
		return orderIdentity{}, fmt.Errorf("read owner order application: %w", err)
	}
	if record.AccountID != order.AccountID || record.SnapshotVersion != order.SnapshotVersion ||
		record.RequestSHA256 != task.RequestSHA256 || record.OrderID != order.OrderID {
		return orderIdentity{}, ledger.ErrConflict
	}
	confirmed, err := completedFollow(record, IdentityVersion)
	if err != nil {
		return orderIdentity{}, err
	}
	execution := confirmed.Application.Execution
	if execution == nil || execution.Version != paper_sim.IdentityQuoteVersion ||
		execution.Source != paper_sim.Source || execution.Order.ReservationRequest != order.ReservationRequest ||
		!execution.Order.CreatedAt.Equal(order.CreatedAt) {
		return orderIdentity{}, ErrInvalid
	}
	job, err := research.Get(ctx, req.JobID)
	if err != nil {
		return orderIdentity{}, err
	}
	result, err := research.GetResult(ctx, req.JobID)
	if err != nil {
		return orderIdentity{}, err
	}
	if job.Status != "completed" || job.ID != req.JobID {
		return orderIdentity{}, ErrInvalid
	}
	application, err := validateCompletedResearch(job, result, req, order.CreatedAt)
	if err != nil {
		return orderIdentity{}, err
	}
	prepared, err := application.prepareOrder(record.Quote, order.CreatedAt)
	if err != nil {
		return orderIdentity{}, err
	}
	intent := prepared.intent
	if prepared.preview.OrderID != order.OrderID || intent.DecisionID != order.DecisionID || intent.Side != side ||
		intent.Instrument != order.Instrument || intent.SecurityID != execution.SecurityID ||
		intent.IdentityRegistrySHA256 != execution.IdentityRegistrySHA256 {
		return orderIdentity{}, ErrInvalid
	}
	for _, decision := range application.batch.Intents {
		if strings.EqualFold(decision.ID, order.DecisionID) && identityActionMatches(side, decision.Action) {
			return orderIdentity{
				Symbol: decision.Symbol, SecurityID: intent.SecurityID, Instrument: order.Instrument,
				Registry: req.Identity.Registry, ApplicationSHA256: orderApplicationDigest(record), Decision: decision,
			}, nil
		}
	}
	return orderIdentity{}, ErrInvalid
}

func identityActionMatches(side, action string) bool {
	return side == "BUY" && (action == "BUY" || action == "ADD") ||
		side == "SELL" && (action == "SELL" || action == "REDUCE")
}

func orderApplicationDigest(record ledger.ApplicationRecord) string {
	// The three inputs are fixed-length SHA-256 digests from the application.
	quote, receipt := sha256.Sum256(record.Quote), sha256.Sum256(record.Result)
	hash := sha256.New()
	hash.Write(record.RequestSHA256[:])
	hash.Write(quote[:])
	hash.Write(receipt[:])
	return hex.EncodeToString(hash.Sum(nil))
}
