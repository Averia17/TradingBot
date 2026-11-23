package contract

import (
	"bytes"
	"os"
	"testing"
)

func TestPaperShadowOutcomeContractMatchesPublishedSchema(t *testing.T) {
	public, err := os.ReadFile("../../../../contracts/paper-shadow-outcome.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(public, paperShadowOutcomeSchema) {
		t.Fatal("embedded paper shadow outcome schema differs from the public contract")
	}
}
