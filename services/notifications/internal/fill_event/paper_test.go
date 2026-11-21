package fill_event

import (
	"bytes"
	"strings"
	"testing"
)

func TestPaperFillWirePreservesExactAmountsAndIdentities(t *testing.T) {
	if published := loadContractFile(t, "paper-fill-event.schema.json"); !bytes.Equal(paperSchemaJSON, published) {
		t.Fatal("embedded paper fill contract differs from the published schema")
	}
	data := loadContractFile(t, "valid-paper-fill-event.json")
	e, err := DecodePaper(data)
	if err != nil || e.QuantityRaw != "3" || e.Decimals != 18 || e.GrossUSD != "1" || e.FeeUSD != "0.00000001" {
		t.Fatalf("paper ledger fill = %+v, %v", e, err)
	}
	if _, err := Decode(data); err == nil {
		t.Fatal("the fixture contract accepted an authoritative ledger fill")
	}
	for name, invalid := range map[string]string{
		"live":                strings.Replace(string(data), `"mode": "paper"`, `"mode": "live"`, 1),
		"wrong id":            strings.Replace(string(data), `"id": "paper:11111111`, `"id": "paper:99999999`, 1),
		"duplicated field":    strings.Replace(string(data), `"gross_usd": "1"`, `"gross_usd": "1", "gross_usd": "1"`, 1),
		"premature recording": strings.Replace(string(data), `09:00:02.123456Z`, `08:59:59.123456Z`, 1),
		"numeric amount":      strings.Replace(string(data), `"gross_usd": "1"`, `"gross_usd": 1`, 1),
		"escaped timestamp":   strings.Replace(string(data), `09:00:02.123456Z`, `09:00:02.123456\u005a`, 1),
		"trailing JSON":       string(data) + "{}",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodePaper([]byte(invalid)); err == nil {
				t.Fatal("invalid paper fill was accepted")
			}
		})
	}
}
