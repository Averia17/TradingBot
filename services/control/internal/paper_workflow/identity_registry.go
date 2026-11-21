package paper_workflow

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"time"
	"unicode/utf8"

	"tradingbot/control/internal/contract"
	"tradingbot/control/internal/jobs"
	"tradingbot/control/internal/ledger"
	"tradingbot/control/internal/paper_quotes"
	"tradingbot/control/internal/research_scheduler"
)

const maxIdentityRegistryBytes = 16 * 1024 * 1024

type identityRegistry struct {
	Version          string          `json:"schema_version"`
	UniverseProvider string          `json:"universe_provider"`
	Entries          []registryEntry `json:"entries"`
}

type registryEntry struct {
	ResearchSecurityBinding
	AvailableAt     time.Time          `json:"available_at"`
	EffectiveAt     time.Time          `json:"effective_at"`
	ExpiresAt       *time.Time         `json:"expires_at"`
	RecordProviders []string           `json:"record_providers"`
	SECCIK          *string            `json:"sec_cik"`
	Instrument      *ledger.Instrument `json:"instrument"`
}

func (entry registryEntry) activeAt(at time.Time) bool {
	return !entry.AvailableAt.After(at) && !entry.EffectiveAt.After(at) &&
		(entry.ExpiresAt == nil || entry.ExpiresAt.After(at))
}

func readIdentityRegistry(root *os.Root, reference ArchiveReference) (identityRegistry, error) {
	var registry identityRegistry
	info, err := root.Stat(reference.Ref)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxIdentityRegistryBytes {
		return registry, errors.New("paper identity registry must be a bounded regular file")
	}
	file, err := root.Open(reference.Ref)
	if err != nil {
		return registry, errors.New("open paper identity registry failed")
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, maxIdentityRegistryBytes+1))
	if err != nil || len(raw) > maxIdentityRegistryBytes {
		return registry, errors.New("read paper identity registry failed")
	}
	sum := sha256.Sum256(raw)
	if !utf8.Valid(raw) || hex.EncodeToString(sum[:]) != reference.SHA256 ||
		contract.Decode(bytes.TrimPrefix(raw, []byte{0xef, 0xbb, 0xbf}), contract.ResearchIdentityRegistry, &registry) != nil {
		return registry, fmt.Errorf("%w: registry schema or pinned SHA-256 differs", ErrInvalid)
	}
	for _, entry := range registry.Entries {
		if entry.ExpiresAt != nil && !entry.ExpiresAt.After(entry.EffectiveAt) {
			return registry, fmt.Errorf("%w: registry validity ends before its effective time", ErrInvalid)
		}
		seen := make(map[string]bool, len(entry.RecordProviders))
		for _, provider := range entry.RecordProviders {
			if seen[provider] {
				return registry, fmt.Errorf("%w: duplicated registry record provider", ErrInvalid)
			}
			seen[provider] = true
		}
	}
	return registry, nil
}

// BindOwnerIdentity compiles an independently pinned registry and tape into
// ordered security/instrument eligibility for one frozen paper snapshot.
func BindOwnerIdentity(archiveRoot string, config SnapshotContext, owner OwnerIdentity, bindings []ResearchSecurityBinding,
	projectedSHA string, asOf time.Time, event *research_scheduler.VerifiedEvent) (*IdentityBinding, error) {
	if !validArchiveReference(owner.Registry) || !validArchiveReference(owner.QuoteTape) ||
		!independentEntryRefs(owner.Registry, owner.QuoteTape) || !entryDigest.MatchString(projectedSHA) || len(bindings) == 0 ||
		asOf.IsZero() {
		return nil, fmt.Errorf("%w: invalid frozen owner paper identity", ErrInvalid)
	}
	root, err := os.OpenRoot(archiveRoot)
	if err != nil {
		return nil, errors.New("open paper identity archive root failed")
	}
	defer root.Close()
	registry, err := readIdentityRegistry(root, owner.Registry)
	if err != nil {
		return nil, err
	}
	if _, err := paper_quotes.OpenIdentityTape(root, owner.QuoteTape.Ref, owner.QuoteTape.SHA256, config.AccountID, owner.Registry.SHA256); err != nil {
		return nil, fmt.Errorf("verify owner paper quote tape: %w", err)
	}
	governed := make(map[string]ledger.Instrument, len(config.Policy.Instruments))
	for _, rule := range config.Policy.Instruments {
		governed[rule.Symbol] = rule.Instrument
	}
	bound := &IdentityBinding{OwnerIdentity: owner, ProjectedSnapshotSHA256: projectedSHA,
		Securities: make([]BoundSecurity, 0, len(bindings))}
	for _, research := range bindings {
		entry := registry.lookup(research, asOf)
		if entry == nil || entry.Instrument == nil || *entry.Instrument != governed[research.Symbol] {
			return nil, fmt.Errorf("%w: no unique active registry instrument matching paper policy for %s", ErrInvalid, research.Symbol)
		}
		if event != nil && !entry.matchesEvent(*event, asOf) {
			return nil, fmt.Errorf("%w: factual catalyst is outside the stable security binding", ErrInvalid)
		}
		bound.Securities = append(bound.Securities, BoundSecurity{Symbol: entry.Symbol, SecurityID: entry.SecurityID,
			Instrument: *entry.Instrument, ExpiresAt: entry.ExpiresAt})
	}
	return bound, nil
}

