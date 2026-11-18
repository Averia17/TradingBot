package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tradingbot/control/internal/contract"
	"tradingbot/control/internal/jobs"
	"tradingbot/control/internal/ledger"
	"tradingbot/control/internal/paperapply"
	"tradingbot/control/internal/paperrisk"
)

type stableEntryFixture struct {
	ctx          context.Context
	dsn          string
	clock        func() time.Time
	research     *jobs.Store
	paper        *ledger.Store
	dir          string
	accountID    string
	otherAccount string
	securityID   string
	instrument   ledger.Instrument
	manifest     paperapply.ArchiveReference
	config       paperapply.EntrySchedule
	portfolio    contract.Portfolio
	projected    string
	expiry       time.Time
	owner        func(string, paperapply.ArchiveReference, string) paperapply.OwnerIdentity
	registry     func(string, ledger.Instrument) paperapply.ArchiveReference
	invoke       func(paperapply.EntrySchedule, contract.Portfolio, string, string) (paperapply.EntryReceipt, error)
}

func newStableEntryFixture(t *testing.T, observations func(time.Time, ledger.Instrument, string, string) []any) stableEntryFixture {
	t.Helper()
	const accountID = "11111111-1111-4111-8111-111111111111"
	const otherAccountID = "22222222-2222-4222-8222-222222222222"
	const securityID = "current-equity-id"
	// This fixture includes scheduled wall-clock observations and subprocess restarts.
	ctx, dsn, clock := isolatedDatabaseWithTimeout(t, 45*time.Second)
	research, err := jobs.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(research.Close)
	paper, err := ledger.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(paper.Close)
	account, _, err := paper.CreateAccount(ctx, ledger.CreateAccount{ID: accountID, InitialCashUSD: "1000"})
	if err != nil {
		t.Fatal(err)
	}
	at := clock()
	instrument := ledger.Instrument{ID: "fixture:TSLA", RegistryVersion: "fixture-v1", Decimals: 2}
	marksAt := at.Add(-time.Second)
	snapshot := paperapply.SnapshotContext{AccountID: accountID, SnapshotVersion: account.Version,
		Policy: paperrisk.Policy{Version: "stable-entry-fixture", MaxOrderUSD: "200", MinFreeCashUSD: "10",
			MaxPositionBPS: 2500, MaxGroupBPS: 5000, MaxGrossBPS: 8000, MaxFeeBPS: 50, MaxMarkAgeSeconds: 600,
			Instruments: []paperrisk.InstrumentRule{{Symbol: "TSLA", Group: "stocks", Instrument: instrument}}},
		Marks: []paperrisk.Mark{{Instrument: instrument, PriceUSD: "20", At: marksAt, AvailableAt: marksAt}}}
	portfolio, err := paperapply.Snapshot(account, snapshot, at)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	pin := func(name string, document any) paperapply.ArchiveReference {
		data := encode(t, document)
		path := saveDocument(t, dir, name, data)
		digest := sha256.Sum256(data)
		return paperapply.ArchiveReference{Ref: filepath.Base(path), SHA256: hex.EncodeToString(digest[:])}
	}
	manifest := pin("market", map[string]any{"schema_version": "0.1", "source_mode": "synthetic_fixture", "records": []any{}})
	expiry := at.Add(time.Minute)
	registry := func(name string, selectedInstrument ledger.Instrument) paperapply.ArchiveReference {
		row := map[string]any{"symbol": "TSLA", "security_id": securityID, "instrument": selectedInstrument,
			"effective_at": at.Add(-time.Minute), "available_at": at.Add(-time.Minute), "expires_at": expiry, "record_providers": []string{"fixture-market"}}
		return pin(name, map[string]any{"schema_version": "0.1", "universe_provider": "fixture-roster", "entries": []map[string]any{row}})
	}
	owner := func(name string, ref paperapply.ArchiveReference, tapeAccount string) paperapply.OwnerIdentity {
		rows := []any{}
		if observations != nil {
			rows = observations(at, instrument, securityID, ref.SHA256)
		}
		tape := pin(name, map[string]any{"paper_quote_tape_schema_version": "0.3", "source_mode": "synthetic_fixture",
			"account_id": tapeAccount, "identity_registry_sha256": ref.SHA256, "observations": rows})
		return paperapply.OwnerIdentity{Registry: ref, QuoteTape: tape}
	}
	projected := strings.Repeat("8", 64)
	identity := owner("owner-tape", registry("owner-registry", instrument), accountID)
	binding := []paperapply.ResearchSecurityBinding{{Symbol: "TSLA", SecurityID: securityID}}
	config := paperapply.EntrySchedule{Version: paperapply.IdentityVersion, ID: "stable-entry", At: at,
		MaxLateness: 90, Budget: 20, Context: snapshot, Identity: &identity}
	inputPaths := func(c paperapply.EntrySchedule, book contract.Portfolio, projectedSHA string) (string, string) {
		raw := encode(t, map[string]any{"selection_schema_version": "0.2", "source_mode": "synthetic_fixture",
			"snapshot_sha256": projectedSHA, "stockstats_version": "fixture", "candidates": []any{}, "job": map[string]any{
				"job_schema_version": "0.2", "job_id": otherAccountID, "portfolio": book, "requested_symbols": []string{"TSLA"},
				"data_manifest": manifest, "policy_version": snapshot.Policy.Version, "max_llm_calls": 10,
				"identity_registry": c.Identity.Registry, "security_bindings": binding,
			}})
		digest := sha256.Sum256(raw)
		c.SelectionSHA256 = hex.EncodeToString(digest[:])
		return saveDocument(t, dir, "entry-selection", raw), saveDocument(t, dir, "entry-config", encode(t, c))
	}
	invoke := func(c paperapply.EntrySchedule, book contract.Portfolio, proof, trustedPin string) (paperapply.EntryReceipt, error) {
		selectionPath, configPath := inputPaths(c, book, proof)
		var stdout bytes.Buffer
		err := run(ctx, dsn, []string{"schedule-entry", "--config", configPath, "--selection", selectionPath, "--archive-root", dir,
			"--identity-registry-sha256", trustedPin}, &stdout, clock)
		if err != nil {
			return paperapply.EntryReceipt{}, err
		}
		var out paperapply.EntryReceipt
		err = json.Unmarshal(stdout.Bytes(), &out)
		return out, err
	}
	return stableEntryFixture{ctx: ctx, dsn: dsn, clock: clock, research: research, paper: paper, dir: dir, accountID: accountID,
		otherAccount: otherAccountID, securityID: securityID, instrument: instrument, config: config, portfolio: portfolio,
		projected: projected, manifest: manifest, expiry: expiry, owner: owner, registry: registry, invoke: invoke}
}

