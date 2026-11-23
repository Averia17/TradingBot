package contract

import _ "embed"

//go:embed paper-reconciliation.schema.json
var paperReconciliationSchema []byte

var PaperReconciliationObservation = compile(paperReconciliationSchema, "#")
