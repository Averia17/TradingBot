package contract

import _ "embed"

//go:embed equity-route-study.schema.json
var equityRouteStudySchema []byte

//go:embed equity-route-theses.schema.json
var equityRouteThesesSchema []byte

var EquityRouteStudy = compile(equityRouteStudySchema, "")
var EquityRouteTheses = compile(equityRouteThesesSchema, "")
