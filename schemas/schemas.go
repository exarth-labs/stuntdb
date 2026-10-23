// Package schemas holds the JSON Schemas that stuntdb publishes. Editors use
// them for autocomplete and validation; stuntdb validates against the same
// files, so the two can never disagree.
package schemas

import (
	"bytes"
	_ "embed"
)

// ConfigURL is where the schema of stuntdb.yaml is published: its $id.
// Files that stuntdb writes point editors at it.
const ConfigURL = "https://raw.githubusercontent.com/exarth-labs/stuntdb/main/schemas/stuntdb.schema.json"

//go:embed stuntdb.schema.json
var config []byte

// Config returns the JSON Schema of stuntdb.yaml.
func Config() []byte { return bytes.Clone(config) }
