package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"tradingbot/control/internal/contract"
	"tradingbot/control/internal/ledger"
	"tradingbot/control/internal/paper_thesis"
	"tradingbot/control/internal/paper_workflow"
	"tradingbot/control/internal/research_scheduler"
)

// Intercept only the subprocess response. The real Go CLI has already committed
// by the time the first runner process exits without acknowledging it in SQLite.
const reviewCycleLoseAck = `
import hashlib, json, os, sqlite3, subprocess, sys
from pathlib import Path
from tradingbot import review_cycles

drop, trace, journal = sys.argv[1:4]
sys.argv[1:4] = []
run = subprocess.run

def observe(command, **kwargs):
    reply = run(command, **kwargs)
    if command[1] == 'schedule-review':
        raw = Path(command[command.index('--config') + 1]).read_bytes()
        digest = hashlib.sha256(raw).hexdigest()
        with open(trace, 'a', encoding='utf-8') as stream:
            stream.write(json.dumps({'sha256': digest, 'receipt': json.loads(reply.stdout)}) + '\n')
        if drop == 'yes':
            db = sqlite3.connect(Path(journal) / 'reviews.sqlite3')
            staged = db.execute('SELECT request, request_sha256, receipt FROM reviews WHERE position=0').fetchone()
            db.close()
            if staged != (raw, digest, None):
                raise AssertionError('first send must have exact durable bytes and no receipt')
            os._exit(70)
    return reply

review_cycles.subprocess.run = observe
raise SystemExit(review_cycles.main())
`

