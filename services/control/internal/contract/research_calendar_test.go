package contract

import (
	"bytes"
	"os"
	"testing"
)

func TestResearchCalendarContractMatchesPublishedSchema(t *testing.T) {
	public, err := os.ReadFile("../../../../contracts/research-calendar-occurrence.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(public, researchCalendarSchema) {
		t.Fatal("embedded calendar occurrence schema differs from the public contract")
	}
}
