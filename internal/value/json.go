package value

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"unicode/utf8"
)

// AppendJSON appends the JSON encoding of v, a value of a column of type t,
// to dst. The encoding is described in the package documentation.
func AppendJSON(dst []byte, t Type, v Value) ([]byte, error) {
	return appendTyped(dst, &t, v)
}

// DecodeJSON decodes the JSON encoding of a value of a column of type t.
func DecodeJSON(t Type, data []byte) (Value, error) {
	return decodeTyped(&t, data)
}

// appendTyped encodes v naturally when it matches t, and tagged with its
// kind otherwise. t is nil inside a tagged value, so everything is tagged there.
func appendTyped(dst []byte, t *Type, v Value) ([]byte, error) {
	if v.kind == KindNull {
		return append(dst, "null"...), nil
	}
	if t != nil && v.kind == naturalKind(*t) {
		return appendNatural(dst, t.Elem, v)
	}
	dst = append(dst, `{"$":"`...)
	dst = append(dst, v.kind.String()...)
	dst = append(dst, `","v":`...)
	dst, err := appendNatural(dst, nil, v)
	if err != nil {
		return nil, err
	}
	return append(dst, '}'), nil
}

// appendNatural encodes v by its own kind. elem is the element type of an array.
func appendNatural(dst []byte, elem *Type, v Value) ([]byte, error) {
	switch v.kind {
	case KindBool:
		return strconv.AppendBool(dst, v.num == 1), nil
	case KindInt:
		return strconv.AppendInt(dst, int64(v.num), 10), nil
	case KindUint:
		return strconv.AppendUint(dst, v.num, 10), nil
	case KindFloat:
		f := math.Float64frombits(v.num)
		switch {
		case math.IsNaN(f):
			return append(dst, `{"$":"nan"}`...), nil
		case math.IsInf(f, 1):
			return append(dst, `{"$":"+inf"}`...), nil
		case math.IsInf(f, -1):
			return append(dst, `{"$":"-inf"}`...), nil
		}
		return strconv.AppendFloat(dst, f, 'g', -1, 64), nil
	case KindDecimal:
		if v.special != Normal {
			return appendSpecial(dst, v), nil
		}
		return appendString(dst, v.str), nil
	case KindString:
		if !utf8.ValidString(v.str) {
			dst = append(dst, `{"$":"raw","v":`...)
			dst = appendString(dst, base64.StdEncoding.EncodeToString([]byte(v.str)))
			return append(dst, '}'), nil
		}
		return appendString(dst, v.str), nil
	case KindBytes:
		return appendString(dst, base64.StdEncoding.EncodeToString([]byte(v.str))), nil
	case KindUUID:
		return appendString(dst, FormatUUID(v.UUID())), nil
	case KindDate:
		if v.special != Normal {
			return appendSpecial(dst, v), nil
		}
		dst = append(dst, '"')
		dst = appendDate(dst, v.ts)
		return append(dst, '"'), nil
	case KindTime:
		dst = append(dst, '"')
		dst = appendTimeOfDay(dst, v)
		return append(dst, '"'), nil
	case KindTimestamp:
		if v.special != Normal {
			return appendSpecial(dst, v), nil
		}
		dst = append(dst, '"')
		dst = appendTimestamp(dst, v, 'T')
		return append(dst, '"'), nil
	case KindInterval:
		dst = append(dst, `{"months":`...)
		dst = strconv.AppendInt(dst, int64(v.months), 10)
		dst = append(dst, `,"days":`...)
		dst = strconv.AppendInt(dst, int64(v.days), 10)
		dst = append(dst, `,"nanos":`...)
		dst = strconv.AppendInt(dst, int64(v.num), 10)
		return append(dst, '}'), nil
	case KindArray:
		dst = append(dst, '[')
		for i, e := range v.elems {
			if i > 0 {
				dst = append(dst, ',')
			}
			var err error
			if dst, err = appendTyped(dst, elem, e); err != nil {
				return nil, err
			}
		}
		return append(dst, ']'), nil
	default:
		return nil, fmt.Errorf("value: cannot encode a %s value", v.kind)
	}
}

// appendSpecial encodes a special decimal, date or timestamp.
func appendSpecial(dst []byte, v Value) []byte {
	switch v.special {
	case NaN:
		dst = append(dst, `{"$":"nan"`...)
	case PosInfinity:
		dst = append(dst, `{"$":"+inf"`...)
	case NegInfinity:
		dst = append(dst, `{"$":"-inf"`...)
	case ZeroDate:
		dst = append(dst, `{"$":"zero","v":`...)
		dst = appendString(dst, v.str)
	}
	if v.tz {
		dst = append(dst, `,"tz":true`...)
	}
	return append(dst, '}')
}

const hexDigits = "0123456789abcdef"

// appendString appends s as a JSON string. s must be valid UTF-8.
func appendString(dst []byte, s string) []byte {
	dst = append(dst, '"')
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '"' || c == '\\':
			dst = append(dst, '\\', c)
		case c < 0x20:
			dst = append(dst, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0xF])
		default:
			dst = append(dst, c)
		}
	}
	return append(dst, '"')
}

// decodeTyped decodes data for a column of type t. t is nil inside a tagged
// value, where every non-null value must carry its own kind tag.
func decodeTyped(t *Type, data []byte) (Value, error) {
	data = bytes.TrimSpace(data)
	if string(data) == "null" {
		return Null(), nil
	}
	if tag, fields, ok := parseTag(data); ok {
		if k, isKind := kindByName(tag); isKind {
			return decodeNatural(k, nil, fields["v"])
		}
	}
	if t == nil {
		return Value{}, errors.New("value: a value without a kind tag needs a column type")
	}
	return decodeNatural(naturalKind(*t), t, data)
}

