package jobs

import (
	"encoding/json"
	"testing"
)

func TestPersistedBudgetPreservesLegacyAndMalformedValues(t *testing.T) {
	for _, test := range []struct {
		name    string
		request string
		want    jobCallBudget
	}{
		{"legacy request", `{}`, jobCallBudget{}},
		{"configured", `{"max_llm_calls":64}`, jobCallBudget{limit: 64}},
		{"malformed", `{"max_llm_calls":{"limit":64}}`, jobCallBudget{invalid: true}},
		{"malformed repeated field", `{"max_llm_calls":"64","max_llm_calls":64}`, jobCallBudget{limit: 64, invalid: true}},
		{"null repeated field", `{"max_llm_calls":4,"max_llm_calls":null}`, jobCallBudget{limit: 4}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var input struct {
				MaxLLMCalls jobCallBudget `json:"max_llm_calls"`
			}
			if err := json.Unmarshal([]byte(test.request), &input); err != nil {
				t.Fatal(err)
			}
			if input.MaxLLMCalls != test.want {
				t.Fatalf("budget = %+v, want %+v", input.MaxLLMCalls, test.want)
			}
		})
	}
}
