// Package rules holds stuntdb's built-in classification rule packs. A rule
// pack is a YAML file of rules that suggest what a column holds, such as an
// email address or a password hash, from its name and type alone. The packs
// are embedded in the binary; internal/classify reads them, and its package
// documentation describes the format.
package rules

import (
	"bytes"
	_ "embed"
)

//go:embed core.yaml
var core []byte

//go:embed secrets.yaml
var secrets []byte

// Pack is one rule pack file.
type Pack struct {
	Name string // the pack's name, which is also its file name without .yaml
	Data []byte
}

// Builtin returns the packs built into stuntdb, in the order they apply.
func Builtin() []Pack {
	return []Pack{
		{Name: "core", Data: bytes.Clone(core)},
		{Name: "secrets", Data: bytes.Clone(secrets)},
	}
}
