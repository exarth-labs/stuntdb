package config

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
	"go.yaml.in/yaml/v3"
	"golang.org/x/text/language"
	"golang.org/x/text/message"

	"github.com/exarth-labs/stuntdb/schemas"
)

// Position is a place in a configuration file, or another origin of a
// setting, such as an environment variable.
type Position struct {
	File         string
	Line, Column int
}

func (p Position) String() string {
	if p.Line == 0 {
		return p.File
	}
	return fmt.Sprintf("%s:%d:%d", p.File, p.Line, p.Column)
}

// problems collects the mistakes in a file, so that all of them are reported
// at once, in the order they appear.
type problems []problem

type problem struct {
	at  Position
	msg string
}

func (ps *problems) add(at Position, format string, args ...any) {
	*ps = append(*ps, problem{at: at, msg: fmt.Sprintf(format, args...)})
}

func (ps problems) err() error {
	sorted := slices.Clone(ps)
	slices.SortStableFunc(sorted, func(a, b problem) int {
		return cmp.Or(cmp.Compare(a.at.File, b.at.File), cmp.Compare(a.at.Line, b.at.Line), cmp.Compare(a.at.Column, b.at.Column))
	})
	errList := make([]error, len(sorted))
	for i, p := range sorted {
		errList[i] = fmt.Errorf("%s: %s", p.at, p.msg)
	}
	return errors.Join(errList...)
}

// file is a parsed configuration file: its YAML tree, and where each value is.
type file struct {
	name string
	root *yaml.Node // a mapping
}

func (f *file) pos(n *yaml.Node) Position {
	return Position{File: f.name, Line: n.Line, Column: n.Column}
}

// parseFile reads one YAML document. It rejects what YAML allows but a
// configuration should not: several documents, duplicate keys, anchors and
// aliases, and keys that are not plain text.
func parseFile(name string, data []byte) (*file, error) {
	f := &file{name: name}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%s: the file is empty", name)
		}
		return nil, fmt.Errorf("%s: %s", name, strings.TrimPrefix(err.Error(), "yaml: "))
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: the file holds more than one YAML document", name)
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s: the file must hold settings (key: value), starting with version: 1", name)
	}
	f.root = doc.Content[0]
	var ps problems
	f.check(f.root, &ps)
	return f, ps.err()
}

func (f *file) check(n *yaml.Node, ps *problems) {
	switch n.Kind {
	case yaml.AliasNode:
		ps.add(f.pos(n), "YAML aliases (*name) are not supported; write the value out")
		return
	case yaml.MappingNode:
		seen := map[string]bool{}
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := n.Content[i]
			if k.Kind != yaml.ScalarNode || k.Tag == "!!merge" {
				ps.add(f.pos(k), "a key must be plain text")
				continue
			}
			if seen[k.Value] {
				ps.add(f.pos(k), "%s appears twice", strconv.Quote(k.Value))
			}
			seen[k.Value] = true
			f.check(n.Content[i+1], ps)
		}
	case yaml.SequenceNode:
		for _, c := range n.Content {
			f.check(c, ps)
		}
	}
	if n.Anchor != "" {
		ps.add(f.pos(n), "YAML anchors (&name) are not supported")
	}
}

// instance converts a node into the JSON-like value the schema validates,
// and records where each value is, by JSON pointer.
func (f *file) instance(n *yaml.Node, ptr string, at map[string]*yaml.Node) any {
	at[ptr] = n
	switch n.Kind {
	case yaml.MappingNode:
		m := make(map[string]any, len(n.Content)/2)
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			child := ptr + "/" + escapePointer(k.Value)
			at[child+"#key"] = k
			m[k.Value] = f.instance(v, child, at)
		}
		return m
	case yaml.SequenceNode:
		s := make([]any, len(n.Content))
		for i, c := range n.Content {
			s[i] = f.instance(c, ptr+"/"+strconv.Itoa(i), at)
		}
		return s
	default:
		return scalar(n)
	}
}

