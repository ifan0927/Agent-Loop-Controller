package contracts

import _ "embed"

//go:embed implementation-outcome.schema.json
var ImplementationOutcomeSchema string

//go:embed review-outcome.schema.json
var ReviewOutcomeSchema string
