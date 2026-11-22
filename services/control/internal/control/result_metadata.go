package control

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strconv"

	"tradingbot/control/internal/contract"
)

const (
	syntheticResultVersion = "0.1"
	offlineResultVersion   = "0.2"
	identityResultVersion  = "0.3"
	providerResultVersion  = "0.4"
	syntheticSourceMode    = "synthetic_fixture"
	offlineSourceMode      = "tradingagents_offline"
	providerSourceMode     = "prepared_archive_research"
	offlineModelID         = "scripted-offline-v1"
)

var gitCommit = regexp.MustCompile(`^[0-9a-f]{40}$`)

type syntheticResultMetadata struct {
	SourceMode    string `json:"source_mode"`
	LLMCalls      int    `json:"llm_calls"`
	AlphaMeasured bool   `json:"alpha_measured"`
}

type workerResultMetadata struct {
	SourceMode          string           `json:"source_mode"`
	AlphaMeasured       bool             `json:"alpha_measured"`
	IntradayPITVerified bool             `json:"intraday_pit_verified"`
	LLMCalls            int              `json:"llm_calls"`
	InputTokens         uint64           `json:"input_tokens"`
	OutputTokens        uint64           `json:"output_tokens"`
	MissingUsageCalls   int              `json:"missing_usage_calls"`
	USDCost             *string          `json:"usd_cost"`
	Provenance          workerProvenance `json:"provenance"`
}

type workerProvenance struct {
	JobID                   string            `json:"job_id"`
	Attempt                 int               `json:"attempt"`
	PolicyVersion           string            `json:"policy_version"`
	DataManifest            manifest          `json:"data_manifest"`
	WorkerVersion           string            `json:"worker_version"`
	UpstreamVersion         string            `json:"upstream_version"`
	UpstreamSHA             string            `json:"upstream_sha"`
	ConfigSHA256            string            `json:"config_sha256"`
	ModelID                 string            `json:"model_id"`
	IdentityRegistry        *manifest         `json:"identity_registry,omitempty"`
	SecurityBindings        []securityBinding `json:"security_bindings,omitempty"`
	ProjectedSnapshotSHA256 string            `json:"projected_snapshot_sha256,omitempty"`
	ModelProvider           string            `json:"model_provider,omitempty"`
	ArchiveSourceMode       string            `json:"archive_source_mode,omitempty"`
}

func parseResultMetadata(version string, raw json.RawMessage) (any, error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || !jsonLiteral(fields["alpha_measured"], "false") {
		return nil, errInvalidResult
	}
	switch version {
	case syntheticResultVersion:
		var metadata syntheticResultMetadata
		if !hasExactKeys(fields, "source_mode", "llm_calls", "alpha_measured") ||
			!jsonLiteral(fields["llm_calls"], "0") || json.Unmarshal(raw, &metadata) != nil ||
			metadata.SourceMode != syntheticSourceMode {
			return nil, errInvalidResult
		}
		return metadata, nil
	case offlineResultVersion, identityResultVersion, providerResultVersion:
		return parseWorkerResultMetadata(version, raw, fields)
	default:
		return nil, errInvalidResult
	}
}

func parseWorkerResultMetadata(version string, raw json.RawMessage, fields map[string]json.RawMessage) (workerResultMetadata, error) {
	var metadata workerResultMetadata
	sourceMode := offlineSourceMode
	if version == providerResultVersion {
		sourceMode = providerSourceMode
	}
	if !hasExactKeys(fields, "source_mode", "alpha_measured", "intraday_pit_verified", "llm_calls",
		"input_tokens", "output_tokens", "missing_usage_calls", "usd_cost", "provenance") ||
		!jsonLiteral(fields["intraday_pit_verified"], "false") || !jsonLiteral(fields["usd_cost"], "null") ||
		json.Unmarshal(raw, &metadata) != nil || metadata.SourceMode != sourceMode ||
		metadata.LLMCalls < 1 || metadata.LLMCalls > maxLLMCalls ||
		(version == providerResultVersion && metadata.LLMCalls != 1) ||
		metadata.MissingUsageCalls < 0 || metadata.MissingUsageCalls > metadata.LLMCalls ||
		(metadata.MissingUsageCalls == metadata.LLMCalls && (metadata.InputTokens != 0 || metadata.OutputTokens != 0)) {
		return workerResultMetadata{}, errInvalidResult
	}
	// A typed JSON decoder accepts null into scalar fields. Require actual
	// integer literals for usage counters, including valid zero observations.
	for _, field := range []string{"llm_calls", "missing_usage_calls", "input_tokens", "output_tokens"} {
		if _, err := strconv.ParseUint(string(bytes.TrimSpace(fields[field])), 10, 64); err != nil {
			return workerResultMetadata{}, errInvalidResult
		}
	}
	if !validWorkerProvenance(version, fields["provenance"], metadata.Provenance) {
		return workerResultMetadata{}, errInvalidResult
	}
	return metadata, nil
}

func validWorkerProvenance(version string, raw json.RawMessage, provenance workerProvenance) bool {
	var fields, manifestFields map[string]json.RawMessage
	keys := []string{"job_id", "attempt", "policy_version", "data_manifest", "worker_version", "upstream_version",
		"upstream_sha", "config_sha256", "model_id"}
	bound := version != offlineResultVersion
	if bound {
		keys = append(keys, "identity_registry", "security_bindings", "projected_snapshot_sha256")
	}
	if version == providerResultVersion {
		keys = append(keys, "model_provider", "archive_source_mode")
		if provenance.ArchiveSourceMode != "local_archive" ||
			!contract.ValidPreparedProviderModel(provenance.ModelProvider, provenance.ModelID) {
			return false
		}
	} else if provenance.ModelID != offlineModelID {
		return false
	}
	return json.Unmarshal(raw, &fields) == nil && hasExactKeys(fields, keys...) &&
		json.Unmarshal(fields["data_manifest"], &manifestFields) == nil && hasExactKeys(manifestFields, "ref", "sha256") &&
		jobUUID.MatchString(provenance.JobID) && provenance.Attempt >= 1 && provenance.Attempt <= 3 &&
		policyVersion.MatchString(provenance.PolicyVersion) && manifestRef.MatchString(provenance.DataManifest.Ref) &&
		sha256Hex.MatchString(provenance.DataManifest.SHA256) && policyVersion.MatchString(provenance.WorkerVersion) &&
		policyVersion.MatchString(provenance.UpstreamVersion) && gitCommit.MatchString(provenance.UpstreamSHA) &&
		sha256Hex.MatchString(provenance.ConfigSHA256) &&
		(!bound || validIdentityProvenance(fields, provenance))
}

func validIdentityProvenance(fields map[string]json.RawMessage, provenance workerProvenance) bool {
	var registry map[string]json.RawMessage
	var bindings []map[string]json.RawMessage
	if !validIdentityRegistry(provenance.IdentityRegistry) || !sha256Hex.MatchString(provenance.ProjectedSnapshotSHA256) ||
		json.Unmarshal(fields["identity_registry"], &registry) != nil || !hasExactKeys(registry, "ref", "sha256") ||
		json.Unmarshal(fields["security_bindings"], &bindings) != nil || len(bindings) < 1 || len(bindings) > 100 {
		return false
	}
	for _, binding := range bindings {
		if !hasExactKeys(binding, "symbol", "security_id") {
			return false
		}
	}
	return true
}

func jsonLiteral(raw json.RawMessage, value string) bool {
	return string(bytes.TrimSpace(raw)) == value
}