// scalar converts a YAML scalar. The bare word null stays the text "null",
// because it names the null transformer and the null policy; an empty value
// or ~ is YAML's null.
func scalar(n *yaml.Node) any {
	switch n.ShortTag() {
	case "!!null":
		if strings.EqualFold(n.Value, "null") {
			return "null"
		}
		return nil
	case "!!bool":
		return strings.EqualFold(n.Value, "true")
	case "!!int":
		if i, err := strconv.ParseInt(strings.ReplaceAll(n.Value, "_", ""), 0, 64); err == nil {
			return json.Number(strconv.FormatInt(i, 10))
		}
	case "!!float":
		if f, err := strconv.ParseFloat(n.Value, 64); err == nil && !math.IsInf(f, 0) && !math.IsNaN(f) {
			return json.Number(strconv.FormatFloat(f, 'g', -1, 64))
		}
	}
	return n.Value // text, and anything YAML types that JSON has no type for, such as dates
}

func escapePointer(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1")
}

var english = message.NewPrinter(language.English)

// compiledSchema is the configuration schema, compiled once. It is built
// from a constant, embedded file, and never changes.
var compiledSchema = sync.OnceValues(func() (*jsonschema.Schema, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemas.Config()))
	if err != nil {
		return nil, err
	}
	c := jsonschema.NewCompiler()
	const url = "stuntdb.schema.json"
	if err := c.AddResource(url, doc); err != nil {
		return nil, err
	}
	return c.Compile(url)
})

// validate checks the file against the schema, and reports every problem at
// the position of the value it concerns.
func (f *file) validate() error {
	ps, err := f.schemaProblems()
	if err != nil {
		return err
	}
	return ps.err()
}

func (f *file) schemaProblems() (problems, error) {
	sch, err := compiledSchema()
	if err != nil {
		return nil, fmt.Errorf("config: the built-in schema is broken: %w", err)
	}
	at := map[string]*yaml.Node{}
	inst := f.instance(f.root, "", at)
	err = sch.Validate(inst)
	if err == nil {
		return nil, nil
	}
	ve, ok := errors.AsType[*jsonschema.ValidationError](err)
	if !ok {
		return nil, fmt.Errorf("%s: %w", f.name, err)
	}
	var ps problems
	for _, leaf := range leaves(ve) {
		f.describe(leaf, at, &ps)
	}
	return ps, nil
}

// leaves returns the errors that say what is wrong, skipping the ones that
// only group others. Where a value may take one of several forms (a rule is
// a name or an object), it keeps the form the value has.
func leaves(e *jsonschema.ValidationError) []*jsonschema.ValidationError {
	if len(e.Causes) == 0 {
		return []*jsonschema.ValidationError{e}
	}
	causes := e.Causes
	if _, ok := e.ErrorKind.(*kind.OneOf); ok {
		var sameShape []*jsonschema.ValidationError
		var want []string
		for _, c := range causes {
			if t := wrongTypeHere(c, e.InstanceLocation); t != nil {
				want = append(want, t.Want...)
			} else {
				sameShape = append(sameShape, c)
			}
		}
		if len(sameShape) == 0 {
			// The value has none of the forms: say which forms there are.
			return []*jsonschema.ValidationError{{InstanceLocation: e.InstanceLocation, ErrorKind: &kind.Type{Want: want}}}
		}
		causes = sameShape
	}
	var out []*jsonschema.ValidationError
	for _, c := range causes {
		out = append(out, leaves(c)...)
	}
	return out
}

// wrongTypeHere returns the type error, if e only says that the value at loc
// has another type than one form expects.
func wrongTypeHere(e *jsonschema.ValidationError, loc []string) *kind.Type {
	for len(e.Causes) == 1 {
		e = e.Causes[0]
	}
	if t, ok := e.ErrorKind.(*kind.Type); ok && len(e.Causes) == 0 && slices.Equal(e.InstanceLocation, loc) {
		return t
	}
	return nil
}

