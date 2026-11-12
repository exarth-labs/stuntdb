// Package mask replaces column values with consistent fake values.
//
// Masking is deterministic: the same masking key, generator version, domain
// and input always give the same output, on every run, every engine and
// every Go release. Columns in the same domain map equal inputs to equal
// outputs, so that joins between them still work.
//
// # How a value is masked
//
// HKDF-SHA256 derives separate subkeys from the masking key (see Keys). For
// a seeded transformer, the seed of a value is
//
//	HMAC-SHA256(seed key, "stuntdb/mask/seed", generator version, domain, transformer, value)
//
// with every field length-prefixed, so that two different inputs never give
// the same bytes. The value is encoded by family, not by column type: the
// integer 5 and the decimal 5.00 are one number, as databases compare them,
// so both ends of a foreign key get the same output even when their declared
// types differ. The seed starts a ChaCha8 stream (Rand), which picks from
// versioned word lists. Transformers that must keep values distinct use a
// keyed permutation instead (NIST FF1, see FF1).
//
// # Domains
//
// A domain is a set of columns that give equal values equal fake values,
// such as a key and the foreign keys that reference it. ResolveDomains
// checks the domains of a whole copy before anything is masked: the value
// columns of a domain must use one transformer with the same options, in
// column types it masks alike (Transformer.MasksAlike), and a domain that
// holds a unique column makes all its value columns Distinct. Columns
// seeded with per: are the parts of one entity instead: the columns with
// the same per key, domain and transformer share their options, except
// options for the part each column holds, such as address's part.
//
// # Uniqueness
//
// A column in a primary key, a unique constraint or a unique index needs a
// transformer that never maps two inputs to one output (see Guarantee):
// Bijective transformers are permutations of the value's own format, and
// CollisionResistant ones carry at least 96 bits of a keyed hash of the
// input. Nothing keeps a list of values already used, because the result
// would then depend on the order in which rows arrive.
//
// # Safety
//
// Errors name the column and the transformer, never a value. NULL always
// stays NULL. Generated text is printable ASCII, and fake emails use only
// the reserved domains example.com, example.net and example.org.
package mask