// InstrumentEligibleAt applies the owner registry to each observation cutoff.
func InstrumentEligibleAt(root *os.Root, ref ArchiveReference, binding ResearchSecurityBinding,
	instrument ledger.Instrument, cutoffs ...time.Time) (bool, error) {
	if root == nil || !validArchiveReference(ref) || len(cutoffs) == 0 {
		return false, ErrInvalid
	}
	registry, err := readIdentityRegistry(root, ref)
	if err != nil {
		return false, err
	}
	for _, cutoff := range cutoffs {
		entry := registry.lookup(binding, cutoff)
		if entry == nil || entry.Instrument == nil || *entry.Instrument != instrument {
			return false, nil
		}
		for _, prior := range registry.Entries {
			if prior.SecurityID != binding.SecurityID && prior.Instrument != nil && *prior.Instrument == instrument &&
				!prior.AvailableAt.After(cutoff) && !prior.EffectiveAt.After(cutoff) {
				return false, nil
			}
		}
	}
	return true, nil
}

// VerifyIdentityEvent validates a read-only catalyst against the pinned
// registry's same security and exact recorded holding instrument.
func VerifyIdentityEvent(archiveRoot string, ref ArchiveReference, binding ResearchSecurityBinding,
	instrument ledger.Instrument, asOf time.Time, event research_scheduler.VerifiedEvent) error {
	root, err := os.OpenRoot(archiveRoot)
	if err != nil {
		return errors.New("open paper identity archive root failed")
	}
	defer root.Close()
	if !validArchiveReference(ref) {
		return ErrInvalid
	}
	registry, err := readIdentityRegistry(root, ref)
	if err != nil {
		return err
	}
	entry := registry.lookup(binding, asOf)
	if entry == nil || entry.Instrument == nil || *entry.Instrument != instrument || !entry.matchesEvent(event, asOf) {
		return fmt.Errorf("%w: factual catalyst is outside the stable security binding", ErrInvalid)
	}
	return nil
}

func (entry registryEntry) matchesEvent(event research_scheduler.VerifiedEvent, asOf time.Time) bool {
	var payload map[string]json.RawMessage
	if event.EventTime().IsZero() || event.KnownAt().IsZero() || event.KnownAt().After(asOf) || event.Cutoff().After(asOf) ||
		json.Unmarshal(event.Payload(), &payload) != nil {
		return false
	}
	var id string
	idRaw, hasID := payload["security_id"]
	if hasID && (json.Unmarshal(idRaw, &id) != nil || id == "") {
		return false
	}
	if event.Symbol() == "" {
		return !hasID && slices.Contains([]string{"liquidity", "policy", "review"}, event.Kind())
	}
	if event.Symbol() != entry.Symbol || event.EventTime().Before(entry.EffectiveAt) {
		return false
	}
	if !slices.Contains([]string{"report", "news", "price"}, event.Kind()) {
		return false
	}
	if event.Kind() == "report" && event.Provider() == "sec-edgar" {
		var cik string
		return entry.SECCIK != nil && json.Unmarshal(payload["cik"], &cik) == nil && cik == *entry.SECCIK &&
			(!hasID || id == entry.SecurityID)
	}
	return id == entry.SecurityID && slices.Contains(entry.RecordProviders, event.Provider())
}

func (registry identityRegistry) lookup(binding ResearchSecurityBinding, asOf time.Time) *registryEntry {
	var found *registryEntry
	for i := range registry.Entries {
		entry := &registry.Entries[i]
		if !entry.activeAt(asOf) || (entry.Symbol != binding.Symbol && entry.SecurityID != binding.SecurityID) {
			continue
		}
		if found != nil || entry.ResearchSecurityBinding != binding {
			return nil
		}
		found = entry
	}
	return found
}

// Entry retries use the same durable binding even if owner archive files have
// rotated out of the local directory after the original admission.
func existingEntryIdentity(ctx context.Context, store EntryJobStore, job storedJob, config EntrySchedule, selectionSHA, ownerSHA string) (*IdentityBinding, error) {
	task, err := store.GetPaperDispatch(ctx, job.ID)
	if errors.Is(err, jobs.ErrNotFound) {
		return nil, jobs.ErrConflict
	}
	if err != nil {
		return nil, err
	}
	var saved Request
	if contract.Decode(task.Request, contract.PaperApplication, &saved) != nil || !saved.validFollow() ||
		saved.JobID != job.ID || saved.Version != config.Version || task.AccountID != config.Context.AccountID ||
		saved.EntryScheduleSHA256 != ownerSHA || saved.Identity.OwnerIdentity != *config.Identity ||
		saved.Identity.ProjectedSnapshotSHA256 != selectionSHA || len(saved.Identity.Securities) != len(job.SecurityBindings) {
		return nil, jobs.ErrConflict
	}
	for i, bound := range saved.Identity.Securities {
		if job.SecurityBindings[i] != (ResearchSecurityBinding{Symbol: bound.Symbol, SecurityID: bound.SecurityID}) {
			return nil, jobs.ErrConflict
		}
	}
	return saved.Identity, nil
}