// describe turns one schema error into a message. Messages name the setting
// and what it must be, and never repeat the value, which may be a secret.
func (f *file) describe(e *jsonschema.ValidationError, at map[string]*yaml.Node, ps *problems) {
	ptr := ""
	for _, part := range e.InstanceLocation {
		ptr += "/" + escapePointer(part)
	}
	node := at[ptr]
	if node == nil {
		node = f.root
	}
	where, what := f.pos(node), settingName(e.InstanceLocation)

	switch k := e.ErrorKind.(type) {
	case *kind.AdditionalProperties:
		for _, p := range k.Properties {
			keyAt := where
			if n := at[ptr+"/"+escapePointer(p)+"#key"]; n != nil {
				keyAt = f.pos(n)
			}
			ps.add(keyAt, "unknown setting %s%s", settingName(append(slices.Clone(e.InstanceLocation), p)), laterHint(e.InstanceLocation, p))
		}
	case *kind.Required:
		for _, m := range k.Missing {
			ps.add(where, "%s is missing", settingName(append(slices.Clone(e.InstanceLocation), m)))
		}
	case *kind.Type:
		ps.add(where, "%s must be %s", what, typeList(k.Want))
	case *kind.Enum:
		want := make([]string, len(k.Want))
		for i, w := range k.Want {
			want[i] = fmt.Sprint(w)
		}
		ps.add(where, "%s must be one of: %s", what, strings.Join(want, ", "))
	case *kind.Const:
		ps.add(where, "%s must be %v", what, k.Want)
	case *kind.Pattern:
		ps.add(where, "%s must be %s", what, patternMeaning(k.Want))
	case *kind.MinLength:
		if k.Want == 1 {
			ps.add(where, "%s must not be empty", what)
		} else {
			ps.add(where, "%s must have at least %d characters", what, k.Want)
		}
	case *kind.Minimum:
		ps.add(where, "%s must be at least %s", what, k.Want.RatString())
	case *kind.Maximum:
		ps.add(where, "%s must be at most %s", what, k.Want.RatString())
	case *kind.Not:
		ps.add(where, "%s: use key or key_file, not both", what)
	case *kind.PropertyNames:
		ps.add(where, "%s has an empty name", what)
	default:
		ps.add(where, "%s: %s", what, strings.ReplaceAll(e.ErrorKind.LocalizedString(english), "'", `"`))
	}
}

// settingName writes a location as masking.unclassified, or
// columns["main.users"].email when a part has a dot or a space.
func settingName(loc []string) string {
	if len(loc) == 0 {
		return "the file"
	}
	var b strings.Builder
	for i, part := range loc {
		switch {
		case strings.ContainsAny(part, ". \"[]") || part == "":
			b.WriteString("[" + strconv.Quote(part) + "]")
		case i > 0:
			b.WriteString("." + part)
		default:
			b.WriteString(part)
		}
	}
	return b.String()
}

func typeList(want []string) string {
	words := make([]string, len(want))
	for i, w := range want {
		switch w {
		case "object":
			words[i] = "a set of settings (key: value)"
		case "array":
			words[i] = "a list"
		case "integer":
			words[i] = "a whole number"
		case "number":
			words[i] = "a number"
		case "boolean":
			words[i] = "true or false"
		default:
			words[i] = "a " + w
		}
	}
	return strings.Join(words, " or ")
}

func patternMeaning(pattern string) string {
	switch {
	case strings.Contains(pattern, "ns|us|ms|s|m|h"):
		return "a duration, such as 500ms, 30s, 5m or 1h30m"
	case strings.HasPrefix(pattern, "^[a-z][a-z0-9_]*"):
		return "a transformer name: lower-case letters, digits and _, starting with a letter"
	default:
		return "letters, digits, dots, dashes and _ only"
	}
}

// laterHint explains settings that the plan adds in a later release, so
// that a config written for a newer stuntdb fails clearly.
func laterHint(parent []string, key string) string {
	switch path := strings.Join(append(slices.Clone(parent), key), "."); path {
	case "subset":
		return " (subsetting arrives in a later release)"
	case "output.out", "output.format":
		return " (file outputs arrive in a later release; use output.to for now)"
	default:
		return ""
	}
}
