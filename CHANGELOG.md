# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- `stuntdb init` connects to the source database, reads its structure (never its rows), and writes a starter `stuntdb.yaml`. It suggests a masking rule for each column that needs one, as a comment, so that nothing is used until someone reviews it. It never replaces an existing file.
- `stuntdb check` decides how every column is masked and compares the decisions with `stuntdb.lock`. It fails with exit code 3 when a column has no decision, a rule is not allowed (`keep` on a secret, `null` in a `NOT NULL` column, a rule for a column that doesn't exist), or anything differs from the lockfile. `stuntdb check --update` writes the lockfile. Foreign keys are masked like the columns they reference. Both commands take `-c/--config` and `--json`.
- `stuntdb check` runs every masking rule through the masking engine, without the masking key:
  - unknown transformers (with a "did you mean"), bad options, column types a transformer doesn't take, weak transformers on unique columns, and columns too short for their values. A too-short column gets the exact `max_length` that fits every column of its domain, and when none fits, the transformers that do.
  - A foreign key takes its parent's whole rule (the transformer, its options and its domain), so both get the same fake values. It can't have a rule of its own, and a column whose parents are masked differently, or in different domains, is refused. In a cycle of foreign keys, one column takes the rule.
  - Domains are checked as a whole.
  - `per: <column>` can be written in `stuntdb.yaml`.
  - The lockfile records each column's options, domain and `per`, so a change to any of them shows up in review.
  - `stuntdb init` writes the `max_length` that fits into a suggestion that would be too long for its column.
- Column classification: built-in rule packs (`rules/core.yaml` for personal data, `rules/secrets.yaml` for passwords, keys and tokens) suggest what each column holds, and how to mask it, from its name and type alone. No data is read. Secrets are always replaced. Every suggestion is one the masking engine accepts for the column's type. `init` and `check` use it.
- The configuration format, `stuntdb.yaml` version 1, with a JSON Schema (`schemas/stuntdb.schema.json`) for autocomplete and validation in editors. Every mistake is reported at once, with its line and column. Secrets come only from environment variables or a key file. The lockfile, `stuntdb.lock`, records the decision for every column.
- Error messages and logs are redacted before they are shown: passwords, credentials in URLs and connection settings, and quoted values from database messages never reach the screen or a log.
- SQLite adapter: reads a SQLite database through a read-only connection and a consistent snapshot, and writes a copy into a new file that replaces the target only when it is complete. `init` and `check` read through it; copying is not yet available from the command line.
- The masking engine, with 21 transformers: names, user names, emails, phone numbers, addresses, companies, UUIDs, format-preserving encryption, keyed hashes, tokens, password hashes, redaction, date shifts, noise, filler text, constants, NULL, the column default and keeping a value. The same value always gets the same fake value under the same masking key, in every column of a domain, whatever the columns' lengths or types, so joins and foreign keys keep working. Values in unique columns stay distinct without any stored mapping. With `per:`, the columns seeded from one key change together, such as all of a person's dates by the same number of days. The columns of a domain are checked together before anything is masked: they must use one transformer with the same options, in column types it masks alike, and a domain that holds a unique column keeps all its values distinct. Fake emails use only `example.com`, `.net` and `.org`, and fake phone numbers only ranges reserved for fiction. NaN and infinite numbers stay as they are, as in standard (IEEE 754) arithmetic: they hold no personal data. A decimal column that can't hold whole numbers, such as `decimal(4,4)` or PostgreSQL's negative scales, is refused for the transformers that write them, and `noise` is refused where a decimal's scale is larger than its precision, such as `decimal(2,4)`, which holds less than 0.01. Fuzz tests check every transformer on any text, number, date and timestamp. It is not yet used from the command line.

### Changed

- Adapter SDK: each adapter reports how many key values one read can carry (`Capabilities.MaxKeyValues`), and stuntdb sends longer key lists in batches. Adapters no longer split a read, so its order and limit always hold.

## [0.0.1] - 2026-10-20

The first pre-release: the project's foundations. It doesn't copy databases yet.

### Added

- `stuntdb version`, with `--json` output, and documented exit codes.
- The engine-neutral model of column types, values and database structure, with a lossless JSON encoding.
- The adapter SDK that every database adapter will implement, and a conformance suite that every adapter must pass.
- Per-run workspaces that clean up after themselves, including after a crash.
- Release builds for Linux, macOS and Windows, on x64 and ARM64, with checksums, signatures, SBOMs and build provenance.

### Changed

- License changed from MIT to the Apache License 2.0.

[Unreleased]: https://github.com/exarth-labs/stuntdb/compare/v0.0.1...HEAD
[0.0.1]: https://github.com/exarth-labs/stuntdb/releases/tag/v0.0.1
