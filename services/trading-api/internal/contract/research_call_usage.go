package contract

import (
	_ "embed"
	"errors"
	"regexp"
	"time"
)

//go:embed research-llm-usage.schema.json
var researchLLMUsageSchema []byte

//go:embed research-call-usage.schema.json
var researchCallUsageSchema []byte

var ResearchLLMCallSchema = compile(researchLLMUsageSchema, "#/$defs/LLMCall")
var linkedResearchLLMUsageSchema = schemaResource{
	url: "https://tradingbot.local/research-llm-usage.schema.json", data: researchLLMUsageSchema,
}
var ResearchCallUsageRequest = compile(researchCallUsageSchema, "", linkedResearchLLMUsageSchema)
var ResearchCallUsageReceipt = compile(researchCallUsageSchema, "#/$defs/Receipt", linkedResearchLLMUsageSchema)

var researchCallID = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)
var researchModelID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

type ResearchLLMCall struct {
	RunID        string    `json:"run_id"`
	Provider     *string   `json:"provider"`
	Model        *string   `json:"model"`
	Role         string    `json:"role"`
	Status       string    `json:"status"`
	StartedAt    time.Time `json:"started_at"`
	InputTokens  *int      `json:"input_tokens"`
	OutputTokens *int      `json:"output_tokens"`
	CacheRead    *int      `json:"cache_read_tokens"`
	CacheWrite   *int      `json:"cache_write_tokens"`
	Reasoning    *int      `json:"reasoning_tokens"`
	UsageSource  string    `json:"usage_source"`
}

// ValidateOfflineCall rejects unsupported identities, nonterminal callbacks and
// inconsistencies between the inclusive totals and their nullable subsets.
func (call ResearchLLMCall) ValidateOfflineCall() error {
	if (call.Provider == nil) != (call.Model == nil) ||
		(call.Provider != nil && (*call.Provider != "offline" || *call.Model != "scripted-offline-v1")) {
		return errors.New("invalid offline research model identity")
	}
	return call.validateTerminal()
}

func (call ResearchLLMCall) ValidateProviderCall() error {
	if call.Provider == nil || call.Model == nil ||
		!ValidPreparedProviderModel(*call.Provider, *call.Model) ||
		call.Role != "portfolio_allocator" {
		return errors.New("invalid prepared research model identity")
	}
	return call.validateTerminal()
}

func ValidPreparedProviderModel(provider, model string) bool {
	return (provider == "chatgpt" || provider == "openai") &&
		researchModelID.MatchString(model) && model != "scripted-offline-v1"
}

func (call ResearchLLMCall) validateTerminal() error {
	_, offset := call.StartedAt.Zone()
	if !researchCallID.MatchString(call.RunID) || call.StartedAt.IsZero() || offset != 0 ||
		(call.Status != "completed" && call.Status != "failed") ||
		(call.Role != "research" && call.Role != "portfolio_allocator" && call.Role != "unknown") {
		return errors.New("invalid terminal research callback")
	}
	const maxTokens = 1000000000
	for _, count := range []*int{call.InputTokens, call.OutputTokens, call.CacheRead, call.CacheWrite, call.Reasoning} {
		if count != nil && (*count < 0 || *count > maxTokens) {
			return errors.New("invalid research usage count")
		}
	}
	if !fitsTokenTotal(call.InputTokens, call.CacheRead, call.CacheWrite) ||
		!fitsTokenTotal(call.OutputTokens, call.Reasoning) {
		return errors.New("research usage subsets do not fit inclusive totals")
	}
	unavailable := call.InputTokens == nil && call.OutputTokens == nil
	if (unavailable && call.UsageSource != "unavailable") ||
		(!unavailable && call.UsageSource != "langchain_usage_metadata") {
		return errors.New("research usage source disagrees with token totals")
	}
	return nil
}

func fitsTokenTotal(total *int, counts ...*int) bool {
	var sum int64
	for _, count := range counts {
		if count != nil {
			if total == nil {
				return false
			}
			sum += int64(*count)
		}
	}
	return total == nil || sum <= int64(*total)
}
