package contract

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestPaperJupiterFixtureSchemaAndReceipts(t *testing.T) {
	load := func(name string) []byte {
		t.Helper()
		data, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "contracts", name))
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	if !bytes.Equal(paperJupiterFixtureSchema, load("paper-jupiter-fixture.schema.json")) {
		t.Fatal("embedded Jupiter fixture schema differs from the published schema")
	}
	request := load("valid-paper-jupiter-fixture-request.json")
	var wire bytes.Buffer
	if err := json.Compact(&wire, request); err != nil {
		t.Fatal(err)
	}
	var raw json.RawMessage
	if err := Decode(wire.Bytes(), compile(paperJupiterFixtureSchema, "#/$defs/Request"), &raw); err != nil {
		t.Fatalf("invalid fixture request: %v", err)
	}
	digest := sha256.Sum256(wire.Bytes())
	for _, state := range []string{"pending", "finalized"} {
		data := load("valid-paper-jupiter-fixture-" + state + ".json")
		if err := Decode(data, PaperJupiterFixtureReceipt, &raw); err != nil {
			t.Fatalf("invalid %s receipt: %v", state, err)
		}
		var receipt struct {
			RequestSHA256 string `json:"request_sha256"`
		}
		if err := json.Unmarshal(raw, &receipt); err != nil || receipt.RequestSHA256 != hex.EncodeToString(digest[:]) {
			t.Fatalf("%s receipt does not bind to the compact request bytes", state)
		}
	}
	refusal := bytes.Replace(load("valid-paper-jupiter-fixture-pending.json"), []byte(`"status": "pending"`),
		[]byte(`"status": "pending", "gross_usd": "200"`), 1)
	if Decode(refusal, PaperJupiterFixtureReceipt, &raw) == nil {
		t.Fatal("pending response accepted settlement data")
	}
}
