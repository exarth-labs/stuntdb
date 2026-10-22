package config

import (
	"go.yaml.in/yaml/v3"
)

// columns reads the masking rules, in the order the file lists them. The
// schema has already checked their shape.
func (f *file) columns() ([]*TableRules, error) {
	n := f.lookup("columns")
	if n == nil || n.Kind != yaml.MappingNode {
		return nil, nil
	}
	var out []*TableRules
	var ps problems
	seen := map[TableName]Position{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		key, rules := n.Content[i], n.Content[i+1]
		name, err := ParseTableName(key.Value)
		if err != nil {
			ps.add(f.pos(key), "columns: %v", err)
			continue
		}
		if first, dup := seen[name]; dup {
			ps.add(f.pos(key), "columns: table %s already has rules, at %s", name, first)
			continue
		}
		seen[name] = f.pos(key)
		t := &TableRules{Table: name, At: f.pos(key)}
		for j := 0; j+1 < len(rules.Content); j += 2 {
			t.Columns = append(t.Columns, f.rule(rules.Content[j], rules.Content[j+1]))
		}
		out = append(out, t)
	}
	return out, ps.err()
}

// rule reads one column's rule: a transformer name, or an object with use:,
// an optional domain:, and the transformer's own options.
func (f *file) rule(key, v *yaml.Node) *ColumnRule {
	r := &ColumnRule{Column: key.Value, At: f.pos(key)}
	if v.Kind == yaml.ScalarNode {
		r.Use = v.Value
		return r
	}
	for i := 0; i+1 < len(v.Content); i += 2 {
		name, val := v.Content[i].Value, v.Content[i+1]
		switch name {
		case "use":
			r.Use = val.Value
		case "domain":
			r.Domain = val.Value
		default:
			if r.Options == nil {
				r.Options = map[string]any{}
			}
			r.Options[name] = f.instance(val, "", map[string]*yaml.Node{})
		}
	}
	return r
}
