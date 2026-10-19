# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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
