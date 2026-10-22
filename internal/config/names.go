package config

import (
	"errors"
	"strings"
)

// TableName is a table as the configuration names it: "users",
// "main.users", or with double quotes around a part that contains a dot or
// a quote, as in `main."order.items"`. The parts are kept as written; the
// check step matches them to the database's own names, with the database's
// rules for upper and lower case.
type TableName struct {
	Namespace string // empty means the connection's default namespace
	Name      string
}

// String writes the name back in the form ParseTableName reads.
func (n TableName) String() string {
	if n.Namespace == "" {
		return quotePart(n.Name)
	}
	return quotePart(n.Namespace) + "." + quotePart(n.Name)
}

// ParseTableName parses "table", "namespace.table", or either with parts in
// double quotes, where "" stands for a quote inside.
func ParseTableName(s string) (TableName, error) {
	parts, err := splitParts(s)
	if err != nil {
		return TableName{}, err
	}
	switch len(parts) {
	case 1:
		return TableName{Name: parts[0]}, nil
	case 2:
		return TableName{Namespace: parts[0], Name: parts[1]}, nil
	default:
		return TableName{}, errors.New("a table is written as table or namespace.table; put a name that contains a dot in double quotes")
	}
}

func splitParts(s string) ([]string, error) {
	var parts []string
	for {
		var part string
		if strings.HasPrefix(s, `"`) {
			end := closingQuote(s)
			if end < 0 {
				return nil, errors.New("a double quote in the table name is not closed")
			}
			part, s = strings.ReplaceAll(s[1:end], `""`, `"`), s[end+1:]
		} else {
			i := strings.IndexAny(s, `."`)
			if i < 0 {
				i = len(s)
			} else if s[i] == '"' {
				return nil, errors.New("a double quote must start a part of the table name")
			}
			part, s = s[:i], s[i:]
		}
		if part == "" {
			return nil, errors.New("a part of the table name is empty")
		}
		parts = append(parts, part)
		if s == "" {
			return parts, nil
		}
		if s[0] != '.' {
			return nil, errors.New("a quoted part of the table name must be followed by a dot or nothing")
		}
		s = s[1:]
	}
}

// closingQuote returns the index of the quote that closes the part opened at
// s[0], skipping "" pairs, or -1.
func closingQuote(s string) int {
	for i := 1; i < len(s); i++ {
		if s[i] != '"' {
			continue
		}
		if i+1 < len(s) && s[i+1] == '"' {
			i++
			continue
		}
		return i
	}
	return -1
}

func quotePart(p string) string {
	if p != "" && !strings.ContainsAny(p, `."`) {
		return p
	}
	return `"` + strings.ReplaceAll(p, `"`, `""`) + `"`
}
