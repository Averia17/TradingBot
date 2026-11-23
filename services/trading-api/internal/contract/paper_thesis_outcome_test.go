package contract

import (
	"bytes"
	"os"
	"testing"
)

func TestPaperThesisOutcomeContractMatchesPublishedSchema(t *testing.T) {
	public, err := os.ReadFile("../../../../contracts/paper-thesis-outcome.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(paperThesisOutcomeSchema, public) {
		t.Fatal("embedded and published paper thesis outcome schemas differ")
	}
}
