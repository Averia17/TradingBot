package contract

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
)

func TestResearchCallUsageSchemasMatchPublishedContracts(t *testing.T) {
	for path, embedded := range map[string][]byte{
		"research-call-usage.schema.json": researchCallUsageSchema,
		"research-llm-usage.schema.json":  researchLLMUsageSchema,
	} {
		raw, err := os.ReadFile("../../../../contracts/" + path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(raw, embedded) {
			t.Fatalf("embedded %s differs from published contract", path)
		}
	}
}

func TestResearchCallUsageReceiptUsesCanonicalUTC(t *testing.T) {
	const receipt = `{"job_id":"00000000-0000-4000-8000-abcdef000013","call_id":"00000000-0000-4000-8000-abcdef000019","attempt":3,"recorded_at":"2026-11-18T13:42:16.000001Z"}`
	var decoded map[string]json.RawMessage
	if err := Decode([]byte(receipt), ResearchCallUsageReceipt, &decoded); err != nil {
		t.Fatal(err)
	}
	replaced := bytes.Replace([]byte(receipt), []byte("Z"), []byte("+00:00"), 1)
	if err := Decode(replaced, ResearchCallUsageReceipt, &decoded); err == nil {
		t.Fatal("noncanonical UTC receipt was accepted")
	}
}
