package contract

import (
	"bytes"
	"os"
	"testing"
)

func TestPaperNAVContractMatchesPublishedSchemaAndFixture(t *testing.T) {
	public, err := os.ReadFile("../../../../contracts/paper-nav.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(paperNAVSchema, public) {
		t.Fatal("embedded and published paper NAV schemas differ")
	}
	fixture, err := os.ReadFile("../../../../contracts/valid-paper-nav-input.json")
	if err != nil {
		t.Fatal(err)
	}
	var request any
	if err := Decode(fixture, PaperNAVRequest, &request); err != nil {
		t.Fatal(err)
	}
}
