package contract

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
)

func TestPublishedPaperOwnerHTTPContracts(t *testing.T) {
	for _, contract := range []struct {
		filename string
		embedded []byte
	}{
		{"paper-accounts-http.schema.json", accountsHTTPSchema},
		{"paper-diagnostics-http.schema.json", diagnosticsHTTPSchema},
		{"paper-check.schema.json", diagnosticsReportSchema},
	} {
		published, err := os.ReadFile("../../../../contracts/" + contract.filename)
		if err != nil || !bytes.Equal(published, contract.embedded) {
			t.Fatalf("published and embedded %s differ: %v", contract.filename, err)
		}
	}
	envelope, err := os.ReadFile("../../../../contracts/valid-paper-accounts-http-reconcile.json")
	if err != nil {
		t.Fatal(err)
	}
	var reconcile struct {
		Observation []byte `json:"observation_base64"`
		Digest      string `json:"observation_sha256"`
	}
	if err := Decode(envelope, PaperHTTPReconcile, &reconcile); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(reconcile.Observation)
	if hex.EncodeToString(digest[:]) != reconcile.Digest {
		t.Fatal("reconciliation fixture hash disagrees with its exact bytes")
	}
	var observation map[string]json.RawMessage
	if err := Decode(reconcile.Observation, PaperReconciliationObservation, &observation); err != nil {
		t.Fatal(err)
	}
	request, err := os.ReadFile("../../../../contracts/valid-paper-diagnostics-account-request.json")
	if err != nil {
		t.Fatal(err)
	}
	var diagnostics map[string]json.RawMessage
	if err := Decode(request, PaperAccountDiagnosticsRequest, &diagnostics); err != nil {
		t.Fatal(err)
	}
}