func (f stableEntryFixture) queue(t *testing.T) paperapply.EntryReceipt {
	t.Helper()
	first, err := f.invoke(f.config, f.portfolio, f.projected, f.config.Identity.Registry.SHA256)
	if err != nil || !first.Created || first.Status != "queued" || first.Version != paperapply.IdentityVersion {
		t.Fatalf("stable entry not queued: %+v %v", first, err)
	}
	return first
}

func TestStableEntryRequiresOwnerProofAndReplaysDurably(t *testing.T) {
	f := newStableEntryFixture(t, nil)
	config, portfolio, projected, invoke := f.config, f.portfolio, f.projected, f.invoke
	identity := *config.Identity
	if _, err := invoke(config, portfolio, projected, strings.Repeat("c", 64)); !errors.Is(err, paperapply.ErrInvalid) {
		t.Fatalf("wrong owner pin accepted: %v", err)
	}
	account, err := f.paper.GetAccount(f.ctx, f.accountID)
	if err != nil {
		t.Fatal(err)
	}
	earlier, err := paperapply.Snapshot(account, config.Context, config.At.Add(-time.Microsecond))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := invoke(config, earlier, projected, identity.Registry.SHA256); !errors.Is(err, paperapply.ErrInvalid) {
		t.Fatalf("stable entry accepted a different portfolio cutoff: %v", err)
	}
	wrong := config
	wrongIdentity := f.owner("foreign-tape", identity.Registry, f.otherAccount)
	wrong.Identity = &wrongIdentity
	if _, err := invoke(wrong, portfolio, projected, identity.Registry.SHA256); err == nil || !strings.Contains(err.Error(), "requested job or account") {
		t.Fatalf("foreign account tape accepted: %v", err)
	}
	wrongInstrument := f.instrument
	wrongInstrument.Decimals = 3
	wrongIdentity = f.owner("wrong-instrument-tape", f.registry("wrong-instrument-registry", wrongInstrument), f.accountID)
	if _, err := invoke(wrong, portfolio, projected, wrongIdentity.Registry.SHA256); !errors.Is(err, paperapply.ErrInvalid) {
		t.Fatalf("different registry instrument precision accepted: %v", err)
	}
	first := f.queue(t)
	task, err := f.research.GetPaperDispatch(f.ctx, first.JobID)
	if err != nil {
		t.Fatal(err)
	}
	var application paperapply.Request
	if err := contract.Decode(task.Request, contract.PaperApplication, &application); err != nil || application.Identity == nil ||
		application.Identity.OwnerIdentity != identity || application.Identity.ProjectedSnapshotSHA256 != projected ||
		len(application.Identity.Securities) != 1 {
		t.Fatalf("missing exact stable dispatch proof: %+v %v", application.Identity, err)
	}
	bound := application.Identity.Securities[0]
	if bound.Instrument != f.instrument || bound.SecurityID != f.securityID || bound.ExpiresAt == nil || !bound.ExpiresAt.Equal(f.expiry) {
		t.Fatalf("wrong persisted instrument eligibility: %+v", bound)
	}
	if _, err := invoke(config, portfolio, strings.Repeat("9", 64), identity.Registry.SHA256); !errors.Is(err, jobs.ErrConflict) {
		t.Fatalf("changed projected proof replaced the old admission: %v", err)
	}
	for _, ref := range []paperapply.ArchiveReference{f.manifest, identity.Registry, identity.QuoteTape} {
		if err := os.Remove(filepath.Join(f.dir, ref.Ref)); err != nil {
			t.Fatal(err)
		}
	}
	replay, err := invoke(config, portfolio, projected, identity.Registry.SHA256)
	if err != nil || replay.Created || replay.JobID != first.JobID || !replay.CreatedAt.Equal(*first.CreatedAt) {
		t.Fatalf("stable entry did not replay its original PG binding: %+v %v", replay, err)
	}
}
