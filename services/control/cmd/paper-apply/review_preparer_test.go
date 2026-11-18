package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"tradingbot/control/internal/contract"
	"tradingbot/control/internal/ledger"
	"tradingbot/control/internal/paperapply"
	"tradingbot/control/internal/paperthesis"
)

func checkIdentityReviewPreparer(t *testing.T, fixture stableEntryFixture, order ledger.Order) {
	t.Helper()
	t.Run("python_review_preparer", func(t *testing.T) {
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
		binary := filepath.Join(t.TempDir(), "paper-apply.exe")
		build := exec.CommandContext(fixture.ctx, "go", "build", "-mod=readonly", "-o", binary, ".")
		if output, err := build.CombinedOutput(); err != nil {
			t.Fatalf("build paper-apply for Python preparation: %v\n%s", err, output)
		}

		cutoff := fixture.clock().Truncate(time.Microsecond)
		archive := reviewPreparerArchive(t, fixture, researchRoot, cutoff)
		owner := fixture.owner("prepared-review-tape", fixture.registry("prepared-review-registry", fixture.instrument), fixture.accountID)
		policy := saveDocument(t, fixture.dir, "prepared-review-policy", encode(t, fixture.config.Context.Policy))
		marks := saveDocument(t, fixture.dir, "prepared-review-marks", encode(t, fixture.config.Context.Marks))
		account, err := fixture.paper.GetAccount(fixture.ctx, fixture.accountID)
		if err != nil {
			t.Fatal(err)
		}
		prepare := exec.CommandContext(fixture.ctx, python, "-c", "from tradingbot.cli import main; raise SystemExit(main())",
			"prepare-identity-review", "--paper-apply", binary, "--archive-root", fixture.dir,
			"--account", fixture.accountID, "--order", order.OrderID, "--as-of", cutoff.Format(time.RFC3339Nano),
			"--policy", policy, "--marks", marks,
			"--data-manifest-ref", archive.Ref, "--data-manifest-sha256", archive.SHA256,
			"--identity-registry-ref", owner.Registry.Ref, "--identity-registry-sha256", owner.Registry.SHA256,
			"--quote-tape-ref", owner.QuoteTape.Ref, "--quote-tape-sha256", owner.QuoteTape.SHA256,
			"--max-llm-calls", "3")
		prepare.Dir = researchRoot
		prepare.Env = append(os.Environ(), "TRADINGBOT_DATABASE_URL="+fixture.dsn)
		var stderr bytes.Buffer
		prepare.Stderr = &stderr
		payload, err := prepare.Output()
		if err != nil {
			t.Fatalf("Python could not prepare a review from actual Go receipts: %v\n%s", err, stderr.Bytes())
		}

		var request paperthesis.ScheduleRequest
		if err := contract.Decode(payload, contract.PaperReviewSchedule, &request); err != nil {
			t.Fatalf("prepared request is incompatible with the Go wire contract: %v", err)
		}
		if request.Version != paperapply.IdentityVersion || request.Identity == nil || request.Identity.OwnerIdentity != owner ||
			request.Context.AccountID != fixture.accountID || request.Context.SnapshotVersion != account.Version ||
			request.OpeningOrderID != order.OrderID || request.Manifest != archive || request.Budget != 3 ||
			request.Portfolio == nil || !request.Portfolio.AsOf.Equal(cutoff) || request.Context.CheckPortfolio(*request.Portfolio) != nil {
			t.Fatal("prepared request did not preserve the actual Go portfolio and owner bindings")
		}
		path := saveDocument(t, fixture.dir, "python-prepared-review", payload)
		var result bytes.Buffer
		if err := run(fixture.ctx, fixture.dsn, []string{"schedule-review", "--config", path, "--archive-root", fixture.dir,
			"--identity-registry-sha256", owner.Registry.SHA256}, &result, fixture.clock); err != nil {
			t.Fatalf("Go schedule-review rejected Python's exact bytes: %v", err)
		}
		var receipt paperthesis.ScheduleReceipt
		if err := json.Unmarshal(result.Bytes(), &receipt); err != nil || receipt.Version != paperapply.IdentityVersion ||
			receipt.Status != "not_due" || receipt.Created || receipt.JobID != "" {
			t.Fatalf("unexpected clock review receipt: %s, %v", result.Bytes(), err)
		}
		if after, err := fixture.paper.GetAccount(fixture.ctx, fixture.accountID); err != nil ||
			after.Version != account.Version || after.CashUSD != account.CashUSD || after.ReservedCashUSD != account.ReservedCashUSD {
			t.Fatalf("preparing a stable review changed the paper account: %v", err)
		}
	})
}

func reviewPreparerArchive(t *testing.T, fixture stableEntryFixture, researchRoot string, cutoff time.Time) paperapply.ArchiveReference {
	t.Helper()
	input, err := os.ReadFile(filepath.Join(researchRoot, "examples", "selection", "identity-market.json"))
	if err != nil {
		t.Fatal(err)
	}
	var example struct {
		Records []map[string]any `json:"records"`
	}
	if err := json.Unmarshal(input, &example); err != nil {
		t.Fatal(err)
	}
	var selected []map[string]any
	var listing, bar bool
	for _, record := range example.Records {
		payload, ok := record["payload"].(map[string]any)
		if !ok || record["symbol"] != "TSLA" || payload["security_id"] != "fixture-current" {
			continue
		}
		at := cutoff.Add(-time.Duration(len(selected)+1) * time.Millisecond)
		switch record["kind"] {
		case "listing":
			if payload["status"] != "listed" {
				continue
			}
			listing = true
			record["provider"] = "fixture-roster"
			payload["effective_at"] = at
			payload["source_sha256"] = digestHex(input)
		case "bar":
			bar = true
			record["provider"] = "fixture-market"
		default:
			continue
		}
		record["event_time"], record["available_at"], record["ingested_at"] = at, at, at
		payload["security_id"] = fixture.securityID
		selected = append(selected, record)
	}
	if !listing || !bar {
		t.Fatal("example archive must supply the current issuer's listing and bars")
	}
	raw := encode(t, map[string]any{"schema_version": "0.1", "source_mode": "synthetic_fixture", "records": selected})
	path := saveDocument(t, fixture.dir, "prepared-review-archive", raw)
	return paperapply.ArchiveReference{Ref: filepath.Base(path), SHA256: digestHex(raw)}
}
