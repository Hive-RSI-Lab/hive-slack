// Package hiveslack holds what the module ships beside its program: the
// Slack app's manifest, printed by `hive slack manifest`.
package hiveslack

import _ "embed"

// Manifest is slack-app.json: the Slack app the person makes from it.
//
//go:embed slack-app.json
var Manifest string
