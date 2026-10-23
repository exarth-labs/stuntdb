package classify

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/exarth-labs/stuntdb/internal/value"
	"github.com/exarth-labs/stuntdb/rules"
)

const packVersion = 1

// packFile is a rule pack as written. The rules package documents the format.
type packFile struct {
	Version int        `yaml:"version"`
	Pack    string     `yaml:"pack"`
	Rules   []ruleFile `yaml:"rules"`
}

type ruleFile struct {
	ID         string            `yaml:"id"`
	Class      string            `yaml:"class"`
	Use        string            `yaml:"use"`
	Options    map[string]string `yaml:"options"`
	Confidence float64           `yaml:"confidence"`
	Types      []string          `yaml:"types"`
	Names      []string          `yaml:"names"`
	Tables     []string          `yaml:"tables"`
	Identity   bool              `yaml:"identity"`
}

// rule is a parsed, checked rule.
type rule struct {
	id         string // pack.id
	class      Class
	use        string
	options    map[string]string
	confidence float64
	types      uint32     // a set of value.TypeKind, one bit each
	names      [][]string // phrases, as words
	tables     [][]string
	identity   bool
}

var identifier = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// typeKinds maps the type names a pack may use to their kinds.
var typeKinds = func() map[string]value.TypeKind {
	m := map[string]value.TypeKind{}
	for k := value.TypeKind(1); k.String() != value.TypeInvalid.String(); k++ {
		m[k.String()] = k
	}
	return m
}()

// parsePack reads and checks one rule pack. Every mistake is reported at
// once, with its line.
func parsePack(p rules.Pack) ([]*rule, error) {
	file := p.Name + ".yaml"
	// The tree gives each rule's line; the strict decoding below then
	// refuses misspelled settings, which the tree alone would accept.
	var doc yaml.Node
	if err := yaml.Unmarshal(p.Data, &doc); err != nil {
		return nil, fmt.Errorf("%s: %s", file, strings.TrimPrefix(err.Error(), "yaml: "))
	}
	if len(doc.Content) == 0 {
		return nil, fmt.Errorf("%s: the file is empty", file)
	}
	dec := yaml.NewDecoder(bytes.NewReader(p.Data))
	dec.KnownFields(true)
	var pf packFile
	if err := dec.Decode(&pf); err != nil {
		return nil, fmt.Errorf("%s: %s", file, strings.TrimPrefix(err.Error(), "yaml: "))
	}
	lines := ruleLines(doc.Content[0])

	var problems []error
	add := func(line int, format string, args ...any) {
		problems = append(problems, fmt.Errorf("%s:%d: %s", file, line, fmt.Sprintf(format, args...)))
	}
	switch {
	case pf.Version != packVersion:
		add(1, "version must be %d", packVersion)
	case pf.Pack != p.Name:
		add(1, "pack must be %q, the file's name", p.Name)
	case len(pf.Rules) == 0:
		add(1, "the pack has no rules")
	}

	out := make([]*rule, 0, len(pf.Rules))
	seen := map[string]bool{}
	for i, rf := range pf.Rules {
		line := lines[i]
		r, errs := rf.check(p.Name)
		for _, e := range errs {
			add(line, "rule %s: %s", cmp.Or(rf.ID, fmt.Sprintf("#%d", i+1)), e)
		}
		if seen[rf.ID] {
			add(line, "rule %s appears twice", rf.ID)
		}
		seen[rf.ID] = true
		out = append(out, r)
	}
	if err := errors.Join(problems...); err != nil {
		return nil, err
	}
	return out, nil
}

// ruleLines returns the line of each rule, for error messages. The pack has
// already been decoded, so root is a mapping and its rules a list.
func ruleLines(root *yaml.Node) []int {
	var lines []int
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "rules" {
			for _, n := range root.Content[i+1].Content {
				lines = append(lines, n.Line)
			}
		}
	}
	return lines
}

// check turns a rule as written into a rule, and lists its mistakes.
func (rf ruleFile) check(pack string) (*rule, []string) {
	var bad []string
	r := &rule{id: pack + "." + rf.ID, use: rf.Use, options: rf.Options, confidence: rf.Confidence, identity: rf.Identity}
	if !identifier.MatchString(rf.ID) {
		bad = append(bad, "id must be lowercase letters, digits and underscores")
	}

	class, ok := parseClass(rf.Class)
	switch {
	case !ok || class == Unclassified:
		bad = append(bad, fmt.Sprintf("unknown class %q; use direct, quasi, sensitive, secret, free_text or safe", rf.Class))
	case class == Safe && rf.Use != keep:
		bad = append(bad, "a safe rule must use keep")
	case class == Secret && rf.Use == keep:
		bad = append(bad, "a secret rule cannot use keep: secrets are always replaced")
	}
	r.class = class
	if !identifier.MatchString(rf.Use) {
		bad = append(bad, "use must name a transformer")
	}
	for name := range rf.Options {
		if !identifier.MatchString(name) {
			bad = append(bad, fmt.Sprintf("option %q is not a valid name", name))
		}
	}
	if rf.Confidence <= 0 || rf.Confidence > 1 {
		bad = append(bad, "confidence must be more than 0 and at most 1")
	}

	if len(rf.Types) == 0 {
		bad = append(bad, "types is missing")
	}
	for _, name := range rf.Types {
		kind, ok := typeKinds[name]
		if !ok {
			bad = append(bad, fmt.Sprintf("unknown type %q", name))
			continue
		}
		r.types |= 1 << kind
	}

	var err []string
	r.names, err = phrases("names", rf.Names)
	bad = append(bad, err...)
	r.tables, err = phrases("tables", rf.Tables)
	bad = append(bad, err...)
	if len(rf.Names) == 0 && !rf.Identity {
		bad = append(bad, "a rule needs names, or identity: true")
	}
	return r, bad
}

// phrases splits each phrase into words. A phrase must be written the way
// words() writes names: lowercase words without accents, separated by
// single spaces.
func phrases(field string, list []string) ([][]string, []string) {
	var out [][]string
	var bad []string
	for i, p := range list {
		ws := words(p)
		switch {
		case len(ws) == 0 || strings.Join(ws, " ") != p:
			bad = append(bad, fmt.Sprintf("%s: %q must be lowercase words without accents, separated by single spaces", field, p))
		case slices.Contains(list[:i], p):
			bad = append(bad, fmt.Sprintf("%s: %q appears twice", field, p))
		default:
			out = append(out, ws)
		}
	}
	return out, bad
}
