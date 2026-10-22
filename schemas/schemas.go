// Package schemas holds the JSON Schemas that stuntdb publishes. Editors use
// them for autocomplete and validation; stuntdb validates against the same
// files, so the two can never disagree.
package schemas

import (
	"bytes"
	_ "embed"
)

//go:embed stuntdb.schema.json
var config []byte

// Config returns the JSON Schema of stuntdb.yaml.
func Config() []byte { return bytes.Clone(config) }
