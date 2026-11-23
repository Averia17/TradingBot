package paper_workflow

import (
	"context"
	"errors"
	"testing"
	"time"

	"tradingbot/trading-api/internal/accounts"
	"tradingbot/trading-api/internal/contract"
	"tradingbot/trading-api/internal/paper_risk"
	"tradingbot/trading-api/internal/paper_sim"
)

type activityLedger struct {
	*ledgerStub
	activity accounts.RiskActivity
}

func (s activityLedger) RiskActivity(_ context.Context, accountID string, version int64) (accounts.RiskActivity, error) {
	if accountID != s.activity.AccountID || version != s.activity.AccountVersion {
		return accounts.RiskActivity{}, accounts.ErrStale
	}
	return s.activity, nil
}

func TestNewPaperOrderReadsAccountActivityWhenPolicyRequiresIt(t *testing.T) {
	_, paper, req, quote, now := approveFixture(t, false)
	req.SnapshotContext.Policy.MaxDailyTurnoverUSD = "100"
	req.SnapshotContext.Policy.BlockNewBuysOnUnknown = true
	var decoded Request
	if err := contract.Decode(marshalFixture(t, req), contract.PaperApplication, &decoded); err != nil {
		t.Fatal(err)
	}
	cfg := decoded.SnapshotContext
	portfolio, err := Snapshot(paper.account, cfg, cfg.Marks[0].At)
	if err != nil {
		t.Fatal(err)
	}
	preview, err := paper_sim.Preview(*req.Intent, quote, now)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := checkNewOrder(ctx, paper, cfg, portfolio, *req.Intent, preview, now); !errors.Is(err, paper_risk.ErrSnapshot) {
		t.Fatalf("missing authoritative activity = %v", err)
	}
	withActivity := activityLedger{ledgerStub: paper, activity: accounts.RiskActivity{
		AccountID: paper.account.ID, AccountVersion: paper.account.Version, DayUTC: now.UTC().Truncate(24 * time.Hour),
		SettledGrossUSD: "0.00000001",
	}}
	if err := checkNewOrder(ctx, withActivity, cfg, portfolio, *req.Intent, preview, now); !errors.Is(err, paper_risk.ErrLimit) {
		t.Fatalf("daily turnover above limit = %v", err)
	}
	withActivity.activity.SettledGrossUSD = "0"
	withActivity.activity.HasUnknown = true
	if err := checkNewOrder(ctx, withActivity, cfg, portfolio, *req.Intent, preview, now); !errors.Is(err, paper_risk.ErrLimit) {
		t.Fatalf("account with unknown order = %v", err)
	}
	withActivity.activity.HasUnknown = false
	if err := checkNewOrder(ctx, withActivity, cfg, portfolio, *req.Intent, preview, now); err != nil {
		t.Fatalf("within daily turnover limit = %v", err)
	}
	withActivity.activity.AccountVersion++
	if err := checkNewOrder(ctx, withActivity, cfg, portfolio, *req.Intent, preview, now); !errors.Is(err, accounts.ErrStale) {
		t.Fatalf("account version advanced = %v", err)
	}
	paper.order.OrderID = preview.OrderID
	if err := checkNewOrder(ctx, paper, cfg, portfolio, *req.Intent, preview, now.Add(time.Hour)); err != nil {
		t.Fatalf("existing reservation continues to ledger replay = %v", err)
	}
}
