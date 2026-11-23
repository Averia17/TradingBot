package contract

import _ "embed"

//go:embed paper-accounts-http.schema.json
var accountsHTTPSchema []byte

var accountProtocolSchema = schemaResource{"https://tradingbot.local/contracts/paper-account.schema.json", paperSchema}

var (
	PaperHTTPReservation        = compile(accountsHTTPSchema, "#/$defs/Reservation", accountProtocolSchema)
	PaperHTTPReserveApplication = compile(accountsHTTPSchema, "#/$defs/ReserveApplication", accountProtocolSchema)
	PaperHTTPRecordApplication  = compile(accountsHTTPSchema, "#/$defs/RecordApplication", accountProtocolSchema)
	PaperHTTPRiskRefusal        = compile(accountsHTTPSchema, "#/$defs/RiskRefusal", accountProtocolSchema)
	PaperHTTPReconcile          = compile(accountsHTTPSchema, "#/$defs/Reconcile")

	PaperHTTPAccount           = compile(paperSchema, "#/$defs/Account")
	PaperHTTPOrder             = compile(paperSchema, "#/$defs/OrderWithFills")
	PaperHTTPOrderWrite        = compile(accountsHTTPSchema, "#/$defs/OrderWrite", accountProtocolSchema)
	PaperHTTPApplicationRecord = compile(accountsHTTPSchema, "#/$defs/ApplicationRecord", accountProtocolSchema)
	PaperHTTPRiskActivity      = compile(accountsHTTPSchema, "#/$defs/RiskActivity", accountProtocolSchema)
)
