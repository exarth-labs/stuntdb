// Package dialect is stuntdb's adapter SDK: the interfaces that every database
// engine implements, so that the rest of stuntdb never depends on a specific
// database.
//
// An adapter lives in dialects/<engine>/ and implements [Dialect]. Its
// package's init function calls [Register], and cmd/stuntdb imports each
// adapter package so that it registers.
//
// The core never writes SQL. It describes what it needs: a consistent read
// view ([Snapshot]), structured reads ([ReadQuery]), and a staged bulk load
// ([Loader]). Each adapter renders those in its engine's own dialect,
// handling its own syntax, parameter limits and bulk-load method. Engine
// differences that change what the core does are reported as [Capabilities];
// everything else stays inside the adapter.
//
// Rules every adapter follows:
//
//   - Source connections never write. They are opened read-only wherever the
//     engine allows it.
//   - Capabilities describe the server actually connected to, detected when
//     connecting, not assumed from the engine's name.
//   - Errors never contain row values or connection URLs, which can hold
//     passwords.
//
// The SDK is not stable yet. It becomes stable with stuntdb v1.0.
package dialect
