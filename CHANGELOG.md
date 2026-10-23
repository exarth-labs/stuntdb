# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Column classification: built-in rule packs (`rules/core.yaml` for personal data, `rules/secrets.yaml` for passwords, keys and tokens) suggest what each column holds, and how to mask it, from its name and type alone. No data is read. Secrets are always replaced. Commands start using it in the next releases.
- The configuration format, `stuntdb.yaml` version 1, with a JSON Schema (`schemas/stuntdb.schema.json`) for autocomplete and validation in editors. Every mistake is reported at once, with its line and column. Secrets come only from environment variables or a key file. The lockfile format, `stuntdb.lock`, is defined too. Commands start using both in the next releases.
- Error messages and logs are redacted before they are shown: passwords, credentials in URLs and connection settings, and quoted values from database messages never reach the screen or a log.
- SQLite adapter: reads a SQLite database through a read-only connection and a consistent snapshot, and writes a copy into a new file that replaces the target only when it is complete. Not yet available from the command line.

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