// decodeNatural decodes data as a value of kind k. t is the column type, or
// nil inside a tagged value.
func decodeNatural(k Kind, t *Type, data []byte) (Value, error) {
	data = bytes.TrimSpace(data)
	if tag, fields, ok := parseTag(data); ok {
		return decodeSpecial(k, tag, fields)
	}
	errInvalid := fmt.Errorf("value: invalid %s encoding", k)
	switch k {
	case KindBool:
		switch string(data) {
		case "true":
			return Bool(true), nil
		case "false":
			return Bool(false), nil
		}
	case KindInt:
		if isJSONNumber(data) {
			if i, err := strconv.ParseInt(string(data), 10, 64); err == nil {
				return Int(i), nil
			}
		}
	case KindUint:
		if isJSONNumber(data) {
			if u, err := strconv.ParseUint(string(data), 10, 64); err == nil {
				return Uint(u), nil
			}
		}
	case KindFloat:
		if isJSONNumber(data) {
			if f, err := strconv.ParseFloat(string(data), 64); err == nil {
				return Float(f), nil
			}
		}
	case KindDecimal:
		if s, ok := jsonString(data); ok {
			return Decimal(s)
		}
	case KindString:
		if s, ok := jsonString(data); ok {
			return String(s), nil
		}
	case KindBytes:
		if s, ok := jsonString(data); ok {
			if b, err := base64.StdEncoding.DecodeString(s); err == nil {
				return Bytes(b), nil
			}
		}
	case KindUUID:
		if s, ok := jsonString(data); ok {
			return ParseUUID(s)
		}
	case KindDate:
		if s, ok := jsonString(data); ok {
			if year, month, day, rest, ok := parseDate(s); ok && rest == "" {
				return Date(year, month, day)
			}
		}
	case KindTime:
		if s, ok := jsonString(data); ok {
			return parseTimeOfDay(s)
		}
	case KindTimestamp:
		if s, ok := jsonString(data); ok {
			return parseTimestamp(s, 'T')
		}
	case KindInterval:
		var iv struct {
			Months *int32 `json:"months"`
			Days   *int32 `json:"days"`
			Nanos  *int64 `json:"nanos"`
		}
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&iv); err == nil && iv.Months != nil && iv.Days != nil && iv.Nanos != nil {
			return Interval(*iv.Months, *iv.Days, *iv.Nanos), nil
		}
	case KindArray:
		var raws []json.RawMessage
		if json.Unmarshal(data, &raws) != nil {
			break
		}
		var elem *Type
		if t != nil {
			elem = t.Elem
		}
		elems := make([]Value, len(raws))
		for i, raw := range raws {
			e, err := decodeTyped(elem, raw)
			if err != nil {
				return Value{}, fmt.Errorf("array element %d: %w", i, err)
			}
			elems[i] = e
		}
		return Value{kind: KindArray, elems: elems}, nil
	}
	return Value{}, errInvalid
}

// decodeSpecial decodes a tagged special value of kind k.
func decodeSpecial(k Kind, tag string, fields map[string]json.RawMessage) (Value, error) {
	var tz bool
	if raw, ok := fields["tz"]; ok && json.Unmarshal(raw, &tz) != nil {
		return Value{}, fmt.Errorf("value: invalid special %s encoding", k)
	}
	switch tag {
	case "nan":
		switch k {
		case KindFloat:
			return Float(math.NaN()), nil
		case KindDecimal:
			return DecimalNaN(), nil
		}
	case "+inf", "-inf":
		negative := tag == "-inf"
		switch k {
		case KindFloat:
			if negative {
				return Float(math.Inf(-1)), nil
			}
			return Float(math.Inf(1)), nil
		case KindDecimal:
			return DecimalInfinity(negative), nil
		case KindDate:
			return DateInfinity(negative), nil
		case KindTimestamp:
			return TimestampInfinity(negative, tz), nil
		}
	case "zero":
		if s, ok := jsonString(fields["v"]); ok && s != "" {
			switch k {
			case KindDate:
				return MySQLZeroDate(s), nil
			case KindTimestamp:
				return MySQLZeroTimestamp(s, tz), nil
			}
		}
	case "raw":
		if s, ok := jsonString(fields["v"]); ok && k == KindString {
			if b, err := base64.StdEncoding.DecodeString(s); err == nil {
				return String(string(b)), nil
			}
		}
	}
	return Value{}, fmt.Errorf("value: invalid special %s encoding", k)
}

// parseTag recognizes a tagged object {"$":"tag", …} and returns its tag and fields.
func parseTag(data []byte) (tag string, fields map[string]json.RawMessage, ok bool) {
	if len(data) == 0 || data[0] != '{' {
		return "", nil, false
	}
	if json.Unmarshal(data, &fields) != nil {
		return "", nil, false
	}
	raw, ok := fields["$"]
	if !ok || json.Unmarshal(raw, &tag) != nil {
		return "", nil, false
	}
	return tag, fields, true
}

func jsonString(data []byte) (string, bool) {
	var s string
	if len(data) == 0 || data[0] != '"' || json.Unmarshal(data, &s) != nil {
		return "", false
	}
	return s, true
}

// isJSONNumber rejects what strconv accepts but JSON does not, such as "+5" or "Inf".
func isJSONNumber(data []byte) bool {
	return len(data) > 0 && (data[0] == '-' || isDigit(data[0])) && json.Valid(data)
}
