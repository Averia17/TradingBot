package contract

import _ "embed"

//go:embed internal-jobs-http.schema.json
var internalJobsHTTPSchema []byte

var InternalJobCreate = compile(internalJobsHTTPSchema, "#/$defs/CreatePaperJob")
var InternalJobFinish = compile(internalJobsHTTPSchema, "#/$defs/FinishDispatch")
