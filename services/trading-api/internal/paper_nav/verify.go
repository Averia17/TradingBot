package paper_nav

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	"tradingbot/trading-api/internal/contract"
)

// MaxReportBytes matches the limit of the paper evaluator that consumes NAV reports.
const MaxReportBytes = 8 * 1024 * 1024

// CanonicalReport pins the stored bytes and normalizes JSON formatting while
// keeping the exact serialized timestamps, amounts and account versions.
func CanonicalReport(raw []byte, expectedSHA256 string) ([]byte, error) {
	if len(raw) > MaxReportBytes {
		return nil, errors.New("paper NAV report exceeds the byte limit")
	}
	digest := sha256.Sum256(raw)
	if hex.EncodeToString(digest[:]) != expectedSHA256 {
		return nil, errors.New("paper NAV report SHA-256 mismatch")
	}
	var report json.RawMessage
	if err := contract.Decode(raw, contract.PaperNAVReport, &report); err != nil {
		return nil, errors.New("invalid paper NAV report schema")
	}
	return canonicalJSON(report)
}

// CompareReport compares the pinned, validated archive to the rebuilt report.
func CompareReport(archived []byte, rebuilt Report) error {
	raw, err := json.Marshal(rebuilt)
	if err != nil {
		return err
	}
	current, err := canonicalJSON(raw)
	if err != nil {
		return err
	}
	if !bytes.Equal(archived, current) {
		return errors.New("saved paper NAV report does not match the ledger replay and pinned inputs")
	}
	return nil
}

func canonicalJSON(raw []byte) ([]byte, error) {
	var document any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&document); err != nil {
		return nil, err
	}
	return json.Marshal(document)
}
