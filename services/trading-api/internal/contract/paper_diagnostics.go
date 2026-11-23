package contract

import _ "embed"

//go:embed paper-diagnostics-http.schema.json
var diagnosticsHTTPSchema []byte

//go:embed paper-check.schema.json
var diagnosticsReportSchema []byte

var diagnosticsReport = schemaResource{"https://tradingbot.local/contracts/paper-check.schema.json", diagnosticsReportSchema}

var (
	PaperJobsDiagnosticsRequest     = compile(diagnosticsHTTPSchema, "#/$defs/ResearchRequest", diagnosticsReport)
	PaperJobsDiagnosticsResponse    = compile(diagnosticsHTTPSchema, "#/$defs/ResearchResponse", diagnosticsReport)
	PaperAccountDiagnosticsRequest  = compile(diagnosticsHTTPSchema, "#/$defs/AccountRequest", diagnosticsReport)
	PaperAccountDiagnosticsResponse = compile(diagnosticsHTTPSchema, "#/$defs/AccountResponse", diagnosticsReport)
)
