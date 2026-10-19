// Package value is stuntdb's engine-neutral model of column types and values.
//
// Every database adapter converts its native types and values into this
// model, and the rest of stuntdb works only with it. The rules are strict,
// because this is where database copy tools usually corrupt data:
//
//   - decimals never pass through floating point;
//   - unsigned 64-bit integers are supported;
//   - dates and timestamps keep their precision and time-zone meaning;
//   - engine-specific special values (infinity, NaN, MySQL zero dates) are kept;
//   - text that is not valid UTF-8 is kept byte for byte.
//
// A Value never prints its contents: String, Format and LogValue show only
// its kind, so row data cannot leak into logs or error messages by accident.
// Errors from this package never contain values either.
//
// # JSON encoding
//
// AppendJSON and DecodeJSON convert values to and from JSON. The column type
// keeps the encoding compact, because it says how to read each value:
//
//	Column type               JSON
//	bool                      true
//	int                       123 (always exact: readers must parse 64-bit integers)
//	float                     1.5
//	decimal                   "123.4500" (the exact text)
//	string, text, enum, json  "text"
//	binary, opaque            "base64…"
//	uuid                      "123e4567-e89b-12d3-a456-426614174000"
//	date                      "2026-10-19", "-0043-03-15", "+12026-01-01"
//	time                      "13:45:00.25", "13:45:00+05:30"
//	timestamp                 "2026-10-19T13:45:00.123456"   (no time zone)
//	                          "2026-10-19T08:45:00Z"         (an instant)
//	                          "2026-10-19T13:45:00+05:00"    (an instant with its original offset)
//	interval                  {"months":1,"days":-2,"nanos":3000}
//	array                     [1,null,3]
//	any (NULL)                null
//
// Values outside the normal range are tagged objects:
//
//	{"$":"nan"}  {"$":"+inf"}  {"$":"-inf"}    NaN and infinity ("tz":true on time-zone-aware timestamps)
//	{"$":"zero","v":"0000-00-00"}              a MySQL zero date, kept as its original text
//	{"$":"raw","v":"base64…"}                  text that is not valid UTF-8
//
// A value whose kind does not match its column type is tagged with its kind,
// for example text stored in an SQLite INTEGER column: {"$":"string","v":"abc"}.
package value
