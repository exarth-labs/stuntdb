# stuntdb

**A stunt double for your database.** stuntdb makes a small, safe copy of a production database. It replaces personal data with realistic fake values and keeps every relationship between tables intact, so developers and AI agents can work with data that looks and behaves like the real thing.

> **Status: early development.** stuntdb doesn't do anything useful yet.
>
> - The first release with real functionality will support **SQLite**.
> - **PostgreSQL**, **MySQL / MariaDB**, **SQL Server** and **Oracle** will follow, one per release.

## What it will do

- **Subset:** copy only a slice of a database, for example 5% of customers, while keeping every foreign key intact.
- **Mask:** replace names, emails, phone numbers and secrets with consistent fake values, where the same input always gets the same fake output.
- **Stay safe:** never write to the source database, and refuse to run while any column is still unreviewed.
- **Share:** write one `.stuntdb` snapshot file that can be restored on any developer's machine.

## Download

Early test builds are on the [Releases page](https://github.com/exarth-labs/stuntdb/releases) for Linux, macOS and Windows, on both x64 and ARM64. Download the archive for your system, unpack it, and run `stuntdb version`.

New downloads aren't code-signed by Apple or Microsoft yet, so the first launch may show a warning. On macOS, run `xattr -d com.apple.quarantine stuntdb` once.

### Checking a download is genuine

Every release has a signed `checksums.txt` and a build provenance record. With [cosign](https://docs.sigstore.dev/cosign/system_config/installation/) and the [GitHub CLI](https://cli.github.com):

```sh
# The checksums were signed by this repository's release workflow:
cosign verify-blob checksums.txt --bundle checksums.txt.sigstore.json \
  --certificate-identity-regexp '^https://github.com/exarth-labs/stuntdb/\.github/workflows/release\.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com

# Your archive matches the signed checksums:
sha256sum --check --ignore-missing checksums.txt

# The archive was built by GitHub Actions from this repository:
gh attestation verify stuntdb_*.tar.gz --repo exarth-labs/stuntdb
```

## Building from source

You need [Go](https://go.dev/dl/) 1.26 or newer and [Task](https://taskfile.dev).

```sh
git clone https://github.com/exarth-labs/stuntdb.git
cd stuntdb
task build            # creates bin/stuntdb (bin/stuntdb.exe on Windows)
./bin/stuntdb version
```

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md), and please follow our [Code of Conduct](CODE_OF_CONDUCT.md).

## Security

Please report security problems privately. See [SECURITY.md](SECURITY.md).

## License

[Apache License 2.0](LICENSE)