func checkReviewCycleRestart(t *testing.T, fixture stableEntryFixture, order ledger.Order) {
	t.Helper()
	t.Run("python_review_cycles_restart", func(t *testing.T) {
		configured := os.Getenv("TRADINGBOT_IDENTITY_PREPARER_TEST_PYTHON")
		if configured == "" {
			t.Skip("TRADINGBOT_IDENTITY_PREPARER_TEST_PYTHON must point to the prepared research interpreter")
		}
		python, err := exec.LookPath(configured)
		if err != nil {
			t.Fatal("configured research interpreter is unavailable")
		}
		python, err = filepath.Abs(python)
		if err != nil {
			t.Fatal(err)
		}
		researchRoot, err := filepath.Abs("../../../research")
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		archive := t.TempDir()
		binary := filepath.Join(t.TempDir(), "paper.exe")
		build := exec.CommandContext(ctx, "go", "build", "-mod=readonly", "-o", binary, ".")
		if output, err := build.CombinedOutput(); err != nil {
			t.Fatalf("build paper for Python review runner: %v\n%s", err, output)
		}
		second, err := fixture.research.Now(ctx)
		if err != nil {
			t.Fatal(err)
		}
		second = second.Truncate(time.Microsecond)
		first := second.Add(-time.Millisecond)
		archiveFixture := fixture
		archiveFixture.dir = archive
		plan, owner := reviewCyclePlan(t, archiveFixture, order, researchRoot, first, second)
		journal, trace := filepath.Join(t.TempDir(), "journal"), filepath.Join(t.TempDir(), "acks.jsonl")
		temp := t.TempDir()
		invoke := func(drop string) ([]byte, error) {
			cmd := exec.CommandContext(ctx, python, "-c", reviewCycleLoseAck, drop, trace, journal,
				"--root", archive, "--plan-ref", plan.Ref, "--plan-sha256", plan.SHA256, "--journal", journal,
				"--paper-cli", binary, "--identity-registry-sha256", owner.Registry.SHA256)
			cmd.Dir = researchRoot
			cmd.Env = append(os.Environ(), "TRADINGBOT_DATABASE_URL="+fixture.dsn, "TMPDIR="+temp, "TMP="+temp, "TEMP="+temp)
			return cmd.CombinedOutput()
		}
		output, err := invoke("yes")
		var killed *exec.ExitError
		if !errors.As(err, &killed) || killed.ExitCode() != 70 {
			t.Fatalf("review runner failed to lose a real Go acknowledgement: %v\n%s", err, output)
		}
		lost := reviewCycleAcks(t, trace)
		if len(lost) != 1 || !lost[0].Receipt.Created || lost[0].Receipt.Status != "queued" || lost[0].Receipt.CreatedAt == nil {
			t.Fatalf("first Go call did not commit a single review: %+v", lost)
		}
		firstJob := lost[0].Receipt
		task, err := fixture.research.GetPaperDispatch(ctx, firstJob.JobID)
		var request paper_workflow.Request
		if err != nil || contract.Decode(task.Request, contract.PaperApplication, &request) != nil ||
			request.ReviewOrigin == nil || request.ReviewOrigin.OpeningOrderID != order.OrderID ||
			request.Identity == nil || request.Identity.OwnerIdentity != owner || len(request.Identity.Securities) != 1 ||
			request.Identity.Securities[0].SecurityID != fixture.securityID || request.Identity.Securities[0].Instrument != order.Instrument {
			t.Fatalf("the unacknowledged commit lost its authoritative opening proof: %+v %v", request, err)
		}
		report, err := invoke("no")
		var received struct {
			Occurrences []paper_thesis.ScheduleReceipt `json:"occurrences"`
		}
		if err != nil || json.Unmarshal(report, &received) != nil || len(received.Occurrences) != 2 {
			t.Fatalf("restarted review runner failed: %v\n%s", err, report)
		}
		acks := reviewCycleAcks(t, trace)
		if len(acks) != 3 || acks[1].SHA256 != lost[0].SHA256 || acks[2].SHA256 == lost[0].SHA256 {
			t.Fatalf("restart did not replay the frozen request followed by a separately frozen window: %+v", acks)
		}
		for i, status := range []string{"queued", "already_scheduled"} {
			for _, receipt := range []paper_thesis.ScheduleReceipt{acks[i+1].Receipt, received.Occurrences[i]} {
				if receipt.Status != status || receipt.Created || receipt.JobID != firstJob.JobID ||
					receipt.CreatedAt == nil || !receipt.CreatedAt.Equal(*firstJob.CreatedAt) {
					t.Fatalf("window %d did not acknowledge the original job: %+v", i, receipt)
				}
			}
		}
		if replay, err := invoke("no"); err != nil || !bytes.Equal(report, replay) || len(reviewCycleAcks(t, trace)) != 3 {
			t.Fatalf("journal replay after both windows were acknowledged sent a new submission: %v\n%s", err, replay)
		}
		connection, err := pgx.Connect(ctx, fixture.dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer connection.Close(ctx)
		var count int
		err = connection.QueryRow(ctx, `SELECT count(*) FROM research_jobs WHERE identity_registry_sha256 = $1`, owner.Registry.SHA256).Scan(&count)
		if err != nil || count != 1 {
			t.Fatalf("review cycle did not create exactly one durable job: %d %v", count, err)
		}
		lease, err := fixture.research.ClaimWithIdentityRegistry(ctx, "python-review-cycle-worker",
			"60000000-0000-4000-8000-000000000005", time.Hour, owner.Registry.SHA256)
		if err != nil || lease.Job.ID != firstJob.JobID || lease.Attempt != 1 {
			t.Fatalf("worker could not lease the one durable stable review: %+v %v", lease.Job, err)
		}
	})
}

type reviewCycleAck struct {
	SHA256  string                       `json:"sha256"`
	Receipt paper_thesis.ScheduleReceipt `json:"receipt"`
}

func reviewCycleAcks(t *testing.T, path string) []reviewCycleAck {
	t.Helper()
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var acks []reviewCycleAck
	for line := range bytes.SplitSeq(bytes.TrimSpace(payload), []byte("\n")) {
		var ack reviewCycleAck
		if err := json.Unmarshal(line, &ack); err != nil {
			t.Fatal(err)
		}
		acks = append(acks, ack)
	}
	return acks
}

func reviewCyclePlan(t *testing.T, fixture stableEntryFixture, order ledger.Order, researchRoot string, first, second time.Time) (paper_workflow.ArchiveReference, paper_workflow.OwnerIdentity) {
	t.Helper()
	pin := func(name string, document any) paper_workflow.ArchiveReference {
		raw := encode(t, document)
		return paper_workflow.ArchiveReference{Ref: filepath.Base(saveDocument(t, fixture.dir, name, raw)), SHA256: digestHex(raw)}
	}
	const provider = "review-cycle-fixture"
	event := research_scheduler.EventReference{Kind: "news", Provider: provider, RecordID: "cycle-event"}
	base := reviewPreparerArchive(t, fixture, researchRoot, first)
	raw, err := os.ReadFile(filepath.Join(fixture.dir, base.Ref))
	if err != nil {
		t.Fatal(err)
	}
	var market struct {
		Version string           `json:"schema_version"`
		Source  string           `json:"source_mode"`
		Records []map[string]any `json:"records"`
	}
	if err := json.Unmarshal(raw, &market); err != nil {
		t.Fatal(err)
	}
	market.Records = append(market.Records, map[string]any{"kind": event.Kind, "provider": event.Provider, "record_id": event.RecordID,
		"symbol": "TSLA", "revision": 1, "event_time": first, "available_at": first, "ingested_at": first,
		"payload": map[string]any{"security_id": fixture.securityID, "headline": "Synthetic held issuer catalyst"}})
	manifest := pin("cycle-market", market)
	registry := pin("cycle-registry", map[string]any{"schema_version": "0.1", "universe_provider": "fixture-roster",
		"entries": []any{map[string]any{"symbol": "TSLA", "security_id": fixture.securityID, "instrument": order.Instrument,
			"available_at": first.Add(-time.Minute), "effective_at": first.Add(-time.Minute), "expires_at": first.Add(time.Hour),
			"record_providers": []string{"fixture-market", provider}}}})
	owner := paper_workflow.OwnerIdentity{Registry: registry, QuoteTape: pin("cycle-tape", map[string]any{
		"paper_quote_tape_schema_version": "0.3", "source_mode": "synthetic_fixture", "account_id": fixture.accountID,
		"identity_registry_sha256": registry.SHA256, "observations": []any{},
	})}
	policy := pin("cycle-policy", fixture.config.Context.Policy)
	marks := pin("cycle-marks", fixture.config.Context.Marks)
	window := func(cutoff time.Time) map[string]any {
		return map[string]any{"opening_order_id": order.OrderID, "as_of": cutoff, "release_at": cutoff,
			"max_lateness_seconds": 60, "max_llm_calls": 3, "data_manifest": manifest, "marks": marks, "event": event}
	}
	return pin("owner-cycle-plan", map[string]any{"schema_version": "0.1", "account_id": fixture.accountID,
		"schedule_id": "synthetic-stable-review-cycle", "policy": policy, "identity_registry": registry,
		"quote_tape": owner.QuoteTape, "occurrences": []any{window(first), window(second)},
	}), owner
}
